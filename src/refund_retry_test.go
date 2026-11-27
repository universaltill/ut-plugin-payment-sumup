package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// ledgerSumUp models POST /v0.1/me/refund/{txn}: 201 on success, 409 when the
// amount is above what is left (SumUp has no idempotency key on this call).
// loseReplies: the next N refunds are applied at SumUp but the plugin sees a
// transport failure.
type ledgerSumUp struct {
	captured    map[string]int64
	refunded    map[string]int64
	bodies      []string
	loseReplies int
}

func (l *ledgerSumUp) call(method, path string, body []byte) ([]byte, int, bool) {
	txn, ok := refundTxnFromPath(path)
	if method != "POST" || !ok {
		return []byte(`{"message":"unexpected"}`), 400, true
	}
	var req struct {
		Amount int64 `json:"amount"` // integer minor units; a float body fails to decode
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return []byte(`{"message":"amount must be an integer"}`), 400, true
	}
	l.bodies = append(l.bodies, string(body))
	amt := req.Amount
	if amt > l.captured[txn]-l.refunded[txn] {
		return []byte(`{"error_code":"CONFLICT"}`), 409, true
	}
	l.refunded[txn] += amt
	if l.loseReplies > 0 {
		l.loseReplies--
		return nil, 0, false
	}
	return nil, 201, true
}

// refundTxnFromPath parses /v1.0/merchants/MCODE/payments/{id}/refunds.
func refundTxnFromPath(path string) (string, bool) {
	const pre = "/v1.0/merchants/" + testMerchant + "/payments/"
	if !strings.HasPrefix(path, pre) || !strings.HasSuffix(path, "/refunds") {
		return "", false
	}
	return strings.TrimSuffix(strings.TrimPrefix(path, pre), "/refunds"), true
}

const testMerchant = "MCODE"

func newLedger(loseReplies int, captured map[string]int64) *ledgerSumUp {
	return &ledgerSumUp{captured: captured, refunded: map[string]int64{}, loseReplies: loseReplies}
}

func retryPayload(amount int64, legs ...origPayment) eventPayload {
	return eventPayload{Amount: amount, OriginalSaleID: "sale-1", EventID: "evt-1", OriginalPayments: legs}
}

// #6598: a partial refund whose reply is lost must not be applied twice.
func TestRefundLostReplyPartialRetryNeverRefundsTwice(t *testing.T) {
	api := newLedger(1, map[string]int64{"txn-1": 1250})
	p := retryPayload(500, origPayment{TransactionRef: "txn-1", Amount: 1250})
	kv, links := memKV{}, linkStore{blobs: newFakeBlobs()}
	_, r1 := refundSale(api, testMerchant, kv, links, p)
	_, r2 := refundSale(api, testMerchant, kv, links, p)
	if r1 != "network_error" {
		t.Fatalf("first attempt = %q, want network_error", r1)
	}
	if r2 == "" {
		t.Errorf("retry of an unconfirmed partial refund must not be approved blindly")
	}
	if api.refunded["txn-1"] != 500 {
		t.Fatalf("customer got back %d, want exactly 500 (bodies %v)", api.refunded["txn-1"], api.bodies)
	}
}

// #6598: a retry of the same event id never moves on to the other leg.
func TestRefundLostReplyRetryNeverMovesToOtherLeg(t *testing.T) {
	api := newLedger(1, map[string]int64{"txn-A": 2500, "txn-B": 2500})
	p := retryPayload(2500, origPayment{TransactionRef: "txn-A", Amount: 2500}, origPayment{TransactionRef: "txn-B", Amount: 2500})
	kv, links := memKV{}, linkStore{blobs: newFakeBlobs()}
	refundSale(api, testMerchant, kv, links, p)
	refundSale(api, testMerchant, kv, links, p)
	if got := api.refunded["txn-A"] + api.refunded["txn-B"]; got != 2500 {
		t.Fatalf("refunded %d in total for a 2500 refund (A=%d B=%d)", got, api.refunded["txn-A"], api.refunded["txn-B"])
	}
	if api.refunded["txn-B"] != 0 {
		t.Errorf("the other leg was refunded on a retry")
	}
}

// #6598: a full refund whose reply was lost is recordable on retry (SumUp now
// says "nothing left"; that is our own refund, so approve and record it).
func TestRefundLostReplyFullRetryIsRecorded(t *testing.T) {
	api := newLedger(1, map[string]int64{"txn-1": 1250})
	p := retryPayload(1250, origPayment{TransactionRef: "txn-1", Amount: 1250})
	kv, links := memKV{}, linkStore{blobs: newFakeBlobs()}
	_, r1 := refundSale(api, testMerchant, kv, links, p)
	id, r2 := refundSale(api, testMerchant, kv, links, p)
	if r1 != "network_error" || r2 != "" || id != "txn-1" {
		t.Fatalf("attempt1=%q attempt2=(%q,%q), want network_error then approve on txn-1", r1, id, r2)
	}
	if api.refunded["txn-1"] != 1250 {
		t.Errorf("refunded %d, want 1250", api.refunded["txn-1"])
	}
}

// #6598: a repeat of a refund that was already confirmed is answered from the
// record, without another API call.
func TestRefundRepeatOfConfirmedAnsweredFromRecord(t *testing.T) {
	api := newLedger(0, map[string]int64{"txn-1": 1250})
	p := retryPayload(500, origPayment{TransactionRef: "txn-1", Amount: 1250})
	kv, links := memKV{}, linkStore{blobs: newFakeBlobs()}
	refundSale(api, testMerchant, kv, links, p)
	id, r := refundSale(api, testMerchant, kv, links, p)
	if r != "" || id != "txn-1" || len(api.bodies) != 1 || api.refunded["txn-1"] != 500 {
		t.Fatalf("repeat = (%q,%q), calls=%d refunded=%d", id, r, len(api.bodies), api.refunded["txn-1"])
	}
}

// A definite refusal leaves no record: a later retry starts fresh.
func TestRefundDefiniteFailureAllowsFreshRetry(t *testing.T) {
	api := newLedger(0, map[string]int64{"txn-1": 100})
	p := retryPayload(500, origPayment{TransactionRef: "txn-1", Amount: 1250})
	kv, links := memKV{}, linkStore{blobs: newFakeBlobs()}
	if _, r := refundSale(api, testMerchant, kv, links, p); r == "" {
		t.Fatal("expected a decline")
	}
	api.captured["txn-1"] = 1250
	if _, r := refundSale(api, testMerchant, kv, links, p); r != "" {
		t.Fatalf("fresh retry declined: %q", r)
	}
}

// The record is bounded.
func TestRefundRecordBounded(t *testing.T) {
	api := newLedger(0, map[string]int64{"txn-1": 1 << 30})
	kv, links := memKV{}, linkStore{blobs: newFakeBlobs()}
	for i := 0; i < 50; i++ {
		p := retryPayload(100, origPayment{TransactionRef: "txn-1", Amount: 1 << 30})
		p.EventID = "evt-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		refundSale(api, testMerchant, kv, links, p)
	}
	var list []json.RawMessage
	if err := json.Unmarshal([]byte(kv.get(refundsKey)), &list); err != nil || len(list) > maxRefundRecords {
		t.Fatalf("records = %d (err %v), want <= %d", len(list), err, maxRefundRecords)
	}
}
