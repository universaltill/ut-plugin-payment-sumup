package main

// SumUp settlement parsing for payment.sumup.reconcile (ut-docs#6579,
// ADR-0201, reference/uk-card-terminal-integrations-2026-11.md 5.3).
//
// Pure functions only, no host wiring: the event route, the settlement-line
// store and the day-close view belong to universal-till#6578. Untagged so
// `go test` covers them.
//
// Money: SumUp's payouts and history return amounts as JSON decimals in major
// units. They are parsed from their TEXT into integer minor units
// (decimalToMinor), never through float64 (1.15 as a float is
// 114.99999999999999), and an amount that is not exact to the penny is
// refused, never rounded.

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
	"time"
)

// decimalToMinor converts a decimal string ("12.50", "-0.01", "12") to
// integer minor units (2 decimals). ok is false for anything else: an
// exponent, more than two significant decimals, a bad shape, or overflow.
func decimalToMinor(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	neg := false
	switch {
	case strings.HasPrefix(s, "-"):
		neg, s = true, s[1:]
	case strings.HasPrefix(s, "+"):
		s = s[1:]
	}
	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" && !hasDot {
		return 0, false
	}
	if hasDot && frac == "" {
		return 0, false
	}
	for len(frac) > 2 && frac[len(frac)-1] == '0' {
		frac = frac[:len(frac)-1] // 12.500 is exact
	}
	if len(frac) > 2 {
		return 0, false
	}
	for len(frac) < 2 {
		frac += "0"
	}
	var n int64
	for _, r := range whole + frac {
		if r < '0' || r > '9' {
			return 0, false
		}
		if n > (1<<63-1-9)/10 {
			return 0, false
		}
		n = n*10 + int64(r-'0')
	}
	if neg {
		n = -n
	}
	return n, true
}

// rawMinor reads an amount field that may be a JSON number or a quoted
// decimal. Absent/null = 0, ok. Reading the raw text keeps 19.99 exact.
func rawMinor(raw json.RawMessage) (int64, bool) {
	t := strings.TrimSpace(string(raw))
	if t == "" || t == "null" {
		return 0, true
	}
	return decimalToMinor(strings.Trim(t, `"`))
}

// ---- output (the contract answer) ----

// settlementLine is one normalised settlement line (ref 5.3); money is signed
// integer minor units.
type settlementLine struct {
	ProviderLineID string `json:"provider_line_id"`
	TransactionRef string `json:"transaction_ref"`
	Kind           string `json:"kind"` // sale, refund, chargeback, adjustment
	Gross          int64  `json:"gross"`
	Fee            int64  `json:"fee"`
	Net            int64  `json:"net"`
	Tip            int64  `json:"tip"`
	Currency       string `json:"currency"`
	OccurredAt     string `json:"occurred_at"`
	SettledAt      string `json:"settled_at"`
	PayoutRef      string `json:"payout_ref"`
	PayoutDate     string `json:"payout_date"`
	PayoutStatus   string `json:"payout_status"` // paid, pending, failed
}

// settlementPayout totals the lines of one payout reference.
type settlementPayout struct {
	PayoutRef string `json:"payout_ref"`
	Date      string `json:"date"`
	Status    string `json:"status"`
	Currency  string `json:"currency"`
	Gross     int64  `json:"gross"`
	Fee       int64  `json:"fee"`
	Net       int64  `json:"net"`
}

// cannotMatch is a payout record that could not become a line. Never dropped.
type cannotMatch struct {
	ProviderLineID  string `json:"provider_line_id"`
	TransactionCode string `json:"transaction_code,omitempty"`
	PayoutRef       string `json:"payout_ref"`
	Reason          string `json:"reason"` // no_transaction_match, bad_amount
}

type reconcileResult struct {
	Lines       []settlementLine   `json:"lines"`
	Payouts     []settlementPayout `json:"payouts"`
	CannotMatch []cannotMatch      `json:"cannot_match"`
}

// ---- SumUp responses ----

// payoutRecord is one item of GET /v1.0/merchants/{code}/payouts. Amounts stay
// raw text until buildReconcile.
type payoutRecord struct {
	ID              string          `json:"-"`
	RawID           json.RawMessage `json:"id"`
	Type            string          `json:"type"`
	Amount          json.RawMessage `json:"amount"`
	Fee             json.RawMessage `json:"fee"`
	Currency        string          `json:"currency"`
	Date            string          `json:"date"`
	Status          string          `json:"status"`
	Reference       string          `json:"reference"`
	TransactionCode string          `json:"transaction_code"`
}

// historyEntry is one item of GET /v2.1/merchants/{code}/transactions/history.
// ID is what the reader flow's .authorize returned (stored by core as
// transaction_ref); TransactionCode is what the payouts name.
type historyEntry struct {
	ID              string          `json:"id"`
	TransactionCode string          `json:"transaction_code"`
	Amount          json.RawMessage `json:"amount"`
	TipAmount       json.RawMessage `json:"tip_amount"`
	Currency        string          `json:"currency"`
	Timestamp       string          `json:"timestamp"`
	Status          string          `json:"status"`
}

// reconcileStatusCode maps a non-2xx answer to a fixed code ("" for 2xx).
// 403 is scope_missing: the key lacks payouts.read (or the history scope), and
// must never look like an empty settlement.
func reconcileStatusCode(status int) string {
	switch {
	case status >= 200 && status < 300:
		return ""
	case status == 403:
		return "scope_missing"
	case status == 401:
		return "auth_failed"
	case status == 404:
		return "merchant_not_found"
	case status == 429:
		return "rate_limited"
	case status >= 500:
		return "provider_unavailable"
	}
	return "reconcile_failed"
}

// items returns the list in a bare array or an {"items":[...]} wrapper.
func items(body []byte) (json.RawMessage, bool) {
	b := bytes.TrimSpace(body)
	if len(b) > 0 && b[0] == '[' {
		return b, true
	}
	var w struct {
		Items json.RawMessage `json:"items"`
	}
	if json.Unmarshal(b, &w) != nil || len(w.Items) == 0 || w.Items[0] != '[' {
		return nil, false
	}
	return w.Items, true
}

// parsePayouts decodes one payouts response. A non-2xx status or an
// undecodable body returns a code and no records.
func parsePayouts(body []byte, status int) ([]payoutRecord, string) {
	if c := reconcileStatusCode(status); c != "" {
		return nil, c
	}
	raw, ok := items(body)
	if !ok {
		return nil, "bad_response"
	}
	var out []payoutRecord
	if json.Unmarshal(raw, &out) != nil {
		return nil, "bad_response"
	}
	for i := range out {
		out[i].ID = strings.Trim(strings.TrimSpace(string(out[i].RawID)), `"`)
	}
	return out, ""
}

// parseHistoryPage decodes one history page; next is the href of the "next"
// link ("" = last page).
func parseHistoryPage(body []byte, status int) (entries []historyEntry, next, code string) {
	if c := reconcileStatusCode(status); c != "" {
		return nil, "", c
	}
	raw, ok := items(body)
	if !ok {
		return nil, "", "bad_response"
	}
	if json.Unmarshal(raw, &entries) != nil {
		return nil, "", "bad_response"
	}
	var w struct {
		Links []struct {
			Rel  string `json:"rel"`
			Href string `json:"href"`
		} `json:"links"`
	}
	_ = json.Unmarshal(body, &w)
	for _, l := range w.Links {
		if l.Rel == "next" {
			next = l.Href
		}
	}
	return entries, next, ""
}

// payoutsPath is the payouts list for a date range (YYYY-MM-DD, inclusive).
func payoutsPath(merchantCode, from, to string) string {
	q := url.Values{"start_date": {from}, "end_date": {to}}
	return "/v1.0/merchants/" + url.PathEscape(merchantCode) + "/payouts?" + q.Encode()
}

// historyPath is the first page of successful transactions between two RFC
// 3339 instants. A sale settles days after it happens, so the caller picks a
// window that starts before the payout range.
func historyPath(merchantCode, oldest, newest string) string {
	q := url.Values{"oldest_time": {oldest}, "newest_time": {newest}, "statuses[]": {"SUCCESSFUL"}}
	return "/v2.1/merchants/" + url.PathEscape(merchantCode) + "/transactions/history?" + q.Encode()
}

// ---- joining ----

func utcInstant(s string) string {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format("2006-01-02T15:04:05Z")
		}
	}
	return ""
}

func dateOnly(s string) string {
	if t := utcInstant(s); t != "" {
		return t[:10]
	}
	return ""
}

func payoutStatus(s string) string {
	switch strings.ToUpper(s) {
	case "SUCCESSFUL", "PAID":
		return "paid"
	case "FAILED":
		return "failed"
	case "PENDING":
		return "pending"
	}
	return "" // unknown or missing: never read as pending, the record goes to cannot_match
}

func lineKind(typ string) (kind string, deduction bool) {
	switch strings.ToUpper(typ) {
	case "PAYOUT":
		return "sale", false
	case "REFUND_DEDUCTION":
		return "refund", true
	case "CHARGE_BACK_DEDUCTION":
		return "chargeback", true
	}
	return "adjustment", true // DD_RETURN_DEDUCTION, BALANCE_DEDUCTION, unknown
}

// buildReconcile joins payout records to history on transaction_code and
// returns the contract answer. transaction_ref of a line is the history id
// (what .authorize returned); a record with a transaction_code that history
// does not know goes to cannot_match, and one with an amount that is not
// exact to the penny goes there as bad_amount. A deduction is always negative
// whatever sign SumUp sent. Payout totals include cannot_match records that
// parsed, so the totals still equal what SumUp paid.
func buildReconcile(payouts []payoutRecord, history []historyEntry) reconcileResult {
	byCode := map[string]historyEntry{}
	for _, h := range history {
		if h.TransactionCode != "" {
			byCode[h.TransactionCode] = h
		}
	}
	res := reconcileResult{Lines: []settlementLine{}, Payouts: []settlementPayout{}, CannotMatch: []cannotMatch{}}
	totals := map[string]*settlementPayout{}
	var order []string
	for _, p := range payouts {
		ref := firstNonEmpty(p.Reference, p.ID)
		gross, ok1 := rawMinor(p.Amount)
		fee, ok2 := rawMinor(p.Fee)
		if !ok1 || !ok2 {
			res.CannotMatch = append(res.CannotMatch, cannotMatch{p.ID, p.TransactionCode, ref, "bad_amount"})
			continue
		}
		kind, deduction := lineKind(p.Type)
		if deduction {
			if gross > 0 {
				gross = -gross
			}
			if fee < 0 {
				fee = -fee
			}
		}
		net := gross - fee
		cur := strings.ToUpper(p.Currency)
		status := payoutStatus(p.Status)
		if status == "" || cur == "" {
			res.CannotMatch = append(res.CannotMatch, cannotMatch{p.ID, p.TransactionCode, ref, "bad_status"})
			continue
		}

		t := totals[ref]
		if t == nil {
			t = &settlementPayout{PayoutRef: ref, Date: dateOnly(p.Date), Status: status, Currency: cur}
			totals[ref] = t
			order = append(order, ref)
		}
		t.Gross += gross
		t.Fee += fee
		t.Net += net
		if status == "failed" || (status == "pending" && t.Status == "paid") {
			t.Status = status // one open record keeps the whole payout open; failed wins
		}

		line := settlementLine{
			ProviderLineID: p.ID, Kind: kind, Gross: gross, Fee: fee, Net: net, Currency: cur,
			SettledAt: utcInstant(p.Date), PayoutRef: ref, PayoutDate: dateOnly(p.Date), PayoutStatus: status,
		}
		if p.TransactionCode != "" || kind != "adjustment" {
			// Only an adjustment (balance deduction) may lack a transaction_code;
			// a sale/refund/chargeback line with no reference is never silently kept.
			h, found := byCode[p.TransactionCode]
			if !found {
				res.CannotMatch = append(res.CannotMatch, cannotMatch{p.ID, p.TransactionCode, ref, "no_transaction_match"})
				continue
			}
			line.TransactionRef = h.ID
			line.OccurredAt = utcInstant(h.Timestamp)
			if kind == "sale" {
				if tip, ok := rawMinor(h.TipAmount); ok {
					line.Tip = tip
				}
			}
		}
		res.Lines = append(res.Lines, line)
	}
	for _, ref := range order {
		res.Payouts = append(res.Payouts, *totals[ref])
	}
	return res
}
