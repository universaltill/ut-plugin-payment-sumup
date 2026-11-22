package main

import "testing"

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
	id, dec := refundSale(api, kv, links, p)
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
	id, dec := refundSale(api, kv, links, p)
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
	id, dec := refundSale(api, kv, links, p)
	if id != "" || dec != declineRefundPartial || len(api.calls) != 1 || api.calls[0] != "txn-A" {
		t.Fatalf("id=%q dec=%q calls=%v", id, dec, api.calls)
	}
}
