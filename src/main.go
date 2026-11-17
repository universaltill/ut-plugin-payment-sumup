//go:build wasip1

// SumUp card payments — a WASI command (GOOS=wasip1 GOARCH=wasm) the till
// runs in-process for every payment.sumup.authorize event. Authorization is
// BLOCKING and runs BEFORE the sale completes: exit 0 = approved (tender
// proceeds), non-zero = declined (basket intact).
//
// Two modes, chosen by settings:
//   - READER (card-present) when `sumup_reader_id` is set: drives a paired
//     SumUp Solo reader via SumUp's Cloud API — this is SumUp's actual
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

//go:wasmimport ut blob_put_open
func blobPutOpen(nPtr, nLen uint32) int32

//go:wasmimport ut blob_write
func blobWrite(h int32, ptr, n uint32) int32

//go:wasmimport ut blob_commit
func blobCommit(h int32) int32

//go:wasmimport ut blob_get_open
func blobGetOpen(nPtr, nLen uint32) int32

//go:wasmimport ut blob_read
func blobRead(h int32, dstPtr, dstCap uint32) int32

const apiBase = "https://api.sumup.com"

func ptrOf(b []byte) (uint32, uint32) {
	if len(b) == 0 {
		return 0, 0
	}
	return uint32(uintptr(unsafe.Pointer(&b[0]))), uint32(len(b))
}

func init() {
	logHook = func(m string) {
		p, n := ptrOf([]byte(m))
		logWrite(p, n)
	}
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

// hostStorage is the raw plugin storage; put reports the host's code.
type hostStorage struct{}

func (hostStorage) put(key string, v []byte) error {
	kp, kl := ptrOf([]byte(key))
	vp, vl := ptrOf(v)
	if rc := storageSet(kp, kl, vp, vl); rc != 0 {
		return fmt.Errorf("storage_set %q: host code %d", key, rc)
	}
	return nil
}

func (hostStorage) get(key string) string {
	kp, kl := ptrOf([]byte(key))
	v, _ := readGrow(func(dst []byte) int32 {
		dp, dc := ptrOf(dst)
		return storageGet(kp, kl, dp, dc)
	})
	return v
}

// hostBlobs is blob:own (ADR-0121). Codes: -1 no such blob, -2 permission not
// granted, -4 bad name, -5 quota.
type hostBlobs struct{}

const maxBlobRead = 1 << 20

func (hostBlobs) put(name string, b []byte) error {
	np, nl := ptrOf([]byte(name))
	h := blobPutOpen(np, nl)
	if h < 0 {
		return fmt.Errorf("blob_put_open %q: host code %d", name, h)
	}
	if len(b) > 0 {
		bp, bl := ptrOf(b)
		if n := blobWrite(h, bp, bl); n != int32(len(b)) {
			return fmt.Errorf("blob_write %q: host code %d", name, n)
		}
	}
	if rc := blobCommit(h); rc != 0 {
		return fmt.Errorf("blob_commit %q: host code %d", name, rc)
	}
	return nil
}

func (hostBlobs) get(name string) ([]byte, bool, error) {
	np, nl := ptrOf([]byte(name))
	h := blobGetOpen(np, nl)
	if h == -1 {
		return nil, false, nil
	}
	if h < 0 {
		return nil, false, fmt.Errorf("blob_get_open %q: host code %d", name, h)
	}
	var out []byte
	buf := make([]byte, 16<<10)
	for {
		bp, bc := ptrOf(buf)
		n := blobRead(h, bp, bc)
		if n < 0 {
			return nil, false, fmt.Errorf("blob_read %q: host code %d", name, n)
		}
		if n == 0 {
			return out, true, nil // the host releases the handle at the end
		}
		out = append(out, buf[:n]...)
		if len(out) > maxBlobRead {
			return nil, false, fmt.Errorf("blob %q larger than %d bytes", name, maxBlobRead)
		}
	}
}

var (
	pluginKV    kvStore = overflowKV{raw: hostStorage{}, blobs: hostBlobs{}}
	pluginLinks         = linkStore{blobs: hostBlobs{}}
)

// persistResult is false for refund and settle events: only an authorize
// answer is recorded in last_txn. curEventID is the authorize event's id.
var (
	persistResult = true
	curEventID    string
)

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
	if persistResult {
		// transaction_ref: core stores it with the sale and hands it back on a
		// refund (ut-docs#6438); auth_code keeps its meaning.
		fields["transaction_ref"] = authCode
		saveAuthorizeResult(pluginKV, fields, &recentEntry{
			ID: curEventID, Ref: authCode, Amount: amount, Tip: tipAmount, At: time.Now().Unix(),
		})
	}
	result, _ := json.Marshal(fields)
	if tipAmount > 0 {
		logf("sumup: APPROVED %d %s (%s), tip %d", amount, currency, maskRef(authCode), tipAmount)
	} else {
		logf("sumup: APPROVED %d %s (%s)", amount, currency, maskRef(authCode))
	}
	_, _ = os.Stdout.Write(append(result, '\n'))
	os.Exit(approvedExit)
}

func decline(amount int64, currency, reason string) {
	fields := map[string]any{
		"provider": "sumup", "amount": amount, "currency": currency,
		"outcome": "declined", "decline_code": reason,
	}
	if persistResult {
		saveAuthorizeResult(pluginKV, fields, nil)
	}
	result, _ := json.Marshal(fields)
	logf("sumup: DECLINED %d %s (%s)", amount, currency, scrubRefs(reason))
	_, _ = os.Stdout.Write(append(result, '\n'))
	os.Exit(declinedExit)
}

// hostSumUp and realClock wire reader.go's flow to the host.
type hostSumUp struct{ apiKey string }

func (h hostSumUp) call(method, path string, body []byte) ([]byte, int, bool) {
	return sumupCall(method, path, h.apiKey, body)
}

type realClock struct{}

func (realClock) now() time.Time        { return time.Now() }
func (realClock) sleep(d time.Duration) { time.Sleep(d) }

func main() {
	start := time.Now() // reader.go's deadlines count from here (ut-docs#6304)
	raw, _ := io.ReadAll(os.Stdin)
	ev := parseEvent(raw)
	amount := ev.Payload.Amount
	curEventID = ev.ID

	// Post-sale settle notification: when core already holds the reference
	// (transaction_ref) nothing is stored; otherwise link the sale to its
	// approval in a link bucket (links.go, recent.go). Never blocks the sale.
	if strings.HasSuffix(ev.Type, ".requested") {
		settleSale(pluginKV, pluginLinks, ev.Payload, time.Now())
		os.Exit(0)
	}
	persistResult = !strings.HasSuffix(ev.Type, ".refund")

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
		refundTxn(apiKey, ev.Payload, currency)
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
		api: hostSumUp{apiKey: apiKey}, clk: realClock{}, kv: pluginKV,
		affiliateKey: strings.TrimSpace(setting("sumup_affiliate_key")),
		appID:        strings.TrimSpace(setting("sumup_app_id")),
		merchantCode: merchantCode, currency: currency, readerID: readerID,
		requestID: ev.ID, amount: amount, start: start,
	}
	if out := c.run(); out.approved {
		approveWithTip(amount, currency, out.authCode, out.tip)
	} else {
		decline(amount, currency, out.reason)
	}
}

// refundTxn refunds (part of) the original sale's transaction, found via
// core's original_payments, the link bucket or the legacy key (refund.go).
// Without a reference it declines: guessing a transaction would refund the
// wrong customer.
func refundTxn(apiKey string, p eventPayload, currency string) {
	id, reason := refundSale(hostSumUp{apiKey: apiKey}, pluginKV, pluginLinks, p)
	if reason != "" {
		decline(p.Amount, currency, reason)
	}
	approve(p.Amount, currency, id)
}
