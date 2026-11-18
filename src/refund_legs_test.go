package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// legFake wraps ledgerSumUp (ut-docs#6603): per transaction it can refuse
// without applying (failNet: transport failure, nothing refunded) or apply and
// lose the reply (dropReply, N times).
type legFake struct {
	*ledgerSumUp
	failNet   map[string]bool
	dropReply map[string]int
	calls     []string // txn id per call
}

func newLegFake(captured map[string]int64) *legFake {
	return &legFake{ledgerSumUp: newLedger(0, captured), failNet: map[string]bool{}, dropReply: map[string]int{}}
}

func (f *legFake) call(method, path string, body []byte) ([]byte, int, bool) {
	txn := strings.TrimPrefix(path, "/v0.1/me/refund/")
	f.calls = append(f.calls, txn)
	if f.failNet[txn] {
		return nil, 0, false
	}
	b, st, ok := f.ledgerSumUp.call(method, path, body)
	if f.dropReply[txn] > 0 && st == 204 {
		f.dropReply[txn]--
		return nil, 0, false
	}
	return b, st, ok
}

func ra(n int64) *int64 { return &n }

func lp(amount int64, evt string, legs ...origPayment) eventPayload {
	return eventPayload{Amount: amount, OriginalSaleID: "sale-1", EventID: evt, OriginalPayments: legs}
}

func leg(ref string, amount, tip int64, refund *int64) origPayment {
	return origPayment{TransactionRef: ref, Amount: amount, TipAmount: tip, RefundAmount: refund}
}

func legEnv() (memKV, linkStore) { return memKV{}, linkStore{blobs: newFakeBlobs()} }

func captureLog(t *testing.T) *[]string {
	var lines []string
	logHook = func(m string) { lines = append(lines, m) }
	t.Cleanup(func() { logHook = nil })
	return &lines
}

func noRawRef(t *testing.T, lines []string, refs ...string) {
	t.Helper()
	for _, l := range lines {
		for _, r := range refs {
			if strings.Contains(l, r) {
				t.Errorf("log leaks %q: %s", r, l)
			}
		}
	}
}

func TestLegsAbsentUnchanged(t *testing.T) {
	api := newLegFake(map[string]int64{"txn-A": 3000, "txn-B": 2000})
	kv, links := legEnv()
	// two legs, no refund_amount: legacy big-first on ONE leg
	id, dec := refundSale(api, kv, links, lp(2000, "e1", leg("txn-A", 3000, 0, nil), leg("txn-B", 2000, 0, nil)))
	if dec != "" || id != "txn-A" || api.refunded["txn-A"] != 2000 || api.refunded["txn-B"] != 0 {
		t.Fatalf("id=%q dec=%q refunded=%v", id, dec, api.refunded)
	}
}

func TestLegsSingleLegPartial(t *testing.T) {
	api := newLegFake(map[string]int64{"txn-A": 3000})
	kv, links := legEnv()
	id, dec := refundSale(api, kv, links, lp(1500, "e1", leg("txn-A", 3000, 0, ra(1500))))
	if dec != "" || id != "txn-A" || len(api.bodies) != 1 || api.bodies[0] != `{"amount":15}` {
		t.Fatalf("id=%q dec=%q bodies=%v", id, dec, api.bodies)
	}
}

func TestLegsTwoLegsEachOwnAmount(t *testing.T) {
	api := newLegFake(map[string]int64{"txn-A": 3000, "txn-B": 2000})
	kv, links := legEnv()
	id, dec := refundSale(api, kv, links, lp(5000, "e1", leg("txn-A", 3000, 0, ra(3000)), leg("txn-B", 2000, 0, ra(2000))))
	if dec != "" || id != "txn-A" {
		t.Fatalf("id=%q dec=%q", id, dec)
	}
	if fmt.Sprint(api.calls) != "[txn-A txn-B]" || api.bodies[0] != `{"amount":30}` || api.bodies[1] != `{"amount":20}` {
		t.Fatalf("calls=%v bodies=%v", api.calls, api.bodies)
	}
	if rec := loadRefundRecords(kv); len(rec) != 1 || rec[0].State != "done" || rec[0].Ref != "txn-A" || len(rec[0].Legs) != 2 {
		t.Fatalf("record %+v", rec)
	}
}

func TestLegsSecondFailsThenRetry(t *testing.T) {
	api := newLegFake(map[string]int64{"txn-AAAA1111": 3000, "txn-BBBB2222": 100}) // B too small: 409
	kv, links := legEnv()
	lines := captureLog(t)
	p := lp(5000, "e1", leg("txn-AAAA1111", 3000, 0, ra(3000)), leg("txn-BBBB2222", 2000, 0, ra(2000)))
	_, dec := refundSale(api, kv, links, p)
	if dec != declineRefundPartial {
		t.Fatalf("dec=%q, want refund_partial", dec)
	}
	api.captured["txn-BBBB2222"] = 2000
	id, dec := refundSale(api, kv, links, p)
	if dec != "" || id != "txn-AAAA1111" {
		t.Fatalf("retry id=%q dec=%q", id, dec)
	}
	if api.refunded["txn-AAAA1111"] != 3000 || api.refunded["txn-BBBB2222"] != 2000 {
		t.Fatalf("refunded %v", api.refunded)
	}
	nA := 0
	for _, c := range api.calls {
		if c == "txn-AAAA1111" {
			nA++
		}
	}
	if nA != 1 {
		t.Errorf("leg A called %d times, want 1", nA)
	}
	// a different event id is a new refund (A is refundable no more here)
	_, dec = refundSale(api, kv, links, lp(5000, "e2", p.OriginalPayments...))
	if dec == "" {
		t.Errorf("different event id must not be deduped")
	}
	noRawRef(t, *lines, "txn-AAAA1111", "txn-BBBB2222")
}

func TestLegsDefiniteFailureFirstLegStartsFresh(t *testing.T) {
	api := newLegFake(map[string]int64{"txn-A": 100, "txn-B": 2000})
	kv, links := legEnv()
	p := lp(5000, "e1", leg("txn-A", 3000, 0, ra(3000)), leg("txn-B", 2000, 0, ra(2000)))
	_, dec := refundSale(api, kv, links, p)
	if dec != "CONFLICT" {
		t.Fatalf("dec=%q, want the leg's reason", dec)
	}
	if rec := loadRefundRecords(kv); len(rec) != 0 {
		t.Fatalf("record left behind: %+v", rec)
	}
	if len(api.calls) != 1 {
		t.Errorf("calls %v, want only leg A", api.calls)
	}
}

func TestLegsSendingFullResend(t *testing.T) {
	api := newLegFake(map[string]int64{"txn-A": 3000, "txn-B": 2000})
	api.dropReply["txn-B"] = 1 // applied at SumUp, reply lost
	kv, links := legEnv()
	p := lp(5000, "e1", leg("txn-A", 3000, 0, ra(3000)), leg("txn-B", 2000, 0, ra(2000)))
	if _, dec := refundSale(api, kv, links, p); dec != declineRefundPartial {
		t.Fatalf("dec=%q, want refund_partial", dec)
	}
	id, dec := refundSale(api, kv, links, p) // resend gets 409 "nothing left": ours
	if dec != "" || id != "txn-A" || api.refunded["txn-B"] != 2000 || api.refunded["txn-A"] != 3000 {
		t.Fatalf("id=%q dec=%q refunded=%v", id, dec, api.refunded)
	}
}

func TestLegsSendingPartialUnknown(t *testing.T) {
	api := newLegFake(map[string]int64{"txn-A": 3000, "txn-B": 2000})
	api.dropReply["txn-B"] = 1
	kv, links := legEnv()
	p := lp(3500, "e1", leg("txn-A", 3000, 0, ra(3000)), leg("txn-B", 2000, 0, ra(500)))
	if _, dec := refundSale(api, kv, links, p); dec != declineRefundPartial {
		t.Fatalf("dec=%q", dec)
	}
	n := len(api.calls)
	_, dec := refundSale(api, kv, links, p)
	if dec != declineRefundUnknown || len(api.calls) != n {
		t.Fatalf("dec=%q, new calls=%d", dec, len(api.calls)-n)
	}
	if api.refunded["txn-B"] != 500 {
		t.Errorf("B refunded %d, want 500", api.refunded["txn-B"])
	}
}

func TestLegsRecordBoundedAndLegacyReadable(t *testing.T) {
	api := newLegFake(map[string]int64{"txn-A": 1 << 30, "txn-B": 1 << 30})
	kv, links := legEnv()
	for i := 1; i <= 9; i++ {
		p := lp(200, fmt.Sprintf("evt-%d", i), leg("txn-A", 1<<20, 0, ra(100)), leg("txn-B", 1<<20, 0, ra(100)))
		if _, dec := refundSale(api, kv, links, p); dec != "" {
			t.Fatalf("event %d: %q", i, dec)
		}
	}
	rec := loadRefundRecords(kv)
	if len(rec) != maxRefundRecords || rec[0].ID != "evt-2" || rec[len(rec)-1].ID != "evt-9" {
		t.Fatalf("records %d first=%s", len(rec), rec[0].ID)
	}

	// a v1.6.0 record
	p := lp(5000, "evt-1", leg("txn-A", 3000, 0, ra(3000)), leg("txn-B", 2000, 0, ra(2000)))
	api = newLegFake(map[string]int64{"txn-A": 3000, "txn-B": 2000})
	kv, links = legEnv()
	kv[refundsKey] = `[{"id":"evt-1","ref":"txn-A","state":"done"}]`
	id, dec := refundSale(api, kv, links, p)
	if dec != "" || id != "txn-A" || len(api.calls) != 0 {
		t.Fatalf("legacy done: id=%q dec=%q calls=%v", id, dec, api.calls)
	}
	kv[refundsKey] = `[{"id":"evt-1","ref":"txn-A","state":"sending"}]`
	_, dec = refundSale(api, kv, links, p)
	if dec != declineRefundUnknown || len(api.calls) != 0 {
		t.Fatalf("legacy sending: dec=%q calls=%v", dec, api.calls)
	}
	// and the legacy path still reads a v1.6.0 record without refund_amount
	var r []refundRecord
	_ = json.Unmarshal([]byte(kv[refundsKey]), &r)
	if len(r) != 1 || r[0].Legs != nil {
		t.Fatalf("legacy record parsed as %+v", r)
	}
}

func TestLegsRecordFitsHostCap(t *testing.T) {
	kv := newCapKV()
	for e := 0; e < maxRefundRecords; e++ {
		id := fmt.Sprintf("%0145d", e)
		rec := refundRecord{ID: id, Ref: "ut-" + strings.Repeat("r", 77), State: "legs"}
		for i := 0; i < maxRefundLegs; i++ {
			rec.Legs = append(rec.Legs, legRecord{Ref: "ut-" + strings.Repeat("x", 77), Amount: 123456789, State: "done"})
		}
		saveRefundRecord(kv, rec)
	}
	if got := len(loadRefundRecords(kv)); got != maxRefundRecords {
		t.Fatalf("stored %d records (put failed?), size %d", got, len(kv.get(refundsKey)))
	}
}

func TestLegsInvalidNoCall(t *testing.T) {
	many := []origPayment{}
	for i := 0; i < maxRefundLegs+1; i++ {
		many = append(many, leg(fmt.Sprintf("txn-%d", i), 100, 0, ra(100)))
	}
	cases := []struct {
		name string
		p    eventPayload
		want string
	}{
		{"zero", lp(100, "e", leg("a", 3000, 0, ra(0)), leg("b", 100, 0, ra(100))), declineInvalidRefundAmount},
		{"negative", lp(100, "e", leg("a", 3000, 0, ra(-5)), leg("b", 105, 0, ra(105))), declineInvalidRefundAmount},
		{"above leg", lp(3101, "e", leg("a", 3000, 100, ra(3101))), declineInvalidRefundAmount},
		{"sum mismatch", lp(4000, "e", leg("a", 3000, 0, ra(3000)), leg("b", 2000, 0, ra(500))), declineInvalidRefundAmount},
		{"negative tip", lp(100, "e", leg("a", 3000, -1, ra(100))), declineInvalidRefundAmount},
		{"missing ref", lp(5000, "e", leg("a", 3000, 0, ra(3000)), leg("", 2000, 0, ra(2000))), declineUnknownOriginal},
		{"one without field", lp(5000, "e", leg("a", 3000, 0, ra(3000)), leg("b", 2000, 0, nil)), declineInvalidRefundAmount},
		{"too many legs", lp(int64(len(many))*100, "e", many...), declineTooManyLegs},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := newLegFake(map[string]int64{"a": 1 << 30, "b": 1 << 30})
			kv, links := legEnv()
			_, dec := refundSale(api, kv, links, c.p)
			if dec != c.want || len(api.calls) != 0 {
				t.Fatalf("dec=%q want %q, calls=%v", dec, c.want, api.calls)
			}
		})
	}
}

func TestLegsDemoAndNoKey(t *testing.T) {
	kv, links := legEnv()
	id, dec := refundSale(nil, kv, links, lp(5000, "e1", leg("demo-1", 3000, 0, ra(3000)), leg("demo-2", 2000, 0, ra(2000))))
	if id != "demo-1" || dec != "" {
		t.Fatalf("demo: %q %q", id, dec)
	}
	_, dec = refundSale(nil, kv, links, lp(5000, "e2", leg("txn-A", 3000, 0, ra(3000)), leg("demo-2", 2000, 0, ra(2000))))
	if dec != "no_api_key" {
		t.Fatalf("dec=%q", dec)
	}
}

func TestLegsReversalIgnoresRefundAmount(t *testing.T) {
	api := newLegFake(map[string]int64{"txn-H": 900})
	kv, links := legEnv()
	p := lp(900, "e1", leg("txn-H", 900, 0, ra(100)))
	p.Reversal = true
	id, dec := refundSale(api, kv, links, p)
	if dec != "" || id != "txn-H" || len(api.bodies) != 1 || api.bodies[0] != `{"amount":9}` {
		t.Fatalf("id=%q dec=%q bodies=%v", id, dec, api.bodies)
	}
}

func TestLegsLogsMasked(t *testing.T) {
	lines := captureLog(t)
	api := newLegFake(map[string]int64{"txn-AAAA1111": 3000, "txn-BBBB2222": 2000})
	kv, links := legEnv()
	p := lp(5000, "e1", leg("txn-AAAA1111", 3000, 0, ra(3000)), leg("txn-BBBB2222", 2000, 0, ra(2000)))
	api.failNet["txn-BBBB2222"] = true
	refundSale(api, kv, links, p)
	api.failNet["txn-BBBB2222"] = false
	refundSale(api, kv, links, p)
	if len(*lines) == 0 {
		t.Fatal("expected log lines")
	}
	noRawRef(t, *lines, "txn-AAAA1111", "txn-BBBB2222")
}
