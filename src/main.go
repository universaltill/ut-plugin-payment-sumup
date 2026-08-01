//go:build wasip1

// SumUp card payments — a WASI command (GOOS=wasip1 GOARCH=wasm) the till
// runs in-process for every payment.sumup.authorize event. Authorization is
// BLOCKING and runs BEFORE the sale completes: exit 0 = approved (tender
// proceeds), non-zero = declined (basket intact).
//
// Two modes, chosen by settings:
//   - READER (card-present) when `sumup_reader_id` is set: drives a paired
//     SumUp Solo/Air/3G reader via SumUp's Cloud API — this is SumUp's actual
//     product (unlike Stripe, SumUp has no synchronous online-charge API a
//     till can confirm within one blocking call; card-present via a physical
//     reader is the only real production path). Starts a checkout on the
//     reader, then polls the Transactions API for a terminal status, same
//     shape as the Stripe plugin's reader-polling loop. A tip the customer
//     selects on the reader itself comes back on that same poll; this
//     plugin reports it on the approve response's `tip_amount` field
//     (integer minor units) so the till can apply it to the payment even
//     though the original tender request carried no tip — see
//     README "Tips" for the caveat on this field's shape.
//   - DEMO (no reader configured): deterministic outcomes, no real API call
//     — same "amount ends .13 declines / .99 times out / else approves"
//     contract as ut-plugin-payment-demo. Not a real payment path (see
//     README "Design note" for why SumUp has no equivalent to Stripe's
//     online-charge fallback).
//
// Config (plugin settings): `sumup_api_key` (merchant's own SumUp API key,
// created in the SumUp developer portal), `sumup_merchant_code`,
// `sumup_affiliate_key` (Universal Till's integrator key — see README),
// `currency` (ISO, default eur), and optional `sumup_reader_id` (paired
// reader id, register-scoped).
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unsafe"
)

//go:wasmimport ut log_write
func logWrite(ptr, n uint32)

//go:wasmimport ut settings_get
func settingsGet(kPtr, kLen, dstPtr, dstCap uint32) int32

//go:wasmimport ut http_request
func httpRequest(rPtr, rLen, dstPtr, dstCap uint32) int32

//go:wasmimport ut storage_set
func storageSet(kPtr, kLen, vPtr, vLen uint32) int32

//go:wasmimport ut storage_get
func storageGet(kPtr, kLen, dstPtr, dstCap uint32) int32

const apiBase = "https://api.sumup.com"

func ptrOf(b []byte) (uint32, uint32) {
	if len(b) == 0 {
		return 0, 0
	}
	return uint32(uintptr(unsafe.Pointer(&b[0]))), uint32(len(b))
}

func logf(format string, args ...any) {
	msg := []byte(fmt.Sprintf(format, args...))
	p, n := ptrOf(msg)
	logWrite(p, n)
}

// setting reads one of the plugin's own settings. Grows the buffer once if the
// host reports a longer value than fits (writeGuest returns the full length).
func setting(key string) string {
	kp, kl := ptrOf([]byte(key))
	buf := make([]byte, 4096)
	bp, bc := ptrOf(buf)
	n := settingsGet(kp, kl, bp, bc)
	if n < 0 {
		return ""
	}
	if int(n) > len(buf) {
		buf = make([]byte, n)
		bp, bc = ptrOf(buf)
		n = settingsGet(kp, kl, bp, bc)
		if n < 0 || int(n) > len(buf) {
			return ""
		}
	}
	return string(buf[:n])
}

func storagePut(key string, v []byte) {
	kp, kl := ptrOf([]byte(key))
	vp, vl := ptrOf(v)
	storageSet(kp, kl, vp, vl)
}

func storageRead(key string) string {
	kp, kl := ptrOf([]byte(key))
	buf := make([]byte, 4096)
	bp, bc := ptrOf(buf)
	n := storageGet(kp, kl, bp, bc)
	if n < 0 || int(n) > len(buf) {
		return ""
	}
	return string(buf[:n])
}

func saveTxn(v []byte) { storagePut("last_txn", v) }

// sumupCall performs one SumUp API request (JSON body, Bearer auth) and
// returns the decoded response body and HTTP status. ok is false only on a
// host/transport failure (a non-2xx HTTP status still returns ok=true with
// status/body set — the host function always surfaces the real response).
func sumupCall(method, path, apiKey string, jsonBody []byte) (body []byte, status int, ok bool) {
	headers := map[string]string{
		"Authorization": "Bearer " + apiKey,
		"Content-Type":  "application/json",
	}
	reqJSON, _ := json.Marshal(map[string]any{
		"method":   method,
		"url":      apiBase + path,
		"headers":  headers,
		"body_b64": base64.StdEncoding.EncodeToString(jsonBody),
	})
	rp, rl := ptrOf(reqJSON)
	respBuf := make([]byte, 64*1024)
	bp, bc := ptrOf(respBuf)
	code := httpRequest(rp, rl, bp, bc)
	if code < 0 || int(code) > len(respBuf) {
		return nil, 0, false
	}
	var httpResp struct {
		Status  int    `json:"status"`
		BodyB64 string `json:"body_b64"`
	}
	_ = json.Unmarshal(respBuf[:code], &httpResp)
	b, _ := base64.StdEncoding.DecodeString(httpResp.BodyB64)
	return b, httpResp.Status, true
}

const (
	approvedExit = 0
	declinedExit = 2 // non-zero exit -> the till declines the tender (basket kept)
)

func approve(amount int64, currency, authCode string) {
	approveWithTip(amount, currency, authCode, 0)
}

// approveWithTip is approve, but also reports a tip the reader captured
// from the customer (integer minor units, same convention as universal-till's
// own `tip` tender field). universal-till's completeTender reads this back
// off the authorize response and applies it to the payment — the customer
// only picks a tip on the reader itself, after the till already sent the
// charge amount, so there is no other way for the till to learn it.
// tipAmount is omitted from the JSON entirely when zero, so every other
// approve() caller (demo mode, refunds — neither has a real tip to report)
// is byte-for-byte unaffected.
func approveWithTip(amount int64, currency, authCode string, tipAmount int64) {
	fields := map[string]any{
		"provider": "sumup", "amount": amount, "currency": currency,
		"outcome": "approved", "auth_code": authCode,
	}
	if tipAmount > 0 {
		fields["tip_amount"] = tipAmount
	}
	result, _ := json.Marshal(fields)
	saveTxn(result)
	if tipAmount > 0 {
		logf("sumup: APPROVED %d %s (%s), tip %d", amount, currency, authCode, tipAmount)
	} else {
		logf("sumup: APPROVED %d %s (%s)", amount, currency, authCode)
	}
	_, _ = os.Stdout.Write(append(result, '\n'))
	os.Exit(approvedExit)
}

func decline(amount int64, currency, reason string) {
	result, _ := json.Marshal(map[string]any{
		"provider": "sumup", "amount": amount, "currency": currency,
		"outcome": "declined", "decline_code": reason,
	})
	saveTxn(result)
	logf("sumup: DECLINED %d %s (%s)", amount, currency, reason)
	_, _ = os.Stdout.Write(append(result, '\n'))
	os.Exit(declinedExit)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func main() {
	raw, _ := io.ReadAll(os.Stdin)
	var ev struct {
		Type    string `json:"type"`
		Payload struct {
			Amount         int64  `json:"amount"`
			SaleID         string `json:"sale_id"`
			OriginalSaleID string `json:"original_sale_id"`
			Currency       string `json:"currency"`
		} `json:"payload"`
	}
	_ = json.Unmarshal(raw, &ev)
	amount := ev.Payload.Amount

	// Post-sale settle notification: link the sale id to the transaction we
	// just authorized so a later refund can find it. Not blocking.
	if strings.HasSuffix(ev.Type, ".requested") {
		var last struct {
			AuthCode string `json:"auth_code"`
		}
		_ = json.Unmarshal([]byte(storageRead("last_txn")), &last)
		if ev.Payload.SaleID != "" && last.AuthCode != "" {
			storagePut("sale_txn:"+ev.Payload.SaleID, []byte(last.AuthCode))
			logf("sumup: sale %s settled with %s", ev.Payload.SaleID, last.AuthCode)
		}
		os.Exit(0)
	}

	currency := strings.TrimSpace(setting("currency"))
	if currency == "" {
		currency = "eur"
	}

	readerID := strings.TrimSpace(setting("sumup_reader_id"))

	// Provider refund (blocking, pre-return): send the money back on the
	// original transaction. Handled BEFORE the demo/reader authorize
	// branching below — a refund is never a "demo amount" decision, it
	// always needs to either really refund (reader/API configured) or
	// no-op-approve (nothing real was ever charged in demo mode).
	if strings.HasSuffix(ev.Type, ".refund") {
		if readerID == "" {
			// DEMO mode: no reader was ever configured, so the original
			// "charge" was never a real SumUp transaction — nothing to
			// refund against SumUp's API, and declining would incorrectly
			// block a return in a demo/test install.
			approve(amount, currency, "demo-refund")
		}
		apiKey := strings.TrimSpace(setting("sumup_api_key"))
		if apiKey == "" {
			decline(amount, currency, "no_api_key")
		}
		refundTxn(apiKey, ev.Payload.OriginalSaleID, amount, currency)
		return
	}

	// From here on this is a payment.sumup.authorize call.

	// DEMO mode: no reader configured, so there is no real production path
	// (see package doc comment) — deterministic outcome, no network call,
	// same contract as ut-plugin-payment-demo.
	if readerID == "" {
		switch amount % 100 {
		case 13:
			decline(amount, currency, "demo_declined")
		case 99:
			decline(amount, currency, "demo_timeout")
		default:
			approve(amount, currency, "demo-"+fmt.Sprintf("%d", amount))
		}
		return
	}

	apiKey := strings.TrimSpace(setting("sumup_api_key"))
	if apiKey == "" {
		decline(amount, currency, "no_api_key")
	}
	merchantCode := strings.TrimSpace(setting("sumup_merchant_code"))
	if merchantCode == "" {
		decline(amount, currency, "no_merchant_code")
	}
	affiliateKey := strings.TrimSpace(setting("sumup_affiliate_key"))
	readerCharge(apiKey, affiliateKey, merchantCode, currency, readerID, amount)
}

// readerCharge drives a paired SumUp reader via the Cloud API: start a
// checkout on the reader, then poll the Transactions API (by
// client_transaction_id) for a terminal status. Up to ~60s so a customer has
// time to tap/insert.
//
// NEEDS SANDBOX VERIFICATION: the affiliate-key placement in the reader
// checkout request body below (a top-level "affiliate" object) follows
// SumUp's published Cloud API docs description ("the Affiliate Key ... under
// the affiliate section") but was not confirmed against a live sandbox
// response — verify this shape against a real SumUp merchant account before
// relying on it in production.
//
// NOT DONE: this request sends no tipping configuration at all, so whether
// the reader actually prompts the customer for a tip depends entirely on
// tipping already being enabled in the merchant's own SumUp device/profile
// settings — this plugin does not (and, per SumUp's published Cloud API
// docs at write time, has no documented per-checkout field to) turn
// tipping on. If the reader was never configured to prompt, pollTransaction
// will simply never see a tip_amount and this feature silently does
// nothing, which is a real gap, not covered by the "Needs sandbox
// verification" caveat above — see README "Tips".
func readerCharge(apiKey, affiliateKey, merchantCode, currency, readerID string, amount int64) {
	if affiliateKey == "" {
		decline(amount, currency, "no_affiliate_key")
	}
	reqBody, _ := json.Marshal(map[string]any{
		"total_amount": map[string]any{
			"currency":   strings.ToUpper(currency),
			"minor_unit": 2,
			"value":      amount, // already minor units, no conversion needed
		},
		"affiliate": map[string]any{
			"key": affiliateKey,
		},
	})
	path := fmt.Sprintf("/v0.1/merchants/%s/readers/%s/checkout", merchantCode, readerID)
	body, status, ok := sumupCall("POST", path, apiKey, reqBody)
	if !ok {
		decline(amount, currency, "network_error")
	}
	var checkout struct {
		Data struct {
			ClientTransactionID string `json:"client_transaction_id"`
		} `json:"data"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &checkout)
	clientTxnID := checkout.Data.ClientTransactionID
	if status < 200 || status >= 300 || clientTxnID == "" {
		decline(amount, currency, firstNonEmpty(checkout.Message, "reader_start_failed"))
	}

	txnID, outcome, tipAmount := pollTransaction(apiKey, merchantCode, clientTxnID)
	switch outcome {
	case "SUCCESSFUL":
		approveWithTip(amount, currency, txnID, tipAmount)
	case "FAILED", "CANCELLED":
		decline(amount, currency, strings.ToLower(outcome))
	default:
		decline(amount, currency, "reader_timeout")
	}
}

// pollTransaction looks up a reader checkout's outcome, and any tip the
// customer selected on the reader, by client_transaction_id. SumUp's Cloud
// API delivers the real outcome via webhook/return_url; there is no
// dedicated single-transaction lookup-by-id endpoint, so — like the Stripe
// plugin's PaymentIntent poll — this polls the Transactions list endpoint
// filtered by client_transaction_id once a second. The list envelope shape
// (bare array vs an "items" wrapper) wasn't confirmed against a live sandbox
// response, so both are handled here.
//
// NEEDS SANDBOX VERIFICATION: tip_amount's exact presence/type in this
// endpoint's response wasn't confirmed against a live merchant account —
// assumed to be a decimal major-unit number (matching this API family's
// other amount fields, e.g. the Refund endpoint's `amount`), converted to
// minor units via majorToMinor. Deliberately decoded as json.RawMessage
// (never a fixed Go type) on the txn struct below: this field's shape is a
// GUESS, and a wrong guess must never break parsing THIS transaction's own
// id/status — that would turn a card the customer already successfully
// tapped into a false "reader_timeout" decline. parseTipAmountMinor (see
// convert.go) is where the guess is actually interpreted, independently,
// and it degrades to "no tip reported" for anything it doesn't recognize
// rather than erroring.
func pollTransaction(apiKey, merchantCode, clientTxnID string) (txnID, status string, tipAmount int64) {
	path := fmt.Sprintf("/v2.1/merchants/%s/transactions?client_transaction_id=%s", merchantCode, clientTxnID)
	for i := 0; i < 60; i++ {
		body, code, ok := sumupCall("GET", path, apiKey, nil)
		if ok && code >= 200 && code < 300 {
			if id, st, tip, found := parseTransactionPoll(body); found {
				return id, st, tip
			}
		}
		time.Sleep(time.Second)
	}
	return "", "TIMEOUT", 0
}

// refundTxn refunds (part of) the original sale's transaction. The
// transaction id was stored at settle time under sale_txn:<sale_id>; without
// it we must decline — refunding an unknown transaction is not possible.
func refundTxn(apiKey, originalSaleID string, amount int64, currency string) {
	txnID := strings.TrimSpace(storageRead("sale_txn:" + originalSaleID))
	if originalSaleID == "" || txnID == "" {
		decline(amount, currency, "unknown_original_transaction")
	}
	reqBody, _ := json.Marshal(map[string]any{"amount": minorToMajor(amount)})
	body, status, ok := sumupCall("POST", "/v0.1/me/refund/"+txnID, apiKey, reqBody)
	if !ok {
		decline(amount, currency, "network_error")
	}
	// The docs describe 204 No Content on success; treat any 2xx as success
	// rather than pinning to exactly 204, in case a future API version
	// starts returning a 200 with a body.
	if status >= 200 && status < 300 {
		logf("sumup: REFUNDED %d on %s", amount, txnID)
		approve(amount, currency, txnID)
	}
	var errResp struct {
		Message string `json:"message"`
		Error   string `json:"error_code"`
	}
	_ = json.Unmarshal(body, &errResp)
	decline(amount, currency, firstNonEmpty(errResp.Error, errResp.Message, "refund_failed"))
}
