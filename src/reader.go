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
// is approved, never dropped.
//
// SumUp's terminate (developer.sumup.com, "Terminate a Reader Checkout")
// answers 202 "dispatched", is asynchronous, has no confirmation, and only
// acts while the reader waits for a card or PIN; the device has up to 60 s
// to start an accepted checkout. So a 202 proves nothing: unless SumUp
// shows a FAILED/CANCELLED transaction, a checkout with no transaction is
// declined as outcome_unknown and stays on record under the till's request
// id. The till re-sends that SAME id when the cashier taps Pay again on the
// same basket (ut-docs#1762), and the retry settles the old checkout before
// it starts another: paid -> approve it; failed/cancelled -> fresh
// checkout; no transaction -> fresh only once settleWindow has passed since
// it started AND the reader reports itself ONLINE and IDLE, else decline
// again (outcome_unknown) without starting anything.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	pollBudget   = 18 * time.Second // customer's time to tap
	hardDeadline = 24 * time.Second // last call starts by now; host gate is 30 s, and a slow call or cold start eats the rest
	pollInterval = time.Second
	// settleWindow: an earlier checkout with no transaction this long after
	// it started, on an idle reader, never read a card (60 s device start
	// window + margin).
	settleWindow = 90 * time.Second
	// retryStartBudget: past this, settling the earlier attempt left too
	// little of the gate for a fresh checkout; decline and let the next tap
	// start clean.
	retryStartBudget = 6 * time.Second
)

// defaultAppID is Universal Till's application identifier (App ID / Bundle
// ID) as registered with the affiliate key; SumUp requires app_id together
// with key in the checkout's affiliate object. Not secret.
const defaultAppID = "com.universaltill.pos"

// foreignPrefix marks a stored attempt reference that is a foreign
// transaction id (a checkout whose start reply was lost, so SumUp's
// client_transaction_id was never seen) rather than a client_transaction_id.
const foreignPrefix = "ut-"

// sumupAPI performs one SumUp request: body, HTTP status, and ok=false only
// on a transport failure.
type sumupAPI interface {
	call(method, path string, body []byte) (respBody []byte, status int, ok bool)
}

type clock interface {
	now() time.Time
	sleep(time.Duration)
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
	appID        string // affiliate app_id; "" = defaultAppID
	merchantCode string
	currency     string
	readerID     string
	requestID    string // the till's authorize event id; "" on an older till
	amount       int64
	start        time.Time
}

func (c *readerCharger) elapsed() time.Duration { return c.clk.now().Sub(c.start) }

func (c *readerCharger) readerPath(action string) string {
	return fmt.Sprintf("/v0.1/merchants/%s/readers/%s/%s", c.merchantCode, c.readerID, action)
}

func refusedReader(reason string) readerOutcome { return readerOutcome{reason: reason} }

func paidReader(t *transactionPollEntry) readerOutcome {
	return readerOutcome{approved: true, authCode: t.ID, tip: parseTipAmountMinor(t.TipAmount)}
}

func isFailed(t *transactionPollEntry) bool {
	return t != nil && (t.Status == "FAILED" || t.Status == "CANCELLED")
}

// run drives one authorize on the paired reader.
//
// Verified 2026-11-17 in the SumUp sandbox (Virtual Solo; no physical reader
// tested): the checkout body, flat total_amount {currency, minor_unit, value}
// plus affiliate {app_id, key, foreign_transaction_id}, is accepted with HTTP
// 201. The create-checkout reference (developer.sumup.com/api/readers/
// create-checkout) is authoritative; the affiliate-keys doc page still shows
// an outdated {checkout:{amount}} wrapper. A lookup by foreign_transaction_id
// answers 404 NOT_FOUND while the checkout is pending, then 200 with the
// status. A decline and a terminate both end FAILED with no reason; a second
// checkout while one is pending answers 422 READER_BUSY.
//
// NOT DONE: no tipping configuration is sent — whether the reader prompts
// for a tip depends on the merchant's own SumUp settings; see README "Tips".
func (c *readerCharger) run() readerOutcome {
	// 0. A retry of the same tender: settle the earlier checkout first. A
	// late tap that went through is this sale's payment.
	if rec, found := findAttempt(c.kv, c.requestID); found && rec.Ref != "" {
		if out, done := c.settleEarlier(rec); done {
			return out
		}
		putAttempt(c.kv, attemptEntry{ID: c.requestID})
		if c.elapsed() > retryStartBudget {
			return refusedReader("previous_attempt_settled")
		}
	}

	if c.affiliateKey == "" {
		return refusedReader("no_affiliate_key")
	}

	// 1. Start the checkout on the reader. foreign_transaction_id (ours,
	// unique per start) lets a checkout whose reply we never saw be found
	// again through the Transactions API.
	startedAt := c.clk.now().Unix()
	foreignID := ""
	if c.requestID != "" {
		foreignID = fmt.Sprintf("%s%s-%d", foreignPrefix, c.requestID, startedAt)
	}
	appID := c.appID
	if appID == "" {
		appID = defaultAppID
	}
	affiliate := map[string]any{"app_id": appID, "key": c.affiliateKey}
	if foreignID != "" {
		affiliate["foreign_transaction_id"] = foreignID
	}
	reqBody, _ := json.Marshal(map[string]any{
		"total_amount": map[string]any{
			"currency":   strings.ToUpper(c.currency),
			"minor_unit": 2,
			"value":      c.amount, // already minor units, no conversion needed
		},
		"affiliate": affiliate,
	})
	// Record the foreign id BEFORE the call: the host may kill the plugin at
	// 30 s while SumUp has already taken the checkout and the reply hangs;
	// the re-tap of Pay must then find and settle it, not start a second one.
	if foreignID != "" {
		putAttempt(c.kv, attemptEntry{ID: c.requestID, Ref: foreignID, At: startedAt})
	}
	body, status, ok := c.api.call("POST", c.readerPath("checkout"), reqBody)
	var checkout struct {
		Data struct {
			ClientTransactionID string `json:"client_transaction_id"`
		} `json:"data"`
		// The 2026-11-17 sandbox note records the reply flat
		// ({checkout_id, client_transaction_id}); the docs wrap it in data.
		// Accept both: a wrong guess must not turn a started checkout into a
		// "lost reply" decline.
		FlatClientTransactionID string `json:"client_transaction_id"`
	}
	if ok {
		_ = json.Unmarshal(body, &checkout)
	}
	clientTxn := firstNonEmpty(checkout.Data.ClientTransactionID, checkout.FlatClientTransactionID)
	// Only a 4xx is a definite refusal. Any other non-2xx (1xx, 3xx such as a
	// stopped redirect, 5xx, out-of-range) proves nothing about the reader.
	definiteRefusal := ok && status >= 400 && status < 500
	is2xx := status >= 200 && status < 300
	if !ok || (!definiteRefusal && !is2xx) || (is2xx && clientTxn == "") {
		// No usable reply (transport failure, any non-2xx that is not a
		// 4xx, a 2xx without an id): the request may still have reached the
		// reader, which would prompt for a sale the till declines. Stop it
		// and look the sale up by our foreign id; a tap that landed first is
		// this sale's payment.
		// Nothing starts after hardDeadline (the host's gate is 30 s).
		if c.elapsed()+pollInterval >= hardDeadline {
			return refusedReader("network_error")
		}
		c.api.call("POST", c.readerPath("terminate"), nil)
		if foreignID != "" && c.elapsed()+pollInterval < hardDeadline {
			if t := c.awaitTransaction(foreignID); t != nil && t.Status == "SUCCESSFUL" {
				return paidReader(t)
			}
		}
		return refusedReader("network_error")
	}
	if definiteRefusal {
		// A definite 4xx refusal: nothing was started, so nothing to settle.
		putAttempt(c.kv, attemptEntry{ID: c.requestID})
		code, sumupType := readerStartDecline(status, body)
		logReaderRefusal(status, code, sumupType)
		return refusedReader(code)
	}
	putAttempt(c.kv, attemptEntry{ID: c.requestID, Ref: clientTxn, At: startedAt})

	// 2. Give the customer pollBudget to tap.
	for c.elapsed() < pollBudget {
		if t, ok := c.lookup(clientTxn); ok && t != nil {
			switch {
			case t.Status == "SUCCESSFUL":
				return paidReader(t)
			case isFailed(t):
				return refusedReader(strings.ToLower(t.Status))
			}
		}
		c.clk.sleep(pollInterval)
	}

	// 3. Out of time: stop the reader, then report what really happened.
	c.api.call("POST", c.readerPath("terminate"), nil)
	t := c.awaitTransaction(clientTxn)
	switch {
	case t != nil && t.Status == "SUCCESSFUL":
		return paidReader(t)
	case isFailed(t):
		return refusedReader("reader_timeout")
	}
	return refusedReader("outcome_unknown")
}

// settleEarlier resolves a retry's earlier checkout. done=false means it
// can't have charged the customer and a fresh checkout may start.
func (c *readerCharger) settleEarlier(rec attemptEntry) (readerOutcome, bool) {
	t, ok := c.lookup(rec.Ref)
	if !ok {
		return refusedReader("outcome_unknown"), true
	}
	switch {
	case t == nil:
		started := time.Unix(rec.At, 0)
		if rec.At > 0 && c.clk.now().Sub(started) >= settleWindow && c.readerIdle() {
			return readerOutcome{}, false
		}
		// Too early to tell: make sure the reader is stopped, start nothing.
		c.api.call("POST", c.readerPath("terminate"), nil)
		return refusedReader("outcome_unknown"), true
	case t.Status == "SUCCESSFUL":
		return paidReader(t), true
	case isFailed(t):
		return readerOutcome{}, false
	}
	// PENDING: the card is being read; stop the reader and wait for it.
	c.api.call("POST", c.readerPath("terminate"), nil)
	t = c.awaitTransaction(rec.Ref)
	switch {
	case t != nil && t.Status == "SUCCESSFUL":
		return paidReader(t), true
	case isFailed(t):
		return readerOutcome{}, false
	}
	return refusedReader("outcome_unknown"), true
}

// awaitTransaction re-checks the checkout until it has a final status or
// hardDeadline comes; the last transaction seen (or nil) is returned.
func (c *readerCharger) awaitTransaction(clientTxn string) *transactionPollEntry {
	var last *transactionPollEntry
	for {
		if t, ok := c.lookup(clientTxn); ok && t != nil {
			last = t
			if t.Status == "SUCCESSFUL" || isFailed(t) {
				return t
			}
		}
		// Never start a call that could still be running when the host's
		// gate closes: a verdict the till never receives is lost.
		if c.elapsed()+pollInterval >= hardDeadline {
			return last
		}
		c.clk.sleep(pollInterval)
	}
}

// lookup polls the Transactions endpoint filtered by client_transaction_id
// (or foreign_transaction_id for a reference starting foreignPrefix)
// (SumUp has no lookup-by-id for a reader checkout): (nil, true) when SumUp
// has none — documented as a 404, an empty list is treated the same —
// ok=false when the request failed. The envelope shape and tip_amount's
// type are NOT confirmed against a live account — see firstTransaction /
// parseTipAmountMinor in convert.go, which degrade rather than fail so a
// wrong guess never turns a real SUCCESSFUL payment into a decline.
func (c *readerCharger) lookup(clientTxn string) (*transactionPollEntry, bool) {
	param := "client_transaction_id=" + clientTxn
	if strings.HasPrefix(clientTxn, foreignPrefix) {
		param = "foreign_transaction_id=" + url.QueryEscape(clientTxn)
	}
	path := fmt.Sprintf("/v2.1/merchants/%s/transactions?%s", c.merchantCode, param)
	body, code, ok := c.api.call("GET", path, nil)
	switch {
	case !ok:
		return nil, false
	case code == 404:
		return nil, true
	case code < 200 || code >= 300:
		return nil, false
	}
	return firstTransaction(body), true
}

// readerIdle reports whether the reader says it is ONLINE and IDLE (GET
// .../readers/{id}/status). Anything else, or no answer, is "not idle".
func (c *readerCharger) readerIdle() bool {
	status, state, code, ok := readerStatus(c.api, c.merchantCode, c.readerID)
	return ok && code >= 200 && code < 300 && status == "ONLINE" && state == "IDLE"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
