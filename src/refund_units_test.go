package main

import (
	"fmt"
	"testing"
)

// Refund amount units (ut-docs#6686). The till's refund event carries integer
// minor units; the legacy endpoint this plugin calls, POST
// /v0.1/me/refund/{txn_id}, takes {"amount": <number>}, a decimal read as MAJOR
// units — SumUp's own OpenAPI-generated Go SDK (sumup/sumup-go, synced from
// sumup/apis) typed it `Amount *float32` (no unit stated) next to float
// major-unit amounts until late April 2026, when the spec replaced it with POST
// /v1.0/merchants/{code}/payments/{id}/refunds, whose `amount` is an integer
// in minor units ("500 for EUR 5.00"). Until the plugin moves to v1.0, 12.50
// must go out as 12.5, never 1250.
// These pin the exact body for a full refund, a partial leg (refund_amount)
// and a reversal.
func TestRefundBodyIsMajorUnits(t *testing.T) {
	cases := []struct {
		name string
		p    eventPayload
		want string
	}{
		{"full refund of 12.50", lp(1250, "e-full", leg("txn-A", 1250, 0, nil)), `{"amount":12.5}`},
		{"partial leg 4.50 of 12.50", lp(450, "e-part", leg("txn-A", 1250, 0, ra(450))), `{"amount":4.5}`},
		{"reversal of 12.50", eventPayload{Reversal: true, Amount: 1250, EventID: "e-rev",
			OriginalPayments: []origPayment{{TransactionRef: "txn-A", Amount: 1250}}}, `{"amount":12.5}`},
		// A reversal never reads refund_amount: the whole amount still goes out.
		{"reversal ignores refund_amount", eventPayload{Reversal: true, Amount: 1250, EventID: "e-rev2",
			OriginalPayments: []origPayment{{TransactionRef: "txn-A", Amount: 1250, RefundAmount: ra(450)}}}, `{"amount":12.5}`},
		{"one penny", lp(1, "e-1p", leg("txn-A", 1250, 0, nil)), `{"amount":0.01}`},
		{"19.99", lp(1999, "e-1999", leg("txn-A", 1999, 0, nil)), `{"amount":19.99}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newLegFake(map[string]int64{"txn-A": 10000})
			kv, links := legEnv()
			id, dec := refundSale(api, kv, links, tc.p)
			if dec != "" || id != "txn-A" {
				t.Fatalf("id=%q dec=%q", id, dec)
			}
			if fmt.Sprint(api.calls) != "[txn-A]" || len(api.bodies) != 1 || api.bodies[0] != tc.want {
				t.Fatalf("calls=%v bodies=%v, want one %s (major units)", api.calls, api.bodies, tc.want)
			}
			if got := api.refunded["txn-A"]; got != tc.p.Amount {
				t.Fatalf("SumUp refunded %d minor units, want %d", got, tc.p.Amount)
			}
		})
	}
}
