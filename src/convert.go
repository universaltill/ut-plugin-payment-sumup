// No //go:build tag: unlike main.go, this file has no dependency on the
// wasip1-only //go:wasmimport host functions, so it compiles and is
// testable under a plain `go test` (host GOOS/GOARCH) as well as the
// wasip1 build — this plugin's only real unit-testable logic lives here
// for exactly that reason.
package main

import (
	"encoding/json"
	"math"
)

// minorToMajor renders integer minor units as a decimal major-unit number
// for SumUp's Checkouts/Refund APIs (unlike the Reader Cloud API, these use
// major units — e.g. 1050 minor -> 10.50). Assumes a 2-decimal currency
// (true for EUR/GBP/USD, the only currencies this plugin targets today); a
// 0-decimal currency (CLP/COP/HUF) would need a currency-aware divisor if
// this plugin ever needs to support one.
func minorToMajor(amount int64) float64 {
	return float64(amount) / 100.0
}

// majorToMinor is minorToMajor's inverse — used to convert a decimal
// major-unit amount FROM a SumUp API response (e.g. a transaction's
// tip_amount) back to the integer minor units the till's core domain model
// uses everywhere else. Same 2-decimal-currency assumption as minorToMajor.
func majorToMinor(major float64) int64 {
	return int64(math.Round(major * 100))
}

// transactionPollEntry is one item of the Transactions list endpoint's
// response, as decoded by parseTransactionPoll — kept in this tag-free file
// (not main.go) specifically so the decode logic around it is real-Go-
// testable. TipAmount is deliberately json.RawMessage: see
// parseTipAmountMinor's doc comment for why a fixed, failable Go type there
// would be dangerous.
type transactionPollEntry struct {
	ID        string          `json:"id"`
	Status    string          `json:"status"`
	TipAmount json.RawMessage `json:"tip_amount"`
}

// parseTransactionPoll decodes one Transactions-list poll response body
// into a terminal outcome, mirroring pollTransaction's own doc comment: the
// list envelope shape (bare array vs. an "items" wrapper vs. a single bare
// object) wasn't confirmed against a live sandbox, so all three are tried.
// found is false when nothing decodable, or nothing in a terminal state,
// was in this particular response — pollTransaction's caller keeps polling
// in that case, exactly as before this function was extracted.
func parseTransactionPoll(body []byte) (id, status string, tipAmount int64, found bool) {
	var wrapped struct {
		Items []transactionPollEntry `json:"items"`
	}
	var bare []transactionPollEntry
	var one transactionPollEntry
	var entry *transactionPollEntry
	if err := json.Unmarshal(body, &wrapped); err == nil && len(wrapped.Items) > 0 {
		entry = &wrapped.Items[0]
	} else if err := json.Unmarshal(body, &bare); err == nil && len(bare) > 0 {
		entry = &bare[0]
	} else if err := json.Unmarshal(body, &one); err == nil && one.ID != "" {
		entry = &one
	}
	if entry == nil {
		return "", "", 0, false
	}
	switch entry.Status {
	case "SUCCESSFUL", "FAILED", "CANCELLED":
		return entry.ID, entry.Status, parseTipAmountMinor(entry.TipAmount), true
	default:
		return "", "", 0, false
	}
}

// parseTipAmountMinor best-effort-interprets a Transactions API response's
// raw tip_amount field (whatever shape it turns out to have — see
// pollTransaction's doc comment, UNVERIFIED against a live account) as
// integer minor units. Deliberately never errors: absent, null, zero,
// negative, an object, a string, or any other shape this doesn't recognize
// all mean "no tip reported" (0) — a wrong guess about this field's shape
// must only ever cost a missed tip, never corrupt one or (by being decoded
// via a fixed, failable Go type upstream, which this function's caller
// specifically avoids) break parsing of the surrounding transaction the
// tip rode in on.
func parseTipAmountMinor(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var major float64
	if err := json.Unmarshal(raw, &major); err != nil || major <= 0 {
		return 0
	}
	return majorToMinor(major)
}
