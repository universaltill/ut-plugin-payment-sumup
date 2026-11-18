package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// #6599: test mode is an explicit setting; a missing reader is never demo.
func TestAuthorizeModeDecision(t *testing.T) {
	cases := []struct {
		name               string
		test               bool
		apiKey, reader     string
		wantMode, wantDecl string
	}{
		{"explicit test mode", true, "", "", "demo", ""},
		{"test mode beats a key", true, "k", "", "demo", ""},
		{"key, no reader", false, "k", "", "", "no_reader"},
		{"no key, no reader", false, "", "", "", "no_api_key"},
		{"key and reader", false, "k", "rdr", "reader", ""},
		{"reader without key", false, "", "rdr", "", "no_api_key"},
	}
	for _, c := range cases {
		mode, decl := authorizeMode(c.test, c.apiKey, c.reader)
		if mode != c.wantMode || decl != c.wantDecl {
			t.Errorf("%s: got (%q,%q), want (%q,%q)", c.name, mode, decl, c.wantMode, c.wantDecl)
		}
	}
}

// #6599: a real reference is refunded through the API whatever the reader
// setting; a demo- reference never reaches SumUp.
func TestRefundRealRefGoesToAPIDemoRefDoesNot(t *testing.T) {
	api := newLedger(0, map[string]int64{"txn-1": 1000})
	kv, links := memKV{}, linkStore{blobs: newFakeBlobs()}
	p := eventPayload{Amount: 400, EventID: "e1", OriginalPayments: []origPayment{{TransactionRef: "txn-1", Amount: 1000}}}
	if _, r := refundSale(api, kv, links, p); r != "" || api.refunded["txn-1"] != 400 {
		t.Fatalf("real ref: decline %q refunded %d", r, api.refunded["txn-1"])
	}
	d := eventPayload{Amount: 400, EventID: "e2", OriginalPayments: []origPayment{{TransactionRef: "demo-400", Amount: 400}}}
	id, r := refundSale(api, kv, links, d)
	if r != "" || id != "demo-400" || len(api.bodies) != 1 {
		t.Fatalf("demo ref: (%q,%q) api calls %d", id, r, len(api.bodies))
	}
	// no API key (nil api) and a demo ref still approves; a real ref declines.
	if _, r := refundSale(nil, kv, links, d); r != "" {
		t.Errorf("demo ref without key declined: %q", r)
	}
	p.EventID = "e3"
	if _, r := refundSale(nil, kv, links, p); r != "no_api_key" {
		t.Errorf("real ref without key = %q, want no_api_key", r)
	}
}

func TestManifestHasExplicitTestModeSettingDefaultOff(t *testing.T) {
	raw, err := os.ReadFile("../manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Settings []struct {
			Key     string `json:"key"`
			Default string `json:"default_value"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, s := range m.Settings {
		if s.Key == testModeSetting {
			if s.Default != "off" {
				t.Errorf("default = %q, want off", s.Default)
			}
			return
		}
	}
	t.Fatalf("manifest has no %s setting", testModeSetting)
}

func TestTestModeOnlyWhenExplicitlyOn(t *testing.T) {
	for v, want := range map[string]bool{"on": true, " ON ": true, "off": false, "": false, "true": false} {
		if got := isTestMode(v); got != want {
			t.Errorf("isTestMode(%q) = %v, want %v", v, got, want)
		}
	}
	_ = strings.TrimSpace
}

// #6606: the stdout answer carries neither `recent` nor full refs.
func TestSaveAuthorizeResultDoesNotMutateAnswer(t *testing.T) {
	kv := memKV{}
	answer := map[string]any{"provider": "sumup", "transaction_ref": "txn-secret-123456"}
	saveAuthorizeResult(kv, answer, &recentEntry{ID: "e", Ref: "txn-secret-123456", Amount: 100, At: 1})
	if _, has := answer["recent"]; has {
		t.Fatalf("answer map gained `recent`: %v", answer)
	}
	if !strings.Contains(kv.get(lastTxnKey), `"recent"`) {
		t.Errorf("recent no longer stored")
	}
}

// #6600 (plugin side): an unknown outcome is printed as decline_code.
func TestDeclineAnswerCarriesOutcomeUnknown(t *testing.T) {
	var a map[string]any
	if err := json.Unmarshal(declineAnswer(1250, "gbp", "outcome_unknown"), &a); err != nil {
		t.Fatal(err)
	}
	if a["decline_code"] != "outcome_unknown" || a["outcome"] != "declined" {
		t.Fatalf("answer = %v", a)
	}
	if _, has := a["recent"]; has {
		t.Error("recent leaked")
	}
}
