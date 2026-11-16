package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// hostGate mirrors universal-till's paymentGateTimeout (internal/plugins/
// wasm_runtime.go): the host kills the guest at 30 s, so the reader flow —
// terminate included — must be over before then (ut-docs#6304).
const hostGate = 30 * time.Second

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time        { return c.t }
func (c *fakeClock) sleep(d time.Duration) { c.t = c.t.Add(d) }

type memKV map[string]string

func (m memKV) get(k string) string    { return m[k] }
func (m memKV) put(k string, v []byte) { m[k] = string(v) }

// fakeSumUp simulates one merchant with one Solo reader on the Cloud API.
// The customer taps at tapAt (from the flow's start); the transaction is
// then PENDING for procDur and SUCCESSFUL after. A tap only lands while a
// checkout is waiting on the reader; terminate ends the wait, but SumUp
// refuses it once the card is being processed.
type fakeSumUp struct {
	clk     *fakeClock
	start   time.Time
	latency time.Duration
	tapAt   time.Duration // 0 = never taps
	procDur time.Duration

	checkouts     []*fakeCheckout
	terminates    int
	startNetFail  bool // the checkout reaches the reader but the reply is lost
	startRejected bool

	// Closer-to-reality knobs (SumUp's terminate is async, unconfirmed,
	// and only accepted while the reader waits for a card or PIN):
	terminateLeavesNoTxn bool          // a terminated checkout leaves no transaction
	terminateRefusedIdle bool          // terminate on an idle reader -> 422
	terminateAlwaysOK    bool          // 202 even while a card is being processed
	visibleAfter         time.Duration // a transaction is listed only this long after the tap
}

type fakeCheckout struct {
	clientTxn string
	waiting   bool
	tapped    time.Time
	status    string // "" (no transaction yet), PENDING, SUCCESSFUL, CANCELLED
}

func newFakeSumUp(clk *fakeClock) *fakeSumUp {
	return &fakeSumUp{clk: clk, start: clk.t, latency: 300 * time.Millisecond, procDur: 2 * time.Second}
}

func (f *fakeSumUp) advance() {
	for _, c := range f.checkouts {
		if c.waiting && c.status == "" && f.tapAt > 0 && !f.clk.t.Before(f.start.Add(f.tapAt)) {
			c.status, c.tapped, c.waiting = "PENDING", f.start.Add(f.tapAt), false
		}
		if c.status == "PENDING" && !f.clk.t.Before(c.tapped.Add(f.procDur)) {
			c.status = "SUCCESSFUL"
		}
	}
}

func (f *fakeSumUp) charged() int {
	f.advance()
	n := 0
	for _, c := range f.checkouts {
		if c.status == "SUCCESSFUL" {
			n++
		}
	}
	return n
}

func (f *fakeSumUp) call(method, path string, body []byte) ([]byte, int, bool) {
	f.clk.sleep(f.latency)
	f.advance()
	switch {
	case method == "POST" && strings.HasSuffix(path, "/checkout"):
		if f.startRejected {
			return []byte(`{"message":"reader busy"}`), 422, true
		}
		c := &fakeCheckout{clientTxn: "ctx-" + string(rune('a'+len(f.checkouts))), waiting: true}
		f.checkouts = append(f.checkouts, c)
		if f.startNetFail {
			return nil, 0, false
		}
		b, _ := json.Marshal(map[string]any{"data": map[string]string{"client_transaction_id": c.clientTxn}})
		return b, 201, true
	case method == "POST" && strings.HasSuffix(path, "/terminate"):
		f.terminates++
		if f.terminateAlwaysOK {
			for _, c := range f.checkouts {
				c.waiting = false
			}
			return nil, 202, true
		}
		if f.terminateRefusedIdle {
			idle := true
			for _, c := range f.checkouts {
				if c.waiting {
					idle = false
				}
			}
			if idle {
				return []byte(`{"message":"reader not in a transaction"}`), 422, true
			}
		}
		for _, c := range f.checkouts {
			if c.status == "PENDING" {
				return []byte(`{"message":"transaction in progress"}`), 422, true
			}
		}
		for _, c := range f.checkouts {
			if c.waiting {
				c.waiting = false
				if c.status == "" && !f.terminateLeavesNoTxn {
					c.status = "CANCELLED"
				}
			}
		}
		return nil, 202, true
	case method == "GET" && strings.Contains(path, "/transactions?client_transaction_id="):
		id := path[strings.Index(path, "=")+1:]
		for _, c := range f.checkouts {
			if c.clientTxn == id && c.status != "" && c.status != "CANCELLED" && f.clk.t.Before(c.tapped.Add(f.visibleAfter)) {
				break // the transaction exists but is not listed yet
			}
			if c.clientTxn == id && c.status != "" {
				b, _ := json.Marshal(map[string]any{"items": []map[string]any{{"id": "txn-" + id, "status": c.status}}})
				return b, 200, true
			}
		}
		return []byte(`{"items":[]}`), 200, true
	}
	return []byte(`{"message":"unexpected"}`), 400, true
}

func newReaderCharger(f *fakeSumUp, kv memKV, requestID string) *readerCharger {
	return &readerCharger{
		api: f, clk: f.clk, kv: kv,
		affiliateKey: "aff", merchantCode: "M1", currency: "gbp", readerID: "rdr_1",
		requestID: requestID, amount: 1250, start: f.clk.t,
	}
}

func runReader(t *testing.T, c *readerCharger, clk *fakeClock) readerOutcome {
	t.Helper()
	start := clk.t
	out := c.run()
	if took := clk.t.Sub(start); took >= hostGate {
		t.Fatalf("flow took %s, past the host's %s payment gate: the till has already shown the sale unpaid and killed the plugin", took, hostGate)
	}
	return out
}

func TestReaderLateTapAfterTimeoutIsTerminatedNotCharged(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	f := newFakeSumUp(clk)
	f.tapAt = 29 * time.Second
	out := runReader(t, newReaderCharger(f, memKV{}, "att:0:ab"), clk)
	if out.approved || out.reason != "reader_timeout" {
		t.Fatalf("want declined reader_timeout, got %+v", out)
	}
	if f.terminates == 0 {
		t.Fatal("reader checkout was never terminated")
	}
	clk.sleep(60 * time.Second)
	if n := f.charged(); n != 0 {
		t.Fatalf("customer charged %d time(s) for a sale the till showed unpaid", n)
	}
}

func TestReaderTapDuringTerminateIsReportedPaid(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	f := newFakeSumUp(clk)
	f.tapAt = 17900 * time.Millisecond
	f.procDur = 3 * time.Second
	out := runReader(t, newReaderCharger(f, memKV{}, "att:0:ab"), clk)
	if !out.approved || out.authCode == "" {
		t.Fatalf("captured payment not surfaced as approved: %+v", out)
	}
}

func TestReaderUnsettledAtDeadlineIsOutcomeUnknown(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	f := newFakeSumUp(clk)
	f.tapAt = 17 * time.Second
	f.procDur = time.Minute
	kv := memKV{}
	out := runReader(t, newReaderCharger(f, kv, "att:0:ab"), clk)
	if out.approved || out.reason != "outcome_unknown" {
		t.Fatalf("want declined outcome_unknown, got %+v", out)
	}
	if !strings.Contains(kv["attempt:att:0:ab"], "ctx-a") {
		t.Fatalf("attempt record not kept: %q", kv["attempt:att:0:ab"])
	}
}

func TestReaderRetryAfterLateSuccessApprovesWithoutSecondCharge(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	f := newFakeSumUp(clk)
	f.tapAt = 17 * time.Second
	f.procDur = 15 * time.Second
	kv := memKV{}
	if first := runReader(t, newReaderCharger(f, kv, "att:0:ab"), clk); first.approved {
		t.Fatalf("first attempt should not know the outcome yet: %+v", first)
	}
	clk.sleep(30 * time.Second)
	f.start, f.tapAt = clk.t, 0
	second := runReader(t, newReaderCharger(f, kv, "att:0:ab"), clk)
	if !second.approved || second.authCode != "txn-ctx-a" {
		t.Fatalf("retry did not surface the late payment: %+v", second)
	}
	if len(f.checkouts) != 1 || f.charged() != 1 {
		t.Fatalf("want 1 checkout and 1 charge, got %d / %d", len(f.checkouts), f.charged())
	}
}

func TestReaderRetryAfterCancelledAttemptChargesFresh(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	f := newFakeSumUp(clk)
	kv := memKV{}
	if first := runReader(t, newReaderCharger(f, kv, "att:0:ab"), clk); first.approved {
		t.Fatalf("no tap but approved: %+v", first)
	}
	clk.sleep(5 * time.Second)
	f.start, f.tapAt = clk.t, 3*time.Second
	second := runReader(t, newReaderCharger(f, kv, "att:0:ab"), clk)
	if !second.approved || second.authCode != "txn-ctx-b" {
		t.Fatalf("retry should start a fresh checkout: %+v", second)
	}
}

// The checkout reached the reader but its reply was lost: the reader is
// prompting for a sale the till will decline, so terminate it.
func TestReaderLostStartReplyTerminatesReader(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	f := newFakeSumUp(clk)
	f.startNetFail = true
	f.tapAt = 25 * time.Second
	out := runReader(t, newReaderCharger(f, memKV{}, "att:0:ab"), clk)
	if out.approved || out.reason != "network_error" {
		t.Fatalf("want declined network_error, got %+v", out)
	}
	if f.terminates == 0 {
		t.Fatal("reader left prompting after a lost checkout reply")
	}
	clk.sleep(60 * time.Second)
	if n := f.charged(); n != 0 {
		t.Fatalf("customer charged %d time(s) for a declined sale", n)
	}
}

func TestReaderPromptTapApproves(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	f := newFakeSumUp(clk)
	f.tapAt = 4 * time.Second
	out := runReader(t, newReaderCharger(f, memKV{}, "att:0:ab"), clk)
	if !out.approved || out.authCode != "txn-ctx-a" || f.terminates != 0 {
		t.Fatalf("prompt tap: %+v terminates=%d", out, f.terminates)
	}
}

// Real SumUp: terminating a checkout before any card read may leave no
// transaction, and terminate is refused on an idle reader. The cashier's
// retry must still start a fresh checkout, not report outcome_unknown
// forever (ut-docs#6304 Tester D1).
func TestReaderRetryAfterTimeoutWithNoTransactionChargesFresh(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	f := newFakeSumUp(clk)
	f.terminateLeavesNoTxn, f.terminateRefusedIdle = true, true
	kv := memKV{}
	if first := runReader(t, newReaderCharger(f, kv, "att:0:ab"), clk); first.approved || first.reason != "reader_timeout" {
		t.Fatalf("want reader_timeout, got %+v", first)
	}
	clk.sleep(5 * time.Second)
	f.start, f.tapAt = clk.t, 3*time.Second
	second := runReader(t, newReaderCharger(f, kv, "att:0:ab"), clk)
	if !second.approved || second.authCode != "txn-ctx-b" {
		t.Fatalf("retry should start a fresh checkout: %+v", second)
	}
}

// A transaction that shows up a few seconds after an accepted terminate
// (the card was read just before it) is the customer's payment.
func TestReaderTransactionAppearingAfterTerminateIsPaid(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	f := newFakeSumUp(clk)
	f.terminateAlwaysOK, f.terminateLeavesNoTxn = true, true
	f.tapAt = 17900 * time.Millisecond
	f.procDur = time.Second
	f.visibleAfter = 3 * time.Second
	out := runReader(t, newReaderCharger(f, memKV{}, "att:0:ab"), clk)
	if !out.approved || out.authCode != "txn-ctx-a" {
		t.Fatalf("payment that went through was not reported paid: %+v", out)
	}
}

// Terminate refused and no transaction: the reader may be mid-read, so
// this is unknown, never "not charged".
func TestReaderTerminateRefusedWithNoTransactionIsUnknown(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	f := newFakeSumUp(clk)
	f.terminateRefusedIdle, f.terminateLeavesNoTxn = true, true
	f.tapAt = 17900 * time.Millisecond
	f.procDur = time.Minute
	f.visibleAfter = time.Minute
	f.terminateAlwaysOK = false
	out := runReader(t, newReaderCharger(f, memKV{}, "att:0:ab"), clk)
	if out.approved || out.reason != "outcome_unknown" {
		t.Fatalf("want outcome_unknown, got %+v", out)
	}
}

// A retry long after an unknown outcome whose checkout never produced a
// transaction: no card was read, so start fresh instead of staying stuck.
func TestReaderStaleAttemptWithNoTransactionChargesFresh(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	f := newFakeSumUp(clk)
	kv := memKV{"attempt:att:0:ab": `{"client_txn":"ctx-old","started_at":1700000000}`}
	f.terminateRefusedIdle = true
	clk.sleep(10 * time.Minute)
	f.start, f.tapAt = clk.t, 3*time.Second
	out := runReader(t, newReaderCharger(f, kv, "att:0:ab"), clk)
	if !out.approved || out.authCode != "txn-ctx-a" {
		t.Fatalf("stale attempt should not block a fresh checkout: %+v", out)
	}
}
