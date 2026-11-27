package main

import (
	"encoding/json"
	"fmt"
	"testing"
)

// ut-docs#6579: SumUp settlement parsing for payment.sumup.reconcile. Pure
// functions only (no host wiring yet, #6578). Decimals are parsed from their
// text, never through float64: 1.15 as a float is 114.99999999999999 minor.

func TestDecimalToMinorExact(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"12.50", 1250, true}, {"12.5", 1250, true}, {"12", 1200, true},
		{"0.19", 19, true}, {"19.99", 1999, true}, {"0.01", 1, true},
		{"1.15", 115, true}, {"0.07", 7, true}, {"4.35", 435, true},
		{"-5.00", -500, true}, {"-0.01", -1, true}, {"+3.2", 320, true},
		{"12.500", 1250, true}, // trailing zeros are exact
		{"0.015", 0, false},    // half a penny: refuse, never round
		{"1e2", 0, false}, {"", 0, false}, {"abc", 0, false}, {"1.2.3", 0, false},
		{"-", 0, false}, {".5", 50, true}, {"5.", 0, false},
		{"92233720368547758.07", 0, false}, // overflow
	}
	for _, c := range cases {
		got, ok := decimalToMinor(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("decimalToMinor(%q) = %d,%v want %d,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// Fixtures follow SumUp's payouts schema (amount/fee floats in major units,
// type PAYOUT or *_DEDUCTION, transaction_code of the original sale) and the
// transactions/history schema (id, transaction_code, amount, tip_amount).
// Not yet checked against a sandbox merchant (#6579 identifier proof).
const payoutsFixture = `[
 {"id":9001,"type":"PAYOUT","amount":12.50,"fee":0.19,"currency":"GBP","date":"2026-11-20","status":"SUCCESSFUL","reference":"PO-1","transaction_code":"TCODE-A"},
 {"id":9002,"type":"PAYOUT","amount":19.99,"fee":0.30,"currency":"GBP","date":"2026-11-20","status":"SUCCESSFUL","reference":"PO-1","transaction_code":"TCODE-B"},
 {"id":9003,"type":"REFUND_DEDUCTION","amount":-5.00,"fee":0,"currency":"GBP","date":"2026-11-20","status":"SUCCESSFUL","reference":"PO-1","transaction_code":"TCODE-A"},
 {"id":9004,"type":"CHARGE_BACK_DEDUCTION","amount":3.20,"fee":0,"currency":"GBP","date":"2026-11-20","status":"SUCCESSFUL","reference":"PO-1","transaction_code":"TCODE-B"},
 {"id":9005,"type":"BALANCE_DEDUCTION","amount":-0.01,"fee":0,"currency":"GBP","date":"2026-11-20","status":"SUCCESSFUL","reference":"PO-1"},
 {"id":9006,"type":"PAYOUT","amount":7.00,"fee":0.10,"currency":"GBP","date":"2026-11-22","status":"PENDING","reference":"PO-2","transaction_code":"TCODE-NOHIST"}
]`

const historyFixture = `{"items":[
 {"id":"sumup-id-A","transaction_code":"TCODE-A","amount":12.50,"tip_amount":1.00,"currency":"GBP","timestamp":"2026-11-19T14:03:05.123Z","status":"SUCCESSFUL"},
 {"id":"sumup-id-B","transaction_code":"TCODE-B","amount":19.99,"currency":"GBP","timestamp":"2026-11-19T15:00:00Z","status":"SUCCESSFUL"}
],"links":[{"rel":"next","href":"transactions/history?limit=2&oldest_ref=xyz"}]}`

func mustReconcile(t *testing.T) reconcileResult {
	t.Helper()
	pays, code := parsePayouts([]byte(payoutsFixture), 200)
	if code != "" {
		t.Fatalf("parsePayouts: %s", code)
	}
	hist, next, code := parseHistoryPage([]byte(historyFixture), 200)
	if code != "" {
		t.Fatalf("parseHistoryPage: %s", code)
	}
	if next != "transactions/history?limit=2&oldest_ref=xyz" {
		t.Fatalf("next = %q", next)
	}
	return buildReconcile(pays, hist)
}

func TestReconcileSaleLineAmounts(t *testing.T) {
	r := mustReconcile(t)
	var a settlementLine
	for _, l := range r.Lines {
		if l.TransactionRef == "sumup-id-A" && l.Kind == "sale" {
			a = l
		}
	}
	// amount "12.50" fee "0.19" -> gross 1250, fee 19, net 1231; tip 1.00 -> 100.
	if a.Gross != 1250 || a.Fee != 19 || a.Net != 1231 || a.Tip != 100 || a.Currency != "GBP" {
		t.Fatalf("sale line = %+v", a)
	}
	// Joined on transaction_code, but transaction_ref is the id .authorize returned (history id).
	if a.PayoutRef != "PO-1" || a.PayoutDate != "2026-11-20" || a.PayoutStatus != "paid" {
		t.Fatalf("payout fields = %+v", a)
	}
	if a.OccurredAt != "2026-11-19T14:03:05Z" || a.SettledAt != "2026-11-20T00:00:00Z" {
		t.Fatalf("times = %q %q", a.OccurredAt, a.SettledAt)
	}
}

func TestReconcileDeductionKindsAndSigns(t *testing.T) {
	r := mustReconcile(t)
	want := map[string]struct {
		kind       string
		ref        string
		gross, net int64
	}{
		"9003": {"refund", "sumup-id-A", -500, -500},
		"9004": {"chargeback", "sumup-id-B", -320, -320}, // a deduction is always negative, even if SumUp sent it unsigned
		"9005": {"adjustment", "", -1, -1},
	}
	got := map[string]settlementLine{}
	for _, l := range r.Lines {
		got[l.ProviderLineID] = l
	}
	for id, w := range want {
		l, ok := got[id]
		if !ok || l.Kind != w.kind || l.TransactionRef != w.ref || l.Gross != w.gross || l.Net != w.net || l.Fee != 0 {
			t.Errorf("line %s = %+v (found %v), want %+v", id, l, ok, w)
		}
	}
}

func TestReconcilePayoutNetSumsLines(t *testing.T) {
	r := mustReconcile(t)
	var po1 *settlementPayout
	for i := range r.Payouts {
		if r.Payouts[i].PayoutRef == "PO-1" {
			po1 = &r.Payouts[i]
		}
	}
	if po1 == nil {
		t.Fatalf("payouts = %+v", r.Payouts)
	}
	// 1250-19 + 1999-30 - 500 - 320 - 1 = 2379 ; gross 1250+1999-500-320-1 = 2428 ; fee 49
	if po1.Gross != 2428 || po1.Fee != 49 || po1.Net != 2379 || po1.Status != "paid" || po1.Date != "2026-11-20" {
		t.Fatalf("PO-1 = %+v", *po1)
	}
	var sum int64
	for _, l := range r.Lines {
		if l.PayoutRef == "PO-1" {
			sum += l.Net
		}
	}
	if sum != po1.Net {
		t.Fatalf("lines net %d != payout net %d", sum, po1.Net)
	}
}

// A payout record whose transaction_code is not in the history pages is never
// dropped and never given a made-up transaction_ref.
func TestReconcileUnmatchedGoesToCannotMatch(t *testing.T) {
	r := mustReconcile(t)
	if len(r.CannotMatch) != 1 {
		t.Fatalf("cannot_match = %+v", r.CannotMatch)
	}
	c := r.CannotMatch[0]
	if c.ProviderLineID != "9006" || c.TransactionCode != "TCODE-NOHIST" || c.Reason != "no_transaction_match" || c.PayoutRef != "PO-2" {
		t.Fatalf("cannot_match[0] = %+v", c)
	}
	for _, l := range r.Lines {
		if l.TransactionRef == "" && l.Kind != "adjustment" {
			t.Fatalf("line without a transaction_ref: %+v", l)
		}
	}
	if len(r.Lines) != 5 {
		t.Fatalf("lines = %d, want 5", len(r.Lines))
	}
	var po2 *settlementPayout
	for i := range r.Payouts {
		if r.Payouts[i].PayoutRef == "PO-2" {
			po2 = &r.Payouts[i]
		}
	}
	if po2 == nil || po2.Status != "pending" || po2.Net != 690 {
		t.Fatalf("PO-2 = %+v (the unmatched record still counts in its payout)", po2)
	}
}

func TestParsePayoutsBadAmountIsCannotMatchNotZero(t *testing.T) {
	body := `[{"id":1,"type":"PAYOUT","amount":"12.505","fee":0.1,"currency":"GBP","date":"2026-11-20","status":"SUCCESSFUL","reference":"P","transaction_code":"T"}]`
	pays, code := parsePayouts([]byte(body), 200)
	if code != "" || len(pays) != 1 {
		t.Fatalf("%v %q", pays, code)
	}
	hist := []historyEntry{{ID: "i", TransactionCode: "T"}}
	r := buildReconcile(pays, hist)
	if len(r.Lines) != 0 || len(r.CannotMatch) != 1 || r.CannotMatch[0].Reason != "bad_amount" {
		t.Fatalf("result = %+v", r)
	}
}

func TestParsePayoutsStringAmountsAndWrapper(t *testing.T) {
	body := `{"items":[{"id":1,"type":"PAYOUT","amount":"12.50","fee":"0.19","currency":"gbp","date":"2026-11-20T09:00:00Z","status":"SUCCESSFUL","reference":"P","transaction_code":"T"}]}`
	pays, code := parsePayouts([]byte(body), 200)
	if code != "" || len(pays) != 1 {
		t.Fatalf("%v %q", pays, code)
	}
	r := buildReconcile(pays, []historyEntry{{ID: "i", TransactionCode: "T"}})
	if len(r.Lines) != 1 || r.Lines[0].Gross != 1250 || r.Lines[0].Fee != 19 || r.Lines[0].Currency != "GBP" || r.Lines[0].PayoutDate != "2026-11-20" {
		t.Fatalf("lines = %+v", r.Lines)
	}
}

// A missing payouts.read scope is a clear error code, never an empty success.
func TestReconcileStatusCodes(t *testing.T) {
	for _, c := range []struct {
		status int
		want   string
	}{
		{403, "scope_missing"}, {401, "auth_failed"}, {404, "merchant_not_found"},
		{429, "rate_limited"}, {500, "provider_unavailable"}, {400, "reconcile_failed"},
	} {
		pays, code := parsePayouts([]byte(`{"message":"insufficient scope"}`), c.status)
		if code != c.want || pays != nil {
			t.Errorf("status %d -> %v %q, want %q", c.status, pays, code, c.want)
		}
		_, _, code = parseHistoryPage([]byte(`{}`), c.status)
		if code != c.want {
			t.Errorf("history status %d -> %q, want %q", c.status, code, c.want)
		}
	}
	if _, code := parsePayouts([]byte(`not json`), 200); code != "bad_response" {
		t.Errorf("garbage 200 = %q, want bad_response", code)
	}
}

func TestReconcilePaths(t *testing.T) {
	if got := payoutsPath("MC1", "2026-11-01", "2026-11-30"); got != "/v1.0/merchants/MC1/payouts?end_date=2026-11-30&start_date=2026-11-01" {
		t.Fatalf("payouts path = %s", got)
	}
	if got := payoutsPath("M/1", "2026-11-01", "2026-11-30"); got != "/v1.0/merchants/M%2F1/payouts?end_date=2026-11-30&start_date=2026-11-01" {
		t.Fatalf("escaped = %s", got)
	}
	if got := historyPath("MC1", "2026-10-31T00:00:00Z", "2026-11-30T23:59:59Z"); got != "/v2.1/merchants/MC1/transactions/history?newest_time=2026-11-30T23%3A59%3A59Z&oldest_time=2026-10-31T00%3A00%3A00Z&statuses%5B%5D=SUCCESSFUL" {
		t.Fatalf("history path = %s", got)
	}
}

func TestHistoryNextLinkAbsentAndPaging(t *testing.T) {
	p1 := `{"items":[{"id":"a","transaction_code":"TA"}],"links":[{"rel":"next","href":"transactions/history?oldest_ref=1"}]}`
	p2 := `{"items":[{"id":"b","transaction_code":"TB"}],"links":[{"rel":"prev","href":"x"}]}`
	h1, n1, c1 := parseHistoryPage([]byte(p1), 200)
	h2, n2, c2 := parseHistoryPage([]byte(p2), 200)
	if c1 != "" || c2 != "" || n1 == "" || n2 != "" || fmt.Sprint(len(h1), len(h2)) != "1 1" {
		t.Fatalf("%v %q %q | %v %q %q", h1, n1, c1, h2, n2, c2)
	}
	// payouts pages merge: lines from two pages of payouts join one history set.
	pays := append(mustPayouts(t, `[{"id":1,"type":"PAYOUT","amount":1.00,"fee":0,"currency":"GBP","date":"2026-11-20","status":"SUCCESSFUL","reference":"P","transaction_code":"TA"}]`),
		mustPayouts(t, `[{"id":2,"type":"PAYOUT","amount":2.00,"fee":0,"currency":"GBP","date":"2026-11-20","status":"SUCCESSFUL","reference":"P","transaction_code":"TB"}]`)...)
	r := buildReconcile(pays, append(h1, h2...))
	if len(r.Lines) != 2 || len(r.CannotMatch) != 0 || r.Payouts[0].Net != 300 {
		t.Fatalf("%+v", r)
	}
}

func mustPayouts(t *testing.T, body string) []payoutRecord {
	t.Helper()
	p, code := parsePayouts([]byte(body), 200)
	if code != "" {
		t.Fatal(code)
	}
	return p
}

// The answer is the contract JSON: lines/payouts/cannot_match always present.
func TestReconcileResultJSONShape(t *testing.T) {
	b, _ := json.Marshal(buildReconcile(nil, nil))
	if string(b) != `{"lines":[],"payouts":[],"cannot_match":[]}` {
		t.Fatalf("empty = %s", b)
	}
	b, _ = json.Marshal(mustReconcile(t).Lines[0])
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"transaction_ref", "kind", "gross", "fee", "net", "tip", "currency", "occurred_at", "settled_at", "payout_ref", "payout_date", "payout_status"} {
		if _, ok := m[k]; !ok {
			t.Errorf("line JSON lacks %q: %s", k, b)
		}
	}
}

// Reviewer (payments-batch): unknown status and a sale with no reference fail closed.
func TestReconcileUnknownStatusAndMissingCodeGoToCannotMatch(t *testing.T) {
	res := buildReconcile([]payoutRecord{
		{ID: "1", Type: "PAYOUT", Amount: json.RawMessage(`10.00`), Fee: json.RawMessage(`0`), Currency: "GBP", Date: "2026-11-20", Status: "", Reference: "PO-1", TransactionCode: "T1"},
		{ID: "2", Type: "PAYOUT", Amount: json.RawMessage(`10.00`), Fee: json.RawMessage(`0`), Currency: "GBP", Date: "2026-11-20", Status: "SUCCESSFUL", Reference: "PO-2"},
	}, []historyEntry{{ID: "h1", TransactionCode: "T1"}})
	if len(res.Lines) != 0 || len(res.CannotMatch) != 2 || res.CannotMatch[0].Reason != "bad_status" || res.CannotMatch[1].Reason != "no_transaction_match" {
		t.Fatalf("%+v", res)
	}
}
