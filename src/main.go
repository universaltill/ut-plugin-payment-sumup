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
//     shape as the Stripe plugin's reader-polling loop.
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
	result, _ := json.Marshal(map[string]any{
		"provider": "sumup", "amount": amount, "currency": currency,
		"outcome": "approved", "auth_code": authCode,
	})
	saveTxn(result)
	logf("sumup: APPROVED %d %s (%s)", amount, currency, authCode)
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

// minorToMajor renders integer minor units as a decimal major-unit number
// for SumUp's Checkouts/Refund APIs (unlike the Reader Cloud API, these use
// major units — e.g. 1050 minor -> 10.50). Assumes a 2-decimal currency
// (true for EUR/GBP/USD, the only currencies this plugin targets today); a
// 0-decimal currency (CLP/COP/HUF) would need a currency-aware divisor if
// this plugin ever needs to support one.
func minorToMajor(amount int64) float64 {
	return float64(amount) / 100.0
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

	txnID, outcome := pollTransaction(apiKey, merchantCode, clientTxnID)
	switch outcome {
	case "SUCCESSFUL":
		approve(amount, currency, txnID)
	case "FAILED", "CANCELLED":
		decline(amount, currency, strings.ToLower(outcome))
	default:
		decline(amount, currency, "reader_timeout")
	}
}

// pollTransaction looks up a reader checkout's outcome by its
// client_transaction_id. SumUp's Cloud API delivers the real outcome via
// webhook/return_url; there is no dedicated single-transaction lookup-by-id
// endpoint, so — like the Stripe plugin's PaymentIntent poll — this polls
// the Transactions list endpoint filtered by client_transaction_id once a
// second. The list envelope shape (bare array vs an "items" wrapper) wasn't
// confirmed against a live sandbox response, so both are handled here.
func pollTransaction(apiKey, merchantCode, clientTxnID string) (txnID, status string) {
	type txn struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	path := fmt.Sprintf("/v2.1/merchants/%s/transactions?client_transaction_id=%s", merchantCode, clientTxnID)
	for i := 0; i < 60; i++ {
		body, status, ok := sumupCall("GET", path, apiKey, nil)
		if ok && status >= 200 && status < 300 {
			var wrapped struct {
				Items []txn `json:"items"`
			}
			var bare []txn
			var one txn
			var found *txn
			if err := json.Unmarshal(body, &wrapped); err == nil && len(wrapped.Items) > 0 {
				found = &wrapped.Items[0]
			} else if err := json.Unmarshal(body, &bare); err == nil && len(bare) > 0 {
				found = &bare[0]
			} else if err := json.Unmarshal(body, &one); err == nil && one.ID != "" {
				found = &one
			}
			if found != nil {
				switch found.Status {
				case "SUCCESSFUL", "FAILED", "CANCELLED":
					return found.ID, found.Status
				}
			}
		}
		time.Sleep(time.Second)
	}
	return "", "TIMEOUT"
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
