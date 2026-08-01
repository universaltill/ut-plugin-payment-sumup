package main

import (
	"encoding/json"
	"testing"
)

// majorToMinor is what turns a SumUp transaction's reader-captured tip
// (assumed major-unit decimal, per pollTransaction's doc comment — NEEDS
// SANDBOX VERIFICATION) into the integer minor units the till's core domain
// model and /api/pos/tender's `tip` field use everywhere else. Getting this
// wrong silently mis-reports every tip by 100x, so it's worth pinning down
// with real numbers rather than trusting the formula by inspection.
func TestMajorToMinor(t *testing.T) {
	cases := []struct {
		major float64
		want  int64
	}{
		{0, 0},
		{1.50, 150},
		{10.00, 1000},
		{0.01, 1},
		{2.999, 300}, // rounds to the nearest minor unit, not truncates
	}
	for _, c := range cases {
		if got := majorToMinor(c.major); got != c.want {
			t.Errorf("majorToMinor(%v) = %d, want %d", c.major, got, c.want)
		}
	}
}

// This is the exact bug an independent review found before this fix
// shipped: decoding tip_amount as a fixed *float64 field made the WHOLE
// transaction-poll parse fail whenever tip_amount had any other shape
// (an object, a string, ...) — silently discarding a real SUCCESSFUL
// transaction's id/status too, which would have declined a payment the
// customer's card had already been successfully charged for. A tip_amount
// shaped like an object is not hypothetical: this same file's own
// reader-checkout request sends total_amount as exactly that shape
// ({"currency":...,"minor_unit":2,"value":...}) 40-ish lines above
// pollTransaction — at least as plausible a guess for tip_amount as a bare
// decimal. parseTransactionPoll must find the transaction and its outcome
// regardless of what shape tip_amount turns out to have.
func TestParseTransactionPoll_WeirdTipShapeNeverBreaksOutcomeDetection(t *testing.T) {
	cases := []struct {
		name           string
		tipAmountField string // raw JSON for the tip_amount field, or "" to omit it
		wantTip        int64
	}{
		{"absent", "", 0},
		{"bare major decimal (assumed real shape)", `"tip_amount":1.50,`, 150},
		{"object shape, same as total_amount elsewhere in this file", `"tip_amount":{"currency":"EUR","minor_unit":2,"value":150},`, 0},
		{"string shape", `"tip_amount":"1.50",`, 0},
		{"null", `"tip_amount":null,`, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := []byte(`{"id":"txn-abc123","status":"SUCCESSFUL",` + c.tipAmountField + `"currency":"EUR"}`)
			id, status, tip, found := parseTransactionPoll(body)
			if !found {
				t.Fatalf("expected the transaction to be found regardless of tip_amount's shape, got found=false")
			}
			if id != "txn-abc123" || status != "SUCCESSFUL" {
				t.Fatalf("expected id/status to survive an unrecognized tip_amount shape, got id=%q status=%q", id, status)
			}
			if tip != c.wantTip {
				t.Fatalf("want tip %d, got %d", c.wantTip, tip)
			}
		})
	}

	// The "items" wrapper and bare-array envelope shapes must be just as
	// robust — not just the single-object shape exercised above.
	wrapped := []byte(`{"items":[{"id":"txn-xyz","status":"SUCCESSFUL","tip_amount":{"weird":true}}]}`)
	if id, status, tip, found := parseTransactionPoll(wrapped); !found || id != "txn-xyz" || status != "SUCCESSFUL" || tip != 0 {
		t.Fatalf("items-wrapper envelope: got id=%q status=%q tip=%d found=%v", id, status, tip, found)
	}
	bareArray := []byte(`[{"id":"txn-bare","status":"FAILED","tip_amount":"not a number"}]`)
	if id, status, tip, found := parseTransactionPoll(bareArray); !found || id != "txn-bare" || status != "FAILED" || tip != 0 {
		t.Fatalf("bare-array envelope: got id=%q status=%q tip=%d found=%v", id, status, tip, found)
	}

	// A non-terminal status (still processing) must NOT be reported as
	// found — the caller needs to keep polling, same as before this change.
	if _, _, _, found := parseTransactionPoll([]byte(`{"id":"txn-pending","status":"PENDING"}`)); found {
		t.Fatal("a non-terminal status must not be reported as found")
	}
}

// parseTipAmountMinor must degrade to "no tip" (0) for every shape it
// doesn't recognize rather than erroring — its whole reason to exist,
// per pollTransaction's doc comment, is that tip_amount's real shape is an
// UNVERIFIED guess and a wrong guess must never be allowed to break parsing
// of the transaction it rode in on (that used to be possible when
// tip_amount was decoded as a fixed *float64 field directly on the txn
// struct: an object or a string there failed the WHOLE json.Unmarshal,
// discarding a real SUCCESSFUL transaction's id/status too).
func TestParseTipAmountMinor(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int64
	}{
		{"absent", "", 0},
		{"null", "null", 0},
		{"zero", "0", 0},
		{"negative", "-1.5", 0},
		{"positive major decimal", "1.50", 150},
		{"whole number", "10", 1000},
		// The exact shapes this repo's own reader-checkout request uses
		// elsewhere in this file for total_amount — a very real possibility
		// for how SumUp might also shape tip_amount, and exactly the case
		// that broke outcome detection entirely before this fix.
		{"object shape", `{"currency":"EUR","minor_unit":2,"value":150}`, 0},
		{"string shape", `"1.50"`, 0},
		{"malformed", `not json`, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var raw json.RawMessage
			if c.raw != "" {
				raw = json.RawMessage(c.raw)
			}
			if got := parseTipAmountMinor(raw); got != c.want {
				t.Errorf("parseTipAmountMinor(%s) = %d, want %d", c.raw, got, c.want)
			}
		})
	}
}

// minorToMajor and majorToMinor must round-trip for any amount the till can
// actually produce (integer minor units) — this is the exact property the
// Refund flow (minorToMajor) and the new tip read-back (majorToMinor) both
// depend on for the same amount to survive a round trip through SumUp's API.
func TestMinorMajorRoundTrip(t *testing.T) {
	for _, minor := range []int64{0, 1, 50, 99, 100, 1050, 999999} {
		if got := majorToMinor(minorToMajor(minor)); got != minor {
			t.Errorf("round trip broke for %d minor: got %d via major %v", minor, got, minorToMajor(minor))
		}
	}
}
