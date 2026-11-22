package main

// Per-leg refunds (ut-docs#6603, ADR-0196 d.4): when core splits a refund
// across 2+ original payments of this method it sends each leg's own
// `refund_amount`. The plugin refunds exactly that on each leg, in payload
// order, and never picks or skips a leg itself (no "too large -> next leg").
// Untagged so the money path is covered by `go test`.

import (
	"math"
	"strings"
)

const (
	declineRefundPartial       = "refund_partial"
	declineInvalidRefundAmount = "invalid_refund_amount"
	declineTooManyLegs         = "too_many_original_payments"
	maxRefundLegs              = 16
)

// refundLeg is one planned provider refund. Full: it refunds all the leg
// took (amount + tip), so a lost-reply resend is safe to treat as ours.
type refundLeg struct {
	Ref    string
	Amount int64
	Full   bool
}

// perLegMode: a (non-reversal) refund where core sent refund_amount on at
// least one leg. A reversal never reads the field; without it the legacy
// single-leg path runs unchanged.
func perLegMode(p eventPayload) bool {
	if p.Reversal {
		return false
	}
	for _, o := range p.OriginalPayments {
		if o.RefundAmount != nil {
			return true
		}
	}
	return false
}

// planLegs validates the whole allocation before any provider call.
func planLegs(p eventPayload) ([]refundLeg, string) {
	if len(p.OriginalPayments) > maxRefundLegs {
		return nil, declineTooManyLegs
	}
	for _, o := range p.OriginalPayments {
		if strings.TrimSpace(o.TransactionRef) == "" {
			return nil, declineUnknownOriginal
		}
	}
	legs := make([]refundLeg, 0, len(p.OriginalPayments))
	var sum int64
	for _, o := range p.OriginalPayments {
		if o.RefundAmount == nil || *o.RefundAmount <= 0 || o.Amount < 0 || o.TipAmount < 0 || o.Amount > math.MaxInt64-o.TipAmount {
			return nil, declineInvalidRefundAmount
		}
		r, total := *o.RefundAmount, o.Amount+o.TipAmount
		if r > total || sum > math.MaxInt64-r {
			return nil, declineInvalidRefundAmount
		}
		sum += r
		legs = append(legs, refundLeg{Ref: o.TransactionRef, Amount: r, Full: r == total})
	}
	if sum != p.Amount {
		return nil, declineInvalidRefundAmount
	}
	return legs, ""
}

func sameLegs(rec []legRecord, plan []refundLeg) bool {
	if len(rec) != len(plan) {
		return false
	}
	for i := range plan {
		if rec[i].Ref != plan[i].Ref || rec[i].Amount != plan[i].Amount {
			return false
		}
	}
	return true
}

// refundLegs refunds each planned leg exactly once per event id. The record
// (refunds, one entry per event id) keeps each leg's state: "" not sent,
// "sending" (call made, reply not seen) or "done". Returns the first leg's
// reference on success. Any failure stops at that leg; if an earlier leg is
// done the decline is refund_partial (money already moved, never rolled back).
func refundLegs(api sumupAPI, kv kvStore, p eventPayload) (txnID, decline string) {
	legs, dec := planLegs(p)
	if dec != "" {
		return "", dec
	}
	real := false
	for _, l := range legs {
		if !strings.HasPrefix(l.Ref, demoRefPrefix) {
			real = true
		}
	}
	if !real {
		return legs[0].Ref, "" // nothing real was charged: nothing to send back
	}
	if api == nil {
		return "", "no_api_key"
	}

	rec := refundRecord{ID: p.EventID, Ref: legs[0].Ref, State: "legs", Legs: make([]legRecord, len(legs))}
	for i, l := range legs {
		rec.Legs[i] = legRecord{Ref: l.Ref, Amount: l.Amount}
	}
	if p.EventID != "" {
		for _, old := range loadRefundRecords(kv) {
			if old.ID != p.EventID {
				continue
			}
			if len(old.Legs) == 0 { // written by v1.6.0: one whole-amount refund on one leg
				if old.State == "done" {
					return old.Ref, ""
				}
				return "", declineRefundUnknown
			}
			if !sameLegs(old.Legs, legs) {
				return "", declineRefundUnknown
			}
			rec = old
		}
	}
	save := func() error {
		if p.EventID == "" {
			return nil
		}
		anyOpen := false
		for _, l := range rec.Legs {
			if l.State != "" {
				anyOpen = true
			}
		}
		if !anyOpen {
			return saveRefundRecord(kv, refundRecord{ID: p.EventID}) // nothing sent: forget it
		}
		return saveRefundRecord(kv, rec)
	}
	doneBefore := func(upTo int) bool {
		for _, l := range rec.Legs[:upTo] {
			if l.State == "done" {
				return true
			}
		}
		return false
	}

	for i, l := range legs {
		st := rec.Legs[i].State
		switch {
		case st == "done":
			continue
		case strings.HasPrefix(l.Ref, demoRefPrefix):
			rec.Legs[i].State = "done" // test-mode leg: nothing to send
			continue
		case st == "sending" && !l.Full:
			// A partial refund cannot be told apart from a second one.
			logf("sumup: refund leg %s (%d) outcome unknown", maskRef(l.Ref), l.Amount)
			save()
			return "", declineRefundUnknown
		}
		resend := st == "sending"
		rec.Legs[i].State = "sending"
		if err := save(); err != nil {
			// Fail closed (ut-docs#6650): no record, no call.
			rec.Legs[i].State = st
			if doneBefore(i) {
				return "", declineRefundPartial
			}
			return "", declineRefundStorage
		}
		done, tooLarge, reason := refundOne(api, l.Ref, l.Amount)
		if done || (resend && tooLarge) { // resend: SumUp says nothing left = ours
			rec.Legs[i].State = "done"
			save()
			logf("sumup: REFUNDED %d on %s", l.Amount, maskRef(l.Ref))
			continue
		}
		if reason != "network_error" {
			rec.Legs[i].State = "" // definite refusal: nothing sent
		}
		rec.State = "legs"
		save()
		logf("sumup: refund leg %s (%d) failed: %s", maskRef(l.Ref), l.Amount, scrubRefs(reason))
		if doneBefore(len(legs)) {
			return "", declineRefundPartial
		}
		return "", reason
	}
	rec.State = "done"
	save()
	return legs[0].Ref, ""
}
