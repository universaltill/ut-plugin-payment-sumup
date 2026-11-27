package main

import (
	"fmt"
	"strings"
	"testing"
)

// Refund amount units and path (ut-docs#6695, supersedes #6686). SumUp dropped
// POST /v0.1/me/refund/{txn} (float major units) in April 2026 for POST
// /v1.0/merchants/{code}/payments/{id}/refunds, whose `amount` is an INTEGER in
// minor units ("500 for EUR 5.00"). 12.50 must go out as 1250, never 12.5.
// These pin the exact path and body for a full refund, a partial leg
// (refund_amount) and a reversal.
func TestRefundBodyIsMinorUnits(t *testing.T) {
	cases := []struct {
		name string
		p    eventPayload
		want string
	}{
		{"full refund of 12.50", lp(1250, "e-full", leg("txn-A", 1250, 0, nil)), `{"amount":1250}`},
		{"partial leg 4.50 of 12.50", lp(450, "e-part", leg("txn-A", 1250, 0, ra(450))), `{"amount":450}`},
		{"reversal of 12.50", eventPayload{Reversal: true, Amount: 1250, EventID: "e-rev",
			OriginalPayments: []origPayment{{TransactionRef: "txn-A", Amount: 1250}}}, `{"amount":1250}`},
		// A reversal never reads refund_amount: the whole amount still goes out.
		{"reversal ignores refund_amount", eventPayload{Reversal: true, Amount: 1250, EventID: "e-rev2",
			OriginalPayments: []origPayment{{TransactionRef: "txn-A", Amount: 1250, RefundAmount: ra(450)}}}, `{"amount":1250}`},
		{"one penny", lp(1, "e-1p", leg("txn-A", 1250, 0, nil)), `{"amount":1}`},
		{"19.99", lp(1999, "e-1999", leg("txn-A", 1999, 0, nil)), `{"amount":1999}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newLegFake(map[string]int64{"txn-A": 10000})
			kv, links := legEnv()
			id, dec := refundSale(api, testMerchant, kv, links, tc.p)
			if dec != "" || id != "txn-A" {
				t.Fatalf("id=%q dec=%q", id, dec)
			}
			if fmt.Sprint(api.calls) != "[txn-A]" || len(api.bodies) != 1 || api.bodies[0] != tc.want {
				t.Fatalf("calls=%v bodies=%v, want one %s (integer minor units)", api.calls, api.bodies, tc.want)
			}
			if got := api.refunded["txn-A"]; got != tc.p.Amount {
				t.Fatalf("SumUp refunded %d minor units, want %d", got, tc.p.Amount)
			}
		})
	}
}

func TestRefundPathIsV1MerchantPayments(t *testing.T) {
	if got := refundPath("MCODE", "txn-A"); got != "/v1.0/merchants/MCODE/payments/txn-A/refunds" {
		t.Fatalf("path = %s", got)
	}
	// Both parts go into the URL path: escaped, never able to add a segment.
	if got := refundPath("M/../X", "a/b"); got != "/v1.0/merchants/M%2F..%2FX/payments/a%2Fb/refunds" {
		t.Fatalf("unescaped path = %s", got)
	}
}

// A missing sumup_merchant_code declines no_merchant_code: no call to SumUp,
// no fallback to the retired legacy path, and no record left behind.
func TestRefundWithoutMerchantCodeDeclines(t *testing.T) {
	for _, code := range []string{"", "   "} {
		api := newLegFake(map[string]int64{"txn-A": 10000})
		kv, links := legEnv()
		id, dec := refundSale(api, code, kv, links, lp(1250, "e1", leg("txn-A", 1250, 0, nil)))
		if id != "" || dec != declineNoMerchantCode || len(api.calls) != 0 || len(loadRefundRecords(kv)) != 0 {
			t.Fatalf("code %q: id=%q dec=%q calls=%v records=%v", code, id, dec, api.calls, loadRefundRecords(kv))
		}
		// per-leg path too
		_, dec = refundSale(api, code, kv, links, lp(450, "e2", leg("txn-A", 1250, 0, ra(450))))
		if dec != declineNoMerchantCode || len(api.calls) != 0 {
			t.Fatalf("legs, code %q: dec=%q calls=%v", code, dec, api.calls)
		}
	}
	// A demo- reference needs no merchant code (nothing real to send).
	id, dec := refundSale(nil, "", memKV{}, linkStore{blobs: newFakeBlobs()}, lp(400, "e3", leg("demo-400", 400, 0, nil)))
	if id != "demo-400" || dec != "" {
		t.Fatalf("demo: %q %q", id, dec)
	}
}

// A refund recorded by v1.6.0..v1.10.x (same transaction ids, no migration)
// is still answered, and a "sending" one still resends to the same leg on the
// new endpoint.
func TestRefundOldRecordStillWorksOnV1(t *testing.T) {
	api := newLegFake(map[string]int64{"txn-A": 10000})
	kv, links := legEnv()
	saveRefundRecord(kv, refundRecord{ID: "e-old", Ref: "txn-A", State: "done"})
	id, dec := refundSale(api, testMerchant, kv, links, lp(1250, "e-old", leg("txn-A", 1250, 0, nil)))
	if id != "txn-A" || dec != "" || len(api.calls) != 0 {
		t.Fatalf("done record: %q %q calls=%v", id, dec, api.calls)
	}
	saveRefundRecord(kv, refundRecord{ID: "e-old2", Ref: "txn-A", State: "sending"})
	id, dec = refundSale(api, testMerchant, kv, links, lp(1250, "e-old2", leg("txn-A", 1250, 0, nil)))
	if id != "txn-A" || dec != "" || len(api.bodies) != 1 || !strings.Contains(api.bodies[0], "1250") {
		t.Fatalf("sending record: %q %q bodies=%v", id, dec, api.bodies)
	}
}
