package main

// Provider refund against the original SumUp transaction (ut-docs#6438).
// Untagged so the money path is covered by `go test`; main.go only wires the
// host.

import (
	"encoding/json"
	"net/url"
	"strings"
)

const declineUnknownOriginal = "unknown_original_payment"

// sumupTooLarge are SumUp's definite "this transaction cannot take that
// refund" answers (409 not refundable in its state, 422 processor refused
// with max_refundable_amount); only these let a split-tender refund move on
// to the next leg.
func sumupTooLarge(status int) bool { return status == 409 || status == 422 }

// refundOne asks SumUp to refund amount on one transaction.
func refundOne(api sumupAPI, txnID string, amount int64) (done, tooLarge bool, reason string) {
	reqBody, _ := json.Marshal(map[string]any{"amount": minorToMajor(amount)})
	body, status, ok := api.call("POST", "/v0.1/me/refund/"+url.PathEscape(txnID), reqBody)
	if !ok {
		return false, false, "network_error"
	}
	// The docs describe 204 No Content on success; treat any 2xx as success.
	if status >= 200 && status < 300 {
		return true, false, ""
	}
	var errResp struct {
		Message string `json:"message"`
		Error   string `json:"error_code"`
	}
	_ = json.Unmarshal(body, &errResp)
	if status >= 500 {
		return false, false, "network_error" // a 5xx may have been applied: unknown
	}
	return false, sumupTooLarge(status), firstNonEmpty(errResp.Error, errResp.Message, "refund_failed")
}

const (
	refundsKey           = "refunds"
	maxRefundRecords     = 8
	declineRefundUnknown = "refund_outcome_unknown"
	declineRefundStorage = "refund_storage_error"
)

// refundRecord remembers a refund sent for one event id (ut-docs#6598).
// SumUp's refund call has no idempotency key, so a retry after a lost reply
// must be answered from this record or sent to the same leg only.
type refundRecord struct {
	ID    string `json:"id"`
	Ref   string `json:"ref"`
	State string `json:"state"` // "sending" (reply not seen) or "done"; per-leg: "legs" or "done"
	// Legs is set only by a per-leg refund (v1.7.0, ut-docs#6603); a v1.6.0
	// record has none and is read as one whole-amount refund on Ref.
	Legs []legRecord `json:"legs,omitempty"`
}

// legRecord is one leg of a per-leg refund; State "" = not sent, "sending",
// "done".
type legRecord struct {
	Ref    string `json:"ref"`
	Amount int64  `json:"amount"`
	State  string `json:"state,omitempty"`
}

func loadRefundRecords(kv kvStore) []refundRecord {
	var l []refundRecord
	_ = json.Unmarshal([]byte(kv.get(refundsKey)), &l)
	return l
}

// saveRefundRecord upserts rec (newest last, bounded); state "" removes it.
// It returns the storage error (also logged). Callers that write "sending"
// before a money call must fail closed on it (ut-docs#6650); the ones that
// write "done" or clear may ignore it: the call already happened or nothing
// was sent.
func saveRefundRecord(kv kvStore, rec refundRecord) error {
	out := make([]refundRecord, 0, maxRefundRecords)
	for _, o := range loadRefundRecords(kv) {
		if o.ID != rec.ID {
			out = append(out, o)
		}
	}
	if rec.State != "" {
		out = append(out, rec)
	}
	if len(out) > maxRefundRecords {
		out = out[len(out)-maxRefundRecords:]
	}
	b, _ := json.Marshal(out)
	err := kv.put(refundsKey, b)
	if err != nil {
		logf("sumup: could not save the refund record (%v); a retry of this refund may need the SumUp dashboard", err)
	}
	return err
}

// legTotal is what core says the original payment on ref took (0 = unknown).
func legTotal(p eventPayload, ref string) int64 {
	for _, o := range p.OriginalPayments {
		if o.TransactionRef == ref {
			return o.Amount + o.TipAmount
		}
	}
	return 0
}

// refundSale finds the original transaction (refundRefs) and refunds it: one
// transaction only, never split across two. With several candidate legs it
// moves on only after a definite "too large" answer; any other failure
// declines. It never guesses: no reference means unknown_original_payment.
//
// A demo- reference is approved without a call; a real one needs api (nil =
// no key: no_api_key). With an event id, a record is written before the
// call: a repeat of a finished refund is answered from it; a retry after a
// lost reply goes to the SAME leg only, and only when it is a full refund of
// that leg (SumUp then answering "nothing left" is our own refund); a partial
// one cannot be told apart from a second refund, so it declines
// refund_outcome_unknown rather than risk paying twice.
func refundSale(api sumupAPI, kv kvStore, links linkStore, p eventPayload) (txnID, decline string) {
	if perLegMode(p) {
		return refundLegs(api, kv, p)
	}
	if p.EventID != "" {
		for _, rec := range loadRefundRecords(kv) {
			if rec.ID != p.EventID {
				continue
			}
			if rec.State == "done" {
				return rec.Ref, ""
			}
			if api == nil {
				return "", "no_api_key"
			}
			if t := legTotal(p, rec.Ref); t == 0 || p.Amount < t {
				return "", declineRefundUnknown
			}
			done, tooLarge, reason := refundOne(api, rec.Ref, p.Amount)
			if done || tooLarge {
				saveRefundRecord(kv, refundRecord{ID: p.EventID, Ref: rec.Ref, State: "done"})
				return rec.Ref, ""
			}
			if reason != "network_error" {
				saveRefundRecord(kv, refundRecord{ID: p.EventID}) // definite: start fresh next time
			}
			return "", reason
		}
	}
	refs := refundRefs(kv, links, p, "sale_txn:")
	if len(refs) == 0 {
		return "", declineUnknownOriginal
	}
	last := "refund_failed"
	for _, id := range refs {
		if strings.HasPrefix(id, demoRefPrefix) {
			return id, "" // nothing real was charged: nothing to send back
		}
		if api == nil {
			return "", "no_api_key"
		}
		if p.EventID != "" {
			// Fail closed: without the "sending" record a lost reply would be
			// retried as a new refund (double refund, ut-docs#6650).
			if err := saveRefundRecord(kv, refundRecord{ID: p.EventID, Ref: id, State: "sending"}); err != nil {
				saveRefundRecord(kv, refundRecord{ID: p.EventID}) // best effort: no stale earlier candidate
				return "", declineRefundStorage
			}
		}
		done, tooLarge, reason := refundOne(api, id, p.Amount)
		if done {
			if p.EventID != "" {
				saveRefundRecord(kv, refundRecord{ID: p.EventID, Ref: id, State: "done"})
			}
			logf("sumup: REFUNDED %d on %s", p.Amount, maskRef(id))
			return id, ""
		}
		last = reason
		if !tooLarge {
			if p.EventID != "" && reason != "network_error" {
				saveRefundRecord(kv, refundRecord{ID: p.EventID}) // definite refusal: nothing sent
			}
			return "", reason
		}
		if p.EventID != "" {
			// Definite "too large": nothing sent on id. Clear now, before the
			// next candidate's "sending" save: a stale "sending" record on id
			// would make a retry resend here, read "nothing left" as ours and
			// approve with no money moved (ut-docs#6650 review F1).
			saveRefundRecord(kv, refundRecord{ID: p.EventID})
		}
	}
	return "", last
}
