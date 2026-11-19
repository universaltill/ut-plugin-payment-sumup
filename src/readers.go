package main

// Per-call reader (ut-docs#6566). The till's reader registry may name the
// SumUp reader for one authorize call (`reader_id` in the payload), and may
// ask for the state of several readers (`payment.sumup.readers`). Untagged so
// `go test` covers it; main.go only wires the host.
//
// Security: the reader id is put into a SumUp URL path, so a payload id is
// format-checked first (an id such as `rdr_1/../x` must never reach a URL).

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

const declineInvalidReader = "invalid_reader"

// maxStateReaders caps one state call: the check is sequential HTTP inside one
// guest call, and the host gate is short.
const maxStateReaders = 16

// validReaderID: 1..64 of letters, digits, `_` and `-`. Ids seen are `rdr_...`;
// the prefix is not enforced because it is unverified against the live API.
func validReaderID(id string) bool {
	if len(id) < 1 || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// chooseReader picks the reader for an authorize call. A payload id wins; a
// missing or blank one falls back to the `sumup_reader_id` setting exactly as
// before (an old till never sends it). A payload id that is present but
// malformed declines with invalid_reader: never a fallback to another reader,
// which would charge a different physical device.
func chooseReader(payloadID, settingID string) (id, reason string) {
	if strings.TrimSpace(payloadID) == "" {
		return strings.TrimSpace(settingID), ""
	}
	if !validReaderID(payloadID) { // strict: no trimming of a non-empty id
		return "", declineInvalidReader
	}
	return payloadID, ""
}

type readerState struct {
	ReaderID string `json:"reader_id"`
	State    string `json:"state"` // ready | busy | offline | unknown
	Reason   string `json:"reason,omitempty"`
}

// readerStatusPath is GET .../readers/{id}/status.
func readerStatusPath(merchantCode, readerID string) string {
	return fmt.Sprintf("/v0.1/merchants/%s/readers/%s/status", url.PathEscape(merchantCode), readerID)
}

// readerStatus reads SumUp's reader status: (status, state) such as ONLINE and
// IDLE, plus the HTTP code; ok=false on a transport failure or a body that is
// not the documented shape.
func readerStatus(api sumupAPI, merchantCode, readerID string) (status, state string, code int, ok bool) {
	body, code, ok := api.call("GET", readerStatusPath(merchantCode, readerID), nil)
	if !ok {
		return "", "", 0, false
	}
	if code < 200 || code >= 300 {
		return "", "", code, true
	}
	var st struct {
		Data struct {
			Status string `json:"status"`
			State  string `json:"state"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &st) != nil {
		return "", "", code, false
	}
	return st.Data.Status, st.Data.State, code, true
}

// readerStates asks SumUp for each reader's state (read-only GET). A ref that
// is not a valid reader id is never sent. Anything unclear is "unknown": the
// till still lets the cashier choose, and SumUp fails safe on the checkout.
func readerStates(api sumupAPI, merchantCode string, refs []string) []readerState {
	if len(refs) > maxStateReaders {
		refs = refs[:maxStateReaders]
	}
	out := make([]readerState, 0, len(refs))
	for _, ref := range refs {
		rs := readerState{ReaderID: ref, State: "unknown"}
		switch {
		case !validReaderID(ref):
			rs.Reason = declineInvalidReader
		case merchantCode == "":
			rs.Reason = "no_merchant_code"
		default:
			rs = oneReaderState(api, merchantCode, ref)
		}
		out = append(out, rs)
	}
	return out
}

func oneReaderState(api sumupAPI, merchantCode, ref string) readerState {
	rs := readerState{ReaderID: ref, State: "unknown"}
	status, state, code, ok := readerStatus(api, merchantCode, ref)
	switch {
	case code == 404:
		rs.State, rs.Reason = "offline", "reader_unreachable"
	case code == 401:
		rs.Reason = "invalid_api_key" // never log SumUp's message
	case !ok || code < 200 || code >= 300:
		logf("sumup: reader %s state unavailable (http %d)", maskRef(ref), code)
	case status == "ONLINE" && state == "IDLE":
		rs.State = "ready"
	case status == "ONLINE":
		rs.State = "busy"
	case status == "OFFLINE":
		rs.State = "offline"
	}
	return rs
}

// readersAnswer is the stdout JSON of the payment.sumup.readers hook. Only op
// "state" is served; anything else answers an empty list.
func readersAnswer(api sumupAPI, merchantCode string, p eventPayload) []byte {
	rows := []readerState{}
	if p.Op == "state" {
		rows = readerStates(api, merchantCode, p.Readers)
	}
	b, _ := json.Marshal(map[string]any{"readers": rows})
	return b
}
