package main

// Provider refund against the original SumUp transaction (ut-docs#6438).
// Untagged so the money path is covered by `go test`; main.go only wires the
// host.

import (
	"encoding/json"
	"net/url"
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
	return false, sumupTooLarge(status), firstNonEmpty(errResp.Error, errResp.Message, "refund_failed")
}

// refundSale finds the original transaction (refundRefs) and refunds it: one
// transaction only, never split across two. With several candidate legs it
// moves on only after a definite "too large" answer; any other failure
// declines. It never guesses: no reference means unknown_original_payment.
func refundSale(api sumupAPI, kv kvStore, links linkStore, p eventPayload) (txnID, decline string) {
	refs := refundRefs(kv, links, p, "sale_txn:")
	if len(refs) == 0 {
		return "", declineUnknownOriginal
	}
	last := "refund_failed"
	for _, id := range refs {
		done, tooLarge, reason := refundOne(api, id, p.Amount)
		if done {
			logf("sumup: REFUNDED %d on %s", p.Amount, maskRef(id))
			return id, ""
		}
		last = reason
		if !tooLarge {
			return "", reason
		}
	}
	return "", last
}
