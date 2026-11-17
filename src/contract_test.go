package main

// Contract tests (ut-docs#6565, #6561): the plugin's reader flow and refund
// run over REAL HTTP against a fake SumUp server modelled on the exchanges
// recorded in the SumUp sandbox on 2026-11-17 (Virtual Solo, sandbox
// affiliate key; ut-docs#6561 comments). SCOPE: request paths and JSON
// bodies, status codes and reply shapes. NOT covered: headers/auth and the
// base URL (the test adapter httpSumUp stands in for main.go's sumupCall,
// which is wasip1-only). Recorded in the sandbox: flat checkout body, 201,
// lookup by foreign_transaction_id 404 then 200, FAILED for decline and
// terminate, READER_BUSY 422, refund 204 as a separate entry. ASSUMED, not
// recorded: lookup by client_transaction_id behaves the same, the lookup
// reply's id field, the exact checkout reply envelope. No key, merchant code
// or reader id here is real. The clock is virtual, so the suite is instant.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	cMerchant = "MTESTMERCH"
	cReader   = "rdr_TEST"
)

// sandboxServer models the verified behaviour: one reader, one checkout at a
// time, lookups by foreign_transaction_id answer 404 until the checkout has
// a final status.
type sandboxServer struct {
	mu  sync.Mutex
	clk *fakeClock

	// how the customer behaves on the pending checkout
	tapAt    time.Duration // from checkout start; 0 = never
	declines bool          // the tap is a decline -> FAILED
	failedAt time.Duration // a decline only becomes visible this long after the checkout (sandbox: ~40 s)

	flatReply bool // checkout reply is flat {client_transaction_id} instead of {data:{...}}

	// state
	pending   bool
	startedAt time.Time
	foreignID string
	clientTxn string
	final     string // "", SUCCESSFUL, FAILED
	txnID     string
	amount    int64

	requests []recordedReq
	refunds  []string // transaction ids refunded
	history  []string // entries as the dashboard would list them
	problems []string // contract violations found in requests
}

type recordedReq struct {
	method, path string
	body         []byte
}

func (s *sandboxServer) fail(msg string) { s.problems = append(s.problems, msg) }

// settle applies the customer's behaviour up to the virtual now.
func (s *sandboxServer) settle() {
	if !s.pending || s.final != "" || s.tapAt == 0 {
		return
	}
	since := s.clk.now().Sub(s.startedAt)
	switch {
	case !s.declines && since >= s.tapAt+3*time.Second:
		s.final, s.pending = "SUCCESSFUL", false
	case s.declines && since >= s.failedAt:
		s.final, s.pending = "FAILED", false
	}
}

func (s *sandboxServer) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	path := r.URL.Path
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	s.requests = append(s.requests, recordedReq{r.Method, path, body})
	s.settle()
	w.Header().Set("Content-Type", "application/json")
	reply := func(code int, v any) {
		w.WriteHeader(code)
		if v != nil {
			_ = json.NewEncoder(w).Encode(v)
		}
	}
	rp := "/v0.1/merchants/" + cMerchant + "/readers/" + cReader
	switch {
	case r.Method == "POST" && r.URL.Path == rp+"/checkout":
		var req struct {
			TotalAmount struct {
				Currency  string `json:"currency"`
				MinorUnit *int   `json:"minor_unit"`
				Value     *int64 `json:"value"`
			} `json:"total_amount"`
			Affiliate struct {
				AppID     string `json:"app_id"`
				Key       string `json:"key"`
				ForeignID string `json:"foreign_transaction_id"`
			} `json:"affiliate"`
			Checkout json.RawMessage `json:"checkout"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			reply(400, map[string]any{"message": "bad json"})
			return
		}
		if req.Checkout != nil {
			s.fail("outdated {checkout:{amount}} wrapper sent (affiliate-keys docs page); create-checkout uses flat total_amount")
		}
		if req.TotalAmount.Currency == "" || req.TotalAmount.MinorUnit == nil || req.TotalAmount.Value == nil {
			s.fail("total_amount must carry currency, minor_unit and value")
			reply(400, map[string]any{"type": "INVALID_REQUEST"})
			return
		}
		if req.Affiliate.AppID == "" || req.Affiliate.Key == "" {
			s.fail("affiliate needs both app_id and key")
			reply(403, map[string]any{"type": "INVALID_AFFILIATE_KEY"})
			return
		}
		if s.pending {
			reply(422, map[string]any{"type": "READER_BUSY", "message": "There is a pending checkout for the device."})
			return
		}
		s.pending, s.final = true, ""
		s.startedAt, s.foreignID = s.clk.now(), req.Affiliate.ForeignID
		s.amount = *req.TotalAmount.Value
		s.clientTxn = "ctx-" + s.startedAt.Format("150405")
		s.txnID = "TAAA" + s.startedAt.Format("150405")
		if s.flatReply {
			reply(201, map[string]any{"checkout_id": "co-1", "client_transaction_id": s.clientTxn})
		} else {
			reply(201, map[string]any{"data": map[string]any{"client_transaction_id": s.clientTxn}})
		}
	case r.Method == "POST" && r.URL.Path == rp+"/terminate":
		if s.pending && s.final == "" {
			// documented 202; the checkout then ends FAILED, no reason given
			s.final, s.pending = "FAILED", false
		}
		reply(202, nil)
	case r.Method == "GET" && r.URL.Path == rp+"/status":
		state := "IDLE"
		if s.pending {
			state = "WAITING_FOR_CARD"
		}
		reply(200, map[string]any{"data": map[string]any{"status": "ONLINE", "state": state}})
	case r.Method == "GET" && r.URL.Path == "/v2.1/merchants/"+cMerchant+"/transactions":
		q := r.URL.Query()
		match := (q.Get("foreign_transaction_id") != "" && q.Get("foreign_transaction_id") == s.foreignID) ||
			(q.Get("client_transaction_id") != "" && q.Get("client_transaction_id") == s.clientTxn)
		if !match || s.final == "" {
			reply(404, map[string]any{"error_code": "NOT_FOUND", "message": "Not found"})
			return
		}
		reply(200, map[string]any{
			"id": s.txnID, "status": s.final, "amount": float64(s.amount) / 100,
			"currency": "GBP", "foreign_transaction_id": s.foreignID, "payment_type": "POS",
		})
	case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/v0.1/me/refund/"):
		id := strings.TrimPrefix(r.URL.Path, "/v0.1/me/refund/")
		if id != s.txnID || s.final != "SUCCESSFUL" {
			reply(404, map[string]any{"error_code": "NOT_FOUND"})
			return
		}
		s.refunds = append(s.refunds, id)
		s.history = append(s.history, "REFUND "+id) // a separate entry; the original stays SUCCESSFUL
		w.WriteHeader(204)
	default:
		reply(404, map[string]any{"error_code": "NOT_FOUND"})
	}
}

// httpSumUp is the sumupAPI the real plugin gets from the host, over HTTP.
type httpSumUp struct{ base string }

func (h httpSumUp) call(method, path string, body []byte) ([]byte, int, bool) {
	req, err := http.NewRequest(method, h.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, false
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, false
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return b, resp.StatusCode, true
}

func contractWorld(t *testing.T) (*sandboxServer, *fakeClock, memKV, func(req string) *readerCharger) {
	t.Helper()
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s := &sandboxServer{clk: clk}
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	t.Cleanup(srv.Close)
	kv := memKV{}
	mk := func(req string) *readerCharger {
		return &readerCharger{
			api: httpSumUp{srv.URL}, clk: clk, kv: kv,
			affiliateKey: "sandbox-key-not-real", merchantCode: cMerchant,
			currency: "gbp", readerID: cReader, requestID: req, amount: 100,
		}
	}
	return s, clk, kv, mk
}

func noProblems(t *testing.T, s *sandboxServer) {
	t.Helper()
	if len(s.problems) > 0 {
		t.Fatalf("SumUp would reject the plugin's requests: %v", s.problems)
	}
}

func countReq(s *sandboxServer, method, suffix string) int {
	n := 0
	for _, r := range s.requests {
		if r.method == method && strings.Contains(r.path, suffix) {
			n++
		}
	}
	return n
}

func TestContractCheckoutBodyShape(t *testing.T) {
	s, clk, _, mk := contractWorld(t)
	s.tapAt = 2 * time.Second
	out := runReader(t, mk("att:0:ab"), clk)
	noProblems(t, s)
	if !out.approved {
		t.Fatalf("want approved, got %+v", out)
	}
	first := s.requests[0]
	if first.method != "POST" || first.path != "/v0.1/merchants/MTESTMERCH/readers/rdr_TEST/checkout" {
		t.Fatalf("first request = %s %s", first.method, first.path)
	}
	var got map[string]any
	if err := json.Unmarshal(first.body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("top-level keys = %v, want exactly total_amount and affiliate", got)
	}
	ta := got["total_amount"].(map[string]any)
	if ta["currency"] != "GBP" || ta["minor_unit"] != float64(2) || ta["value"] != float64(100) {
		t.Fatalf("total_amount = %v, want GBP / 2 / 100 (minor units, upper-case currency)", ta)
	}
	aff := got["affiliate"].(map[string]any)
	if aff["app_id"] != "com.universaltill.pos" || aff["key"] != "sandbox-key-not-real" ||
		aff["foreign_transaction_id"] != "ut-att:0:ab-1700000000" {
		t.Fatalf("affiliate = %v", aff)
	}
}

// The sandbox answered 404 for ~15 s (checkout pending), then 200.
func TestContractLookup404ThenSuccessful(t *testing.T) {
	s, clk, _, mk := contractWorld(t)
	s.tapAt = 12 * time.Second
	out := runReader(t, mk("att:0:ab"), clk)
	noProblems(t, s)
	if !out.approved || out.authCode == "" {
		t.Fatalf("want approved with the transaction id, got %+v", out)
	}
	if n := countReq(s, "GET", "/transactions?"); n < 6 {
		t.Fatalf("only %d lookups: the 404-means-not-yet answers were not retried", n)
	}
	for _, r := range s.requests {
		if strings.Contains(r.path, "/transactions?") && !strings.Contains(r.path, "foreign_transaction_id=") && !strings.Contains(r.path, "client_transaction_id=") {
			t.Fatalf("lookup without an id filter: %s", r.path)
		}
	}
	if countReq(s, "POST", "/terminate") != 0 {
		t.Fatal("terminated a checkout that was paid")
	}
}

// The lookup answer 404 NOT_FOUND means "not yet" (ok, no transaction), never
// a failed request: settling an earlier attempt depends on telling them apart.
func TestContractLookup404IsNotYetNotAnError(t *testing.T) {
	s, clk, _, mk := contractWorld(t)
	s.tapAt = 60 * time.Second // still pending
	c := mk("att:0:ab")
	c.start = clk.now()
	if _, _, ok := c.api.call("POST", c.readerPath("checkout"), []byte(`{"total_amount":{"currency":"GBP","minor_unit":2,"value":100},"affiliate":{"app_id":"a","key":"k","foreign_transaction_id":"ut-x"}}`)); !ok {
		t.Fatal("setup checkout failed")
	}
	tx, ok := c.lookup("ut-x")
	if tx != nil || !ok {
		t.Fatalf("lookup of a pending checkout = (%v, %v), want (nil, true)", tx, ok)
	}
}

// The recorded reply was written down as {checkout_id, client_transaction_id}
// without a data wrapper; the docs show {data:{client_transaction_id}}.
// Either way a paid sale must be approved.
func TestContractFlatCheckoutReplyStillSettles(t *testing.T) {
	s, clk, _, mk := contractWorld(t)
	s.flatReply = true
	s.tapAt = 2 * time.Second
	out := runReader(t, mk("att:0:ab"), clk)
	noProblems(t, s)
	if !out.approved {
		t.Fatalf("flat reply: want approved, got %+v (requests %d)", out, len(s.requests))
	}
	if countReq(s, "POST", "/terminate") != 0 {
		t.Fatal("terminated a checkout that was paid")
	}
}

// Decline: the sandbox showed FAILED only after ~40 s; the till must give up
// at its own deadline, terminate, and never approve.
func TestContractDeclineNeverApproves(t *testing.T) {
	for name, failedAt := range map[string]time.Duration{"quick FAILED": 3 * time.Second, "slow decline (sandbox ~40 s; the plugin terminates first)": 40 * time.Second} {
		t.Run(name, func(t *testing.T) {
			s, clk, _, mk := contractWorld(t)
			s.tapAt, s.declines, s.failedAt = 1*time.Second, true, failedAt
			out := runReader(t, mk("att:0:ab"), clk)
			noProblems(t, s)
			if out.approved {
				t.Fatalf("a declined card was approved: %+v", out)
			}
			if out.reason != "failed" && out.reason != "reader_timeout" {
				t.Fatalf("reason = %q", out.reason)
			}
			if s.final != "FAILED" {
				t.Fatalf("server final = %q", s.final)
			}
		})
	}
}

// Nobody taps: the plugin terminates (202), the lookup then shows FAILED,
// and the sale is declined as a timeout.
func TestContractTerminateEndsFailed(t *testing.T) {
	s, clk, _, mk := contractWorld(t)
	out := runReader(t, mk("att:0:ab"), clk)
	noProblems(t, s)
	if out.approved || out.reason != "reader_timeout" {
		t.Fatalf("got %+v, want declined reader_timeout", out)
	}
	if n := countReq(s, "POST", "/terminate"); n != 1 {
		t.Fatalf("terminate sent %d times, want 1", n)
	}
	if s.final != "FAILED" {
		t.Fatalf("server final = %q, want FAILED after terminate", s.final)
	}
}

// READER_BUSY: a second checkout while one is pending answers 422; it must
// be a definite refusal that starts nothing and leaves no attempt to settle.
func TestContractReaderBusyIsRefusal(t *testing.T) {
	s, clk, kv, mk := contractWorld(t)
	s.pending, s.startedAt = true, clk.now() // someone else's checkout is on the reader
	out := runReader(t, mk("att:0:ab"), clk)
	noProblems(t, s)
	if out.approved {
		t.Fatal("approved while the reader was busy")
	}
	if out.reason != "There is a pending checkout for the device." && out.reason != "reader_start_failed" {
		t.Fatalf("reason = %q", out.reason)
	}
	if rec, found := findAttempt(kv, "att:0:ab"); found && rec.Ref != "" {
		t.Fatalf("a refused start left an attempt behind: %+v", rec)
	}
	if countReq(s, "POST", "/terminate") != 0 {
		t.Fatal("terminated the other checkout on a definite 422")
	}
}

// Refund: 204, a SEPARATE refund entry, the original keeps SUCCESSFUL, so
// the plugin must trust the 204 and never read refund state from the
// original's status.
func TestContractRefundIsSeparateEntry(t *testing.T) {
	s, clk, _, mk := contractWorld(t)
	s.tapAt = 2 * time.Second
	c := mk("att:0:ab")
	out := runReader(t, c, clk)
	if !out.approved {
		t.Fatalf("setup: %+v", out)
	}
	done, tooLarge, reason := refundOne(c.api, out.authCode, 100)
	if !done || tooLarge || reason != "" {
		t.Fatalf("refund = %v %v %q, want done", done, tooLarge, reason)
	}
	if len(s.refunds) != 1 || s.refunds[0] != out.authCode || len(s.history) != 1 {
		t.Fatalf("server refunds %v history %v", s.refunds, s.history)
	}
	last := s.requests[len(s.requests)-1]
	if last.method != "POST" || last.path != "/v0.1/me/refund/"+out.authCode {
		t.Fatalf("refund request = %s %s", last.method, last.path)
	}
	// {"amount": <major units>} is SumUp's documented partial-refund body; the
	// sandbox record only proves {} (full refund) answers 204, so the amount
	// form is documented, not sandbox-verified (ut-docs#6561).
	var body map[string]any
	if err := json.Unmarshal(last.body, &body); err != nil || body["amount"] != float64(1) {
		t.Fatalf("refund body %s: want major-unit amount 1", last.body)
	}
	// The original, read again, is still SUCCESSFUL: not a signal of refund state.
	b, code, _ := c.api.call("GET", "/v2.1/merchants/"+cMerchant+"/transactions?foreign_transaction_id=ut-att%3A0%3Aab-1700000000", nil)
	if code != 200 || firstTransaction(b).Status != "SUCCESSFUL" {
		t.Fatalf("original after refund: %d %s", code, b)
	}
}

func TestContractRefundOfUnknownTransactionDeclines(t *testing.T) {
	_, _, _, mk := contractWorld(t)
	c := mk("att:0:ab")
	done, tooLarge, reason := refundOne(c.api, "TNOPE", 100)
	if done || tooLarge || reason != "NOT_FOUND" {
		t.Fatalf("got %v %v %q, want declined NOT_FOUND", done, tooLarge, reason)
	}
}
