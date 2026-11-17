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

func (m memKV) get(k string) string { return m[k] }
func (m memKV) put(k string, v []byte) error {
	m[k] = string(v)
	return nil
}

// fakeSumUp simulates one merchant with one Solo reader, following SumUp's
// published Cloud API docs (not a kinder model): the transactions lookup
// answers 404 when no transaction matches; terminate answers 202 while the
// reader is online, acts asynchronously and only on a checkout still
// waiting for a card, and by default leaves no transaction behind. The
// customer taps at tapAt (from the flow's start); the transaction is then
// PENDING for procDur and SUCCESSFUL after, listed visibleAfter the tap.
type fakeSumUp struct {
	clk     *fakeClock
	start   time.Time
	latency time.Duration
	tapAt   time.Duration // 0 = never taps
	procDur time.Duration

	checkouts      []*fakeCheckout
	terminates     int
	statusReads    int
	startNetFail   bool // the checkout reaches the reader but the reply is lost
	startRejected  bool
	terminateFails bool          // terminate leaves a FAILED transaction (undocumented; off by default)
	visibleAfter   time.Duration // listing lag after the tap
	stuckState     string        // reader status state override (e.g. WAITING_FOR_CARD)
	offline        bool          // reader status OFFLINE (state may still read IDLE)
}

type fakeCheckout struct {
	clientTxn string
	waiting   bool
	tapped    time.Time
	status    string // "" (no transaction), PENDING, SUCCESSFUL, FAILED
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
		for _, c := range f.checkouts {
			if c.waiting {
				c.waiting = false
				if f.terminateFails {
					c.status, c.tapped = "FAILED", f.clk.t
				}
			}
		}
		return nil, 202, true
	case method == "GET" && strings.HasSuffix(path, "/status"):
		f.statusReads++
		state := "IDLE"
		for _, c := range f.checkouts {
			if c.waiting {
				state = "WAITING_FOR_CARD"
			}
		}
		if f.stuckState != "" {
			state = f.stuckState
		}
		online := "ONLINE"
		if f.offline {
			online = "OFFLINE"
		}
		b, _ := json.Marshal(map[string]any{"data": map[string]string{"status": online, "state": state}})
		return b, 200, true
	case method == "GET" && strings.Contains(path, "/transactions?client_transaction_id="):
		id := path[strings.Index(path, "=")+1:]
		for _, c := range f.checkouts {
			if c.clientTxn == id && c.status != "" && !f.clk.t.Before(c.tapped.Add(f.visibleAfter)) {
				b, _ := json.Marshal(map[string]any{"id": "txn-" + id, "status": c.status})
				return b, 200, true
			}
		}
		return []byte(`{"error_code":"NOT_FOUND"}`), 404, true
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
	c.start = start
	out := c.run()
	if took := clk.t.Sub(start); took >= hostGate {
		t.Fatalf("flow took %s, past the host's %s payment gate: the till has already shown the sale unpaid and killed the plugin", took, hostGate)
	}
	return out
}

func newWorld() (*fakeClock, *fakeSumUp, memKV) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	return clk, newFakeSumUp(clk), memKV{}
}

// retap is the cashier tapping Pay again on the same basket after wait.
func retap(f *fakeSumUp, clk *fakeClock, wait, tapAt time.Duration) {
	clk.sleep(wait)
	f.start, f.tapAt = clk.t, tapAt
}

func TestReaderLateTapAfterTimeoutIsNotCharged(t *testing.T) {
	for _, failedTxn := range []bool{false, true} {
		clk, f, kv := newWorld()
		f.terminateFails = failedTxn
		f.tapAt = 29 * time.Second
		out := runReader(t, newReaderCharger(f, kv, "att:0:ab"), clk)
		want := "outcome_unknown" // a 202 alone proves nothing
		if failedTxn {
			want = "reader_timeout"
		}
		if out.approved || out.reason != want {
			t.Fatalf("failedTxn=%v: want declined %s, got %+v", failedTxn, want, out)
		}
		if f.terminates == 0 {
			t.Fatal("reader checkout was never terminated")
		}
		clk.sleep(60 * time.Second)
		if n := f.charged(); n != 0 {
			t.Fatalf("customer charged %d time(s) for a sale the till showed unpaid", n)
		}
	}
}

func TestReaderTapDuringTerminateIsReportedPaid(t *testing.T) {
	clk, f, kv := newWorld()
	f.tapAt = 17900 * time.Millisecond
	f.procDur = 3 * time.Second
	out := runReader(t, newReaderCharger(f, kv, "att:0:ab"), clk)
	if !out.approved || out.authCode != "txn-ctx-a" {
		t.Fatalf("captured payment not surfaced as approved: %+v", out)
	}
}

func TestReaderUnsettledAtDeadlineIsOutcomeUnknown(t *testing.T) {
	clk, f, kv := newWorld()
	f.tapAt = 17 * time.Second
	f.procDur = time.Minute
	out := runReader(t, newReaderCharger(f, kv, "att:0:ab"), clk)
	if out.approved || out.reason != "outcome_unknown" {
		t.Fatalf("want declined outcome_unknown, got %+v", out)
	}
	if e, ok := findAttempt(kv, "att:0:ab"); !ok || e.Ref != "ctx-a" {
		t.Fatalf("attempt record not kept: %q", kv[attemptsKey])
	}
}

func TestReaderRetryAfterLateSuccessApprovesWithoutSecondCharge(t *testing.T) {
	clk, f, kv := newWorld()
	f.tapAt = 17 * time.Second
	f.procDur = 15 * time.Second
	if first := runReader(t, newReaderCharger(f, kv, "att:0:ab"), clk); first.approved {
		t.Fatalf("first attempt should not know the outcome yet: %+v", first)
	}
	retap(f, clk, 30*time.Second, 0)
	second := runReader(t, newReaderCharger(f, kv, "att:0:ab"), clk)
	if !second.approved || second.authCode != "txn-ctx-a" {
		t.Fatalf("retry did not surface the late payment: %+v", second)
	}
	if len(f.checkouts) != 1 || f.charged() != 1 {
		t.Fatalf("want 1 checkout and 1 charge, got %d / %d", len(f.checkouts), f.charged())
	}
}

// Review B2: the card is read just before terminate and SumUp lists the
// transaction only seconds later. The first attempt can't see it; the
// retry must find it, never start a second checkout.
func TestReaderListingLagNeverDoubleCharges(t *testing.T) {
	clk, f, kv := newWorld()
	f.tapAt = 17900 * time.Millisecond
	f.procDur = time.Second
	f.visibleAfter = 7 * time.Second
	first := runReader(t, newReaderCharger(f, kv, "att:0:ab"), clk)
	if first.approved || first.reason != "outcome_unknown" {
		t.Fatalf("want outcome_unknown while SumUp lags, got %+v", first)
	}
	retap(f, clk, 2*time.Second, 3*time.Second)
	second := runReader(t, newReaderCharger(f, kv, "att:0:ab"), clk)
	if !second.approved || second.authCode != "txn-ctx-a" {
		t.Fatalf("retry should approve the first checkout: %+v", second)
	}
	if len(f.checkouts) != 1 || f.charged() != 1 {
		t.Fatalf("want 1 checkout and 1 charge, got %d / %d", len(f.checkouts), f.charged())
	}
}

// Review B1/B2: a retry soon after a no-tap timeout finds no transaction
// (404). It must not start a second checkout yet, and must answer fast.
func TestReaderEarlyRetryWithNoTransactionStartsNothing(t *testing.T) {
	clk, f, kv := newWorld()
	if first := runReader(t, newReaderCharger(f, kv, "att:0:ab"), clk); first.approved {
		t.Fatalf("no tap but approved: %+v", first)
	}
	retap(f, clk, 5*time.Second, 3*time.Second)
	start := clk.t
	second := runReader(t, newReaderCharger(f, kv, "att:0:ab"), clk)
	if second.approved || second.reason != "outcome_unknown" || len(f.checkouts) != 1 {
		t.Fatalf("want outcome_unknown and no new checkout, got %+v (%d checkouts)", second, len(f.checkouts))
	}
	if took := clk.t.Sub(start); took > 3*time.Second {
		t.Fatalf("early retry took %s; should answer at once", took)
	}
}

// Past settleWindow with no transaction and an idle reader, the earlier
// checkout never read a card: the retry charges fresh (no stuck basket).
func TestReaderLateRetryOnIdleReaderChargesFresh(t *testing.T) {
	clk, f, kv := newWorld()
	if first := runReader(t, newReaderCharger(f, kv, "att:0:ab"), clk); first.approved {
		t.Fatalf("no tap but approved: %+v", first)
	}
	retap(f, clk, 90*time.Second, 3*time.Second)
	second := runReader(t, newReaderCharger(f, kv, "att:0:ab"), clk)
	if !second.approved || second.authCode != "txn-ctx-b" || f.charged() != 1 {
		t.Fatalf("late retry should charge a fresh checkout once: %+v charged=%d", second, f.charged())
	}
}

// ...but not while the reader says it is still busy.
func TestReaderLateRetryOnBusyReaderStartsNothing(t *testing.T) {
	clk, f, kv := newWorld()
	runReader(t, newReaderCharger(f, kv, "att:0:ab"), clk)
	f.stuckState = "WAITING_FOR_PIN"
	retap(f, clk, 120*time.Second, 3*time.Second)
	second := runReader(t, newReaderCharger(f, kv, "att:0:ab"), clk)
	if second.approved || second.reason != "outcome_unknown" || len(f.checkouts) != 1 {
		t.Fatalf("want outcome_unknown and no new checkout, got %+v", second)
	}
}

// When SumUp does record the terminated checkout as FAILED, the retry
// starts fresh at once.
func TestReaderRetryAfterFailedCheckoutChargesFresh(t *testing.T) {
	clk, f, kv := newWorld()
	f.terminateFails = true
	runReader(t, newReaderCharger(f, kv, "att:0:ab"), clk)
	retap(f, clk, 5*time.Second, 3*time.Second)
	second := runReader(t, newReaderCharger(f, kv, "att:0:ab"), clk)
	if !second.approved || second.authCode != "txn-ctx-b" {
		t.Fatalf("retry should start a fresh checkout: %+v", second)
	}
}

// Settling the earlier attempt used most of the gate: decline rather than
// start a checkout that would be terminated at once.
func TestReaderSlowRetrySettleDeclinesInsteadOfStartingLate(t *testing.T) {
	clk, f, kv := newWorld()
	f.terminateFails = true
	runReader(t, newReaderCharger(f, kv, "att:0:ab"), clk)
	// The earlier card read is still in flight and is declined 8 s later.
	f.checkouts[0].status, f.checkouts[0].tapped = "PENDING", clk.t
	f.procDur = time.Hour
	retap(f, clk, time.Second, 0)
	c := newReaderCharger(f, kv, "att:0:ab")
	c.api = failAfter{f, 8 * time.Second}
	second := runReader(t, c, clk)
	if second.approved || second.reason != "previous_attempt_settled" || len(f.checkouts) != 1 {
		t.Fatalf("want previous_attempt_settled and no new checkout, got %+v (%d checkouts)", second, len(f.checkouts))
	}
}

// failAfter turns the first checkout FAILED once d has passed since the
// wrapper's first call (a card read that is declined by the issuer).
type failAfter struct {
	f *fakeSumUp
	d time.Duration
}

func (w failAfter) call(method, path string, body []byte) ([]byte, int, bool) {
	c := w.f.checkouts[0]
	if c.status == "PENDING" && !w.f.clk.t.Before(c.tapped.Add(w.d)) {
		c.status = "FAILED"
	}
	return w.f.call(method, path, body)
}

// The checkout reached the reader but its reply was lost: the reader is
// prompting for a sale the till will decline, so terminate it.
func TestReaderLostStartReplyTerminatesReader(t *testing.T) {
	clk, f, kv := newWorld()
	f.startNetFail = true
	f.tapAt = 25 * time.Second
	out := runReader(t, newReaderCharger(f, kv, "att:0:ab"), clk)
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
	clk, f, kv := newWorld()
	f.tapAt = 4 * time.Second
	out := runReader(t, newReaderCharger(f, kv, "att:0:ab"), clk)
	if !out.approved || out.authCode != "txn-ctx-a" || f.terminates != 0 {
		t.Fatalf("prompt tap: %+v terminates=%d", out, f.terminates)
	}
}

// An OFFLINE reader may report its last state (IDLE): not proof it is idle.
func TestReaderLateRetryOnOfflineReaderStartsNothing(t *testing.T) {
	clk, f, kv := newWorld()
	runReader(t, newReaderCharger(f, kv, "att:0:ab"), clk)
	f.offline = true
	retap(f, clk, 120*time.Second, 3*time.Second)
	second := runReader(t, newReaderCharger(f, kv, "att:0:ab"), clk)
	if second.approved || second.reason != "outcome_unknown" || len(f.checkouts) != 1 {
		t.Fatalf("want outcome_unknown and no new checkout, got %+v", second)
	}
}

// Only the last maxAttempts tenders are kept, in one storage key.
func TestAttemptLogIsBounded(t *testing.T) {
	kv := memKV{}
	for i := 0; i < maxAttempts+5; i++ {
		putAttempt(kv, attemptEntry{ID: "id" + string(rune('a'+i)), Ref: "ctx"})
	}
	if n := len(loadAttempts(kv)); n != maxAttempts || len(kv) != 1 {
		t.Fatalf("want %d entries in 1 key, got %d entries in %d keys", maxAttempts, n, len(kv))
	}
	if _, ok := findAttempt(kv, "ida"); ok {
		t.Fatal("oldest entry not dropped")
	}
}
