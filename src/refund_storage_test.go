package main

import (
	"fmt"
	"strings"
	"testing"
)

// flakyKV fails every put once armed; before that it behaves like capKV.
type flakyKV struct {
	*capKV
	failAfterPuts int // fail once this many puts succeeded; <0 = never
}

func (f *flakyKV) put(k string, v []byte) error {
	if f.failAfterPuts >= 0 && f.capKV.puts >= f.failAfterPuts {
		f.capKV.failAll = true
	}
	return f.capKV.put(k, v)
}

// A refund whose "sending" record cannot be stored must not be sent: a lost
// reply would then be retried as a brand-new refund (double refund).
func TestRefundStorageFailureWholeNotSent(t *testing.T) {
	api := newLegFake(map[string]int64{"txn-A": 3000})
	kv := newCapKV()
	kv.failAll = true
	links := linkStore{blobs: newFakeBlobs()}
	p := eventPayload{Amount: 3000, OriginalSaleID: "s", EventID: "e1",
		OriginalPayments: []origPayment{leg("txn-A", 3000, 0, nil)}}
	id, dec := refundSale(api, testMerchant, kv, links, p)
	if id != "" || dec != declineRefundStorage || len(api.calls) != 0 {
		t.Fatalf("id=%q dec=%q calls=%v", id, dec, api.calls)
	}
}

func TestRefundStorageFailureLegsNotSent(t *testing.T) {
	api := newLegFake(map[string]int64{"txn-A": 3000, "txn-B": 2000})
	kv := newCapKV()
	kv.failAll = true
	links := linkStore{blobs: newFakeBlobs()}
	p := lp(5000, "e1", leg("txn-A", 3000, 0, ra(3000)), leg("txn-B", 2000, 0, ra(2000)))
	id, dec := refundSale(api, testMerchant, kv, links, p)
	if id != "" || dec != declineRefundStorage || len(api.calls) != 0 {
		t.Fatalf("id=%q dec=%q calls=%v", id, dec, api.calls)
	}
}

func TestRefundStorageFailureAfterFirstLegIsPartial(t *testing.T) {
	api := newLegFake(map[string]int64{"txn-A": 3000, "txn-B": 2000})
	// puts: leg 1 "sending" (1), leg 1 "done" (2); leg 2 "sending" fails.
	kv := &flakyKV{capKV: newCapKV(), failAfterPuts: 2}
	links := linkStore{blobs: newFakeBlobs()}
	p := lp(5000, "e1", leg("txn-A", 3000, 0, ra(3000)), leg("txn-B", 2000, 0, ra(2000)))
	id, dec := refundSale(api, testMerchant, kv, links, p)
	if id != "" || dec != declineRefundPartial || len(api.calls) != 1 || api.calls[0] != "txn-A" {
		t.Fatalf("id=%q dec=%q calls=%v", id, dec, api.calls)
	}
}

// Whole-amount mode tries candidates in turn. After a definite "too large" on
// txn-A, a failed "sending" save for txn-B must not leave txn-A's stale
// "sending" record behind: a retry would resend to txn-A, read "nothing left"
// as its own refund and approve with no money moved (review F1).
func TestRefundStorageFailureOnLaterCandidateNoFalseApproval(t *testing.T) {
	api := newLegFake(map[string]int64{"txn-A": 3000, "txn-B": 3000})
	api.refunded["txn-A"] = 1000 // refunded earlier elsewhere: 2000 left
	kv := &failMatchKV{capKV: newCapKV(), match: `"ref":"txn-B","state":"sending"`}
	links := linkStore{blobs: newFakeBlobs()}
	p := eventPayload{Amount: 3000, OriginalSaleID: "s", EventID: "e1",
		OriginalPayments: []origPayment{leg("txn-A", 3000, 0, nil), leg("txn-B", 3000, 0, nil)}}
	if id, dec := refundSale(api, testMerchant, kv, links, p); id != "" || dec != declineRefundStorage || len(api.calls) != 1 {
		t.Fatalf("first try: id=%q dec=%q calls=%v", id, dec, api.calls)
	}
	kv.match = "" // storage back
	id, dec := refundSale(api, testMerchant, kv, links, p)
	if dec == "" && api.refunded["txn-B"] != 3000 {
		t.Fatalf("approved %q with no money moved: refunded=%v calls=%v", id, api.refunded, api.calls)
	}
	if dec != "" || id != "txn-B" || api.refunded["txn-B"] != 3000 {
		t.Fatalf("retry: id=%q dec=%q refunded=%v calls=%v", id, dec, api.refunded, api.calls)
	}
}

// failMatchKV fails only the puts whose value contains match ("" = none).
type failMatchKV struct {
	*capKV
	match string
}

func (f *failMatchKV) put(k string, v []byte) error {
	if f.match != "" && strings.Contains(string(v), f.match) {
		return fmt.Errorf("storage_set: -3")
	}
	return f.capKV.put(k, v)
}
