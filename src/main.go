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
// `currency` (a picker; default "auto" = the shop's currency, never a
// silent eur for a UK shop), and optional `sumup_reader_id` (paired
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

//go:wasmimport ut view_query
func viewQuery(nPtr, nLen, aPtr, aLen, dstPtr, dstCap uint32) int32

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

// shopContext reads the shop's currency from the core view shop.context.v2
// (view:shop). Known is false when the view is unavailable (permission not
// granted, older till, ...): the caller then falls back, never guesses.
func shopContext() shopInfo {
	np, nl := ptrOf([]byte("shop.context.v2"))
	ap, al := ptrOf([]byte("{}"))
	buf := make([]byte, 8192)
	bp, bc := ptrOf(buf)
	n := viewQuery(np, nl, ap, al, bp, bc)
	if n >= 0 && int(n) > len(buf) {
		// Longer than the buffer: views are read-only, so re-query once
		// with the full length (same contract as setting()).
		buf = make([]byte, n)
		bp, bc = ptrOf(buf)
		n = viewQuery(np, nl, ap, al, bp, bc)
	}
	if n < 0 || int(n) > len(buf) {
		return shopInfo{}
	}
	code, dec, ok := parseShopContext(buf[:n])
	return shopInfo{Code: code, Decimals: dec, Known: ok}
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

// hostSumUp, realClock and pluginKV wire reader.go's flow to the host.
type hostSumUp struct{ apiKey string }

func (h hostSumUp) call(method, path string, body []byte) ([]byte, int, bool) {
	return sumupCall(method, path, h.apiKey, body)
}

type realClock struct{}

func (realClock) now() time.Time        { return time.Now() }
func (realClock) sleep(d time.Duration) { time.Sleep(d) }

type pluginKV struct{}

func (pluginKV) get(k string) string    { return storageRead(k) }
func (pluginKV) put(k string, v []byte) { storagePut(k, v) }

func main() {
	start := time.Now() // reader.go's deadlines count from here (ut-docs#6304)
	raw, _ := io.ReadAll(os.Stdin)
	var ev struct {
		ID      string `json:"id"`
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

	readerID := strings.TrimSpace(setting("sumup_reader_id"))

	// Currency: the saved choice (default "auto" = the shop's own currency
	// via shop.context.v2), checked against the sale's currency. In test
	// mode nothing is charged, so a failed check only falls back for
	// display; with a reader (a real charge) or a real refund it declines
	// rather than charge/refund in the wrong currency.
	savedCurrency := setting("currency")
	shop := shopContext()
	currency, currencyDecline := resolveCurrency(savedCurrency, ev.Payload.Currency, shop)
	if currencyDecline != "" {
		if readerID != "" {
			decline(amount, declinedCurrency(savedCurrency, ev.Payload.Currency, shop), currencyDecline)
		}
		currency = fallbackCurrency
	}

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
		if ok, code := demoAuthorize(amount); ok {
			approve(amount, currency, code)
		} else {
			decline(amount, currency, code)
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
	c := &readerCharger{
		api: hostSumUp{apiKey: apiKey}, clk: realClock{}, kv: pluginKV{},
		affiliateKey: strings.TrimSpace(setting("sumup_affiliate_key")),
		merchantCode: merchantCode, currency: currency, readerID: readerID,
		requestID: ev.ID, amount: amount, start: start,
	}
	if out := c.run(); out.approved {
		approveWithTip(amount, currency, out.authCode, out.tip)
	} else {
		decline(amount, currency, out.reason)
	}
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
