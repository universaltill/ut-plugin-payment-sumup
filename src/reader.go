package main

// The SumUp reader (card-present, Cloud API) authorize flow. No //go:build
// tag, like convert.go: it reaches SumUp, the clock and plugin storage only
// through the interfaces below, so `go test` drives it with fakes
// (reader_test.go) while main.go wires the real host functions.
//
// Why the deadlines (ut-docs#6304): universal-till gives a payment
// authorize call 30 s (paymentGateTimeout in internal/plugins/
// wasm_runtime.go), then kills the plugin and shows the sale unpaid. A
// reader still waiting for a card after that would take a late tap and
// charge a sale the till shows unpaid. So the flow gives the customer
// pollBudget, then terminates the reader checkout and re-checks the
// transaction until hardDeadline — all inside the gate, counted from the
// plugin's own start (a queue wait for a call slot counts against the
// host's 30 s too, hence the margin). A payment that went through anyway
// is approved, never dropped. If the outcome is still unknown the decline
// says so (outcome_unknown) and the checkout stays on record under the
// till's request id: the till re-sends that SAME id when the cashier taps
// Pay again on the same basket (ut-docs#1762), and the retry resolves that
// checkout before it would ever start a second one.
//
// SumUp's terminate is asynchronous, has no confirmation, and only works
// while the reader waits for a card or PIN (developer.sumup.com, "Terminate
// a Reader Checkout"). So: terminate accepted and no transaction appears
// within terminateGrace = nothing was charged; terminate refused, or a
// PENDING transaction at hardDeadline = outcome_unknown.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	pollBudget     = 18 * time.Second // customer's time to tap
	hardDeadline   = 24 * time.Second // last call starts by now; host gate is 30 s, and a slow call or cold start eats the rest
	terminateGrace = 4 * time.Second  // wait for a transaction after an accepted terminate
	staleAttempt   = 5 * time.Minute  // an earlier checkout with no transaction by now never read a card
	pollInterval   = time.Second
)

// sumupAPI performs one SumUp request: body, HTTP status, and ok=false only
// on a transport failure.
type sumupAPI interface {
	call(method, path string, body []byte) (respBody []byte, status int, ok bool)
}

type clock interface {
	now() time.Time
	sleep(time.Duration)
}

type kvStore interface {
	get(k string) string
	put(k string, v []byte)
}

// readerOutcome is the flow's verdict; main.go turns it into
// approveWithTip()/decline().
type readerOutcome struct {
	approved bool
	authCode string // SumUp transaction id when approved
	reason   string // decline code otherwise
	tip      int64  // minor units the customer added on the reader
}

type readerCharger struct {
	api          sumupAPI
	clk          clock
	kv           kvStore
	affiliateKey string
	merchantCode string
	currency     string
	readerID     string
	requestID    string // the till's authorize event id; "" on an older till
	amount       int64
	start        time.Time
}

type readerAttempt struct {
	ClientTxn string `json:"client_txn"`
	StartedAt int64  `json:"started_at"` // unix seconds
}

// Settled states of one reader checkout.
const (
	txnPaid      = "paid"
	txnCancelled = "cancelled" // failed, cancelled, or never started: not charged
	txnUnknown   = "unknown"
)

func (c *readerCharger) elapsed() time.Duration { return c.clk.now().Sub(c.start) }

func (c *readerCharger) attemptKey() string { return "attempt:" + c.requestID }

func (c *readerCharger) saveAttempt(clientTxn string) {
	if c.requestID == "" {
		return
	}
	rec := readerAttempt{ClientTxn: clientTxn}
	if clientTxn != "" {
		rec.StartedAt = c.clk.now().Unix()
	}
	b, _ := json.Marshal(rec)
	c.kv.put(c.attemptKey(), b)
}

func (c *readerCharger) readerPath(action string) string {
	return fmt.Sprintf("/v0.1/merchants/%s/readers/%s/%s", c.merchantCode, c.readerID, action)
}

func refusedReader(reason string) readerOutcome { return readerOutcome{reason: reason} }

// run drives one authorize on the paired reader.
//
// NEEDS SANDBOX VERIFICATION (unchanged from v1.2): the affiliate-key
// placement in the checkout body (a top-level "affiliate" object) follows
// SumUp's Cloud API docs but was not confirmed against a live sandbox.
//
// NOT DONE: no tipping configuration is sent — whether the reader prompts
// for a tip depends on the merchant's own SumUp settings; see README "Tips".
func (c *readerCharger) run() readerOutcome {
	// 0. A retry of the same tender: settle the earlier checkout first. A
	// late tap that went through is this sale's payment.
	if c.requestID != "" {
		var rec readerAttempt
		_ = json.Unmarshal([]byte(c.kv.get(c.attemptKey())), &rec)
		if rec.ClientTxn != "" {
			// SumUp's terminate is refused once the reader is idle again, so
			// an old checkout that never produced a transaction would read
			// as "unknown" on every retry. Past staleAttempt it never read a
			// card: nothing was charged.
			stale := rec.StartedAt > 0 && c.clk.now().Sub(time.Unix(rec.StartedAt, 0)) >= staleAttempt
			state, id, tip := c.settle(rec.ClientTxn, stale)
			switch state {
			case txnPaid:
				return readerOutcome{approved: true, authCode: id, tip: tip}
			case txnCancelled:
				c.saveAttempt("")
			default:
				return refusedReader("outcome_unknown")
			}
		}
	}

	if c.affiliateKey == "" {
		return refusedReader("no_affiliate_key")
	}

	// 1. Start the checkout on the reader.
	reqBody, _ := json.Marshal(map[string]any{
		"total_amount": map[string]any{
			"currency":   strings.ToUpper(c.currency),
			"minor_unit": 2,
			"value":      c.amount, // already minor units, no conversion needed
		},
		"affiliate": map[string]any{
			"key": c.affiliateKey,
		},
	})
	body, status, ok := c.api.call("POST", c.readerPath("checkout"), reqBody)
	if !ok {
		// The request may have reached the reader and the reply been lost:
		// the reader would then prompt for a sale the till declines.
		c.api.call("POST", c.readerPath("terminate"), nil)
		return refusedReader("network_error")
	}
	var checkout struct {
		Data struct {
			ClientTransactionID string `json:"client_transaction_id"`
		} `json:"data"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &checkout)
	clientTxn := checkout.Data.ClientTransactionID
	if status < 200 || status >= 300 || clientTxn == "" {
		return refusedReader(firstNonEmpty(checkout.Message, "reader_start_failed"))
	}
	c.saveAttempt(clientTxn)

	// 2. Give the customer pollBudget to tap.
	for c.elapsed() < pollBudget {
		if t, ok := c.lookup(clientTxn); ok && t != nil {
			switch t.Status {
			case "SUCCESSFUL":
				return readerOutcome{approved: true, authCode: t.ID, tip: parseTipAmountMinor(t.TipAmount)}
			case "FAILED", "CANCELLED":
				return refusedReader(strings.ToLower(t.Status))
			}
		}
		c.clk.sleep(pollInterval)
	}

	// 3. Out of time: stop the reader, then report what really happened.
	state, id, tip := c.settle(clientTxn, false)
	switch state {
	case txnPaid:
		return readerOutcome{approved: true, authCode: id, tip: tip}
	case txnCancelled:
		// Settled as not charged: forget it, so the cashier's retry starts
		// a fresh checkout instead of re-settling this one.
		c.saveAttempt("")
		return refusedReader("reader_timeout")
	}
	return refusedReader("outcome_unknown")
}

// lookup polls the Transactions list filtered by client_transaction_id
// (SumUp has no lookup-by-id for a reader checkout). The envelope shape and
// tip_amount's type are NOT confirmed against a live account — see
// firstTransaction / parseTipAmountMinor in convert.go, which degrade
// rather than fail so a wrong guess never turns a real SUCCESSFUL payment
// into a decline.
//
// lookup fetches the checkout's transaction: (nil, true) when SumUp has
// none yet, ok=false when the request itself failed.
func (c *readerCharger) lookup(clientTxn string) (*transactionPollEntry, bool) {
	path := fmt.Sprintf("/v2.1/merchants/%s/transactions?client_transaction_id=%s", c.merchantCode, clientTxn)
	body, code, ok := c.api.call("GET", path, nil)
	if !ok || code < 200 || code >= 300 {
		return nil, false
	}
	return firstTransaction(body), true
}

func settled(t *transactionPollEntry) (state, id string, tip int64) {
	switch t.Status {
	case "SUCCESSFUL":
		return txnPaid, t.ID, parseTipAmountMinor(t.TipAmount)
	case "FAILED", "CANCELLED":
		return txnCancelled, t.ID, 0
	}
	return txnUnknown, "", 0
}

// settle makes sure the checkout can no longer charge the customer, or
// finds out it already did: terminate it, then re-check until hardDeadline.
// stale: the checkout started long ago (see staleAttempt), so no
// transaction at all means no card was read.
func (c *readerCharger) settle(clientTxn string, stale bool) (state, id string, tip int64) {
	t, ok := c.lookup(clientTxn)
	if ok && t != nil {
		if s, id, tip := settled(t); s != txnUnknown {
			return s, id, tip
		}
	}
	if ok && t == nil && stale {
		return txnCancelled, "", 0
	}
	_, code, ok := c.api.call("POST", c.readerPath("terminate"), nil)
	accepted := ok && code >= 200 && code < 300
	terminatedAt := c.elapsed()
	for {
		t, ok := c.lookup(clientTxn)
		if ok && t != nil {
			if s, id, tip := settled(t); s != txnUnknown {
				return s, id, tip
			}
		}
		// Terminate accepted and still no transaction: the card was never
		// read, so nothing can be charged.
		if accepted && ok && t == nil && c.elapsed()-terminatedAt >= terminateGrace {
			return txnCancelled, "", 0
		}
		// Never start a call that could still be running when the host's
		// gate closes: a verdict the till never receives is lost.
		if c.elapsed()+pollInterval >= hardDeadline {
			return txnUnknown, "", 0
		}
		c.clk.sleep(pollInterval)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
