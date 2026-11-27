package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ut-docs#6566: the till may name the reader per call (payload reader_id).

func TestChooseReaderPayloadWinsOverSetting(t *testing.T) {
	id, reason := chooseReader("rdr_PAYLOAD", "rdr_setting")
	if id != "rdr_PAYLOAD" || reason != "" {
		t.Fatalf("got %q %q", id, reason)
	}
}

func TestChooseReaderFallsBackToSettingWhenMissing(t *testing.T) {
	for _, p := range []string{"", "  "} {
		id, reason := chooseReader(p, " rdr_setting ")
		if id != "rdr_setting" || reason != "" {
			t.Fatalf("payload %q: got %q %q", p, id, reason)
		}
	}
	// no id anywhere: unchanged decisions (no_reader / test-mode demo)
	id, reason := chooseReader("", "")
	if id != "" || reason != "" {
		t.Fatalf("%q %q", id, reason)
	}
	if _, why := authorizeMode(false, "k", id); why != "no_reader" {
		t.Fatalf("no id anywhere must still decline no_reader, got %q", why)
	}
	if mode, _ := authorizeMode(true, "", id); mode != "demo" {
		t.Fatalf("no id anywhere in test mode must stay demo, got %q", mode)
	}
}

func TestChooseReaderRejectsMalformedNoFallback(t *testing.T) {
	bad := []string{
		"rdr_1/../x", "../", "rdr/1", "rdr 1", "rdr_1?x=1", "rdr_1\n", "rdr_é",
		strings.Repeat("a", 65), "rdr%2f1", "rdr_1#frag", "rdr:1", "rdr.1",
	}
	for _, p := range bad {
		id, reason := chooseReader(p, "rdr_setting")
		if id != "" || reason != "invalid_reader" {
			t.Errorf("payload %q: got %q %q, want decline invalid_reader and no fallback", p, id, reason)
		}
	}
	for _, ok := range []string{"rdr_ABC123", "a", strings.Repeat("a", 64), "x-y_z"} {
		if _, reason := chooseReader(ok, ""); reason != "" {
			t.Errorf("%q must be accepted, got %q", ok, reason)
		}
	}
}

func TestEventPayloadCarriesReaderID(t *testing.T) {
	ev := parseEvent([]byte(`{"id":"e1","type":"payment.sumup.authorize","payload":{"amount":1250,"reader_id":"rdr_P","reader_kind":"cloud"}}`))
	if ev.Payload.ReaderID != "rdr_P" || ev.Payload.Amount != 1250 {
		t.Fatalf("%+v", ev.Payload)
	}
}

// The payload reader reaches the provider URL path, not the setting's, and the
// amount stays integer minor units in the checkout body.
func TestPayloadReaderUsedInCheckoutURL(t *testing.T) {
	clk, f, kv := newWorld()
	f.tapAt = 2 * time.Second
	c := newReaderCharger(f, kv, "att:0:ab")
	id, reason := chooseReader("rdr_PAYLOAD9", "rdr_1")
	if reason != "" {
		t.Fatal(reason)
	}
	c.readerID = id
	var paths []string
	c.api = pathSpy{inner: f, paths: &paths}
	if out := runReader(t, c, clk); !out.approved {
		t.Fatalf("got %+v", out)
	}
	hit := false
	for _, p := range paths {
		if strings.Contains(p, "rdr_1") {
			t.Errorf("setting reader used: %s", p)
		}
		if strings.Contains(p, "/readers/rdr_PAYLOAD9/") {
			hit = true
		}
	}
	if !hit {
		t.Fatalf("payload reader never called: %v", paths)
	}
	var body struct {
		Total struct {
			Value int64 `json:"value"`
		} `json:"total_amount"`
	}
	_ = json.Unmarshal(f.checkoutBodies[0], &body)
	if body.Total.Value != 1250 {
		t.Fatalf("amount %s", f.checkoutBodies[0])
	}
}

type pathSpy struct {
	inner sumupAPI
	paths *[]string
}

func (s pathSpy) call(m, p string, b []byte) ([]byte, int, bool) {
	*s.paths = append(*s.paths, p)
	return s.inner.call(m, p, b)
}

// Refund stays by transaction_ref: a reader_id on a leg is accepted and never
// blocks, even a malformed one.
func TestRefundIgnoresReaderIDOnLegs(t *testing.T) {
	raw := `{"id":"evt-9","type":"payment.sumup.refund","payload":{"amount":400,"original_payments":[{"transaction_ref":"txn-1","amount":1000,"reader_id":"../evil"}],"reader_id":"rdr_gone"}}`
	ev := parseEvent([]byte(raw))
	ev.Payload.EventID = ev.ID
	if ev.Payload.OriginalPayments[0].ReaderID != "../evil" {
		t.Fatalf("leg reader not parsed: %+v", ev.Payload.OriginalPayments)
	}
	api := newLedger(0, map[string]int64{"txn-1": 1000})
	if _, r := refundSale(api, testMerchant, memKV{}, linkStore{blobs: newFakeBlobs()}, ev.Payload); r != "" || api.refunded["txn-1"] != 400 {
		t.Fatalf("decline %q refunded %d", r, api.refunded["txn-1"])
	}
}

// ---- .readers state hook ----

type statusFake struct {
	calls []string
	reply map[string]fakeReply
}

type fakeReply struct {
	status int
	body   string
	ok     bool
}

func (f *statusFake) call(method, path string, body []byte) ([]byte, int, bool) {
	f.calls = append(f.calls, method+" "+path)
	parts := strings.Split(path, "/")
	id := parts[len(parts)-2]
	r, found := f.reply[id]
	if !found {
		return []byte(`{"message":"Not Found"}`), 404, true
	}
	return []byte(r.body), r.status, r.ok
}

func st(status, state string) fakeReply {
	return fakeReply{200, `{"data":{"status":"` + status + `","state":"` + state + `"}}`, true}
}

func TestReaderStatesMapping(t *testing.T) {
	f := &statusFake{reply: map[string]fakeReply{
		"rdr_idle": st("ONLINE", "IDLE"), "rdr_wait": st("ONLINE", "WAITING_FOR_CARD"),
		"rdr_off": st("OFFLINE", "IDLE"), "rdr_weird": st("SLEEPING", "IDLE"),
		"rdr_500": {500, `{"message":"sk_live_secret"}`, true}, "rdr_json": {200, "nope", true},
		"rdr_net": {0, "", false}, "rdr_401": {401, `{"message":"bad key"}`, true},
	}}
	refs := []string{"rdr_idle", "rdr_wait", "rdr_off", "rdr_gone", "rdr_weird", "rdr_500", "rdr_json", "rdr_net", "rdr_401"}
	got := readerStates(f, "M1", refs)
	want := map[string][2]string{
		"rdr_idle": {"ready", ""}, "rdr_wait": {"busy", ""}, "rdr_off": {"offline", ""},
		"rdr_gone": {"offline", "reader_unreachable"}, "rdr_weird": {"unknown", ""},
		"rdr_500": {"unknown", ""}, "rdr_json": {"unknown", ""}, "rdr_net": {"unknown", ""},
		"rdr_401": {"unknown", "invalid_api_key"},
	}
	if len(got) != len(refs) {
		t.Fatalf("%d rows", len(got))
	}
	for i, r := range got {
		if r.ReaderID != refs[i] {
			t.Errorf("order %q at %d", r.ReaderID, i)
		}
		w := want[r.ReaderID]
		if r.State != w[0] || r.Reason != w[1] {
			t.Errorf("%s: %s/%s want %s/%s", r.ReaderID, r.State, r.Reason, w[0], w[1])
		}
	}
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "GET /v0.1/merchants/M1/readers/") || !strings.HasSuffix(c, "/status") {
			t.Errorf("state check must be a read-only status GET: %q", c)
		}
	}
}

func TestReaderStatesTruncatesAndNeverCallsMalformed(t *testing.T) {
	f := &statusFake{}
	var refs []string
	for i := 0; i < 20; i++ {
		refs = append(refs, "rdr_r"+string(rune('a'+i)))
	}
	refs[0] = "rdr_bad/../x"
	got := readerStates(f, "M1", refs)
	if len(got) != maxStateReaders {
		t.Fatalf("%d rows", len(got))
	}
	if got[0].State != "unknown" || got[0].Reason != "invalid_reader" {
		t.Fatalf("%+v", got[0])
	}
	if len(f.calls) != maxStateReaders-1 {
		t.Fatalf("%d calls", len(f.calls))
	}
	for _, c := range f.calls {
		if strings.Contains(c, "..") {
			t.Fatalf("malformed ref reached a URL: %q", c)
		}
	}
}

func TestReadersAnswer(t *testing.T) {
	ev := parseEvent([]byte(`{"id":"e","type":"payment.sumup.readers","payload":{"op":"state","readers":["rdr_a","rdr_b"]}}`))
	if ev.Payload.Op != "state" || len(ev.Payload.Readers) != 2 {
		t.Fatalf("%+v", ev.Payload)
	}
	f := &statusFake{reply: map[string]fakeReply{"rdr_a": st("ONLINE", "IDLE")}}
	b := readersAnswer(f, "M1", ev.Payload)
	var out struct {
		Readers []map[string]string `json:"readers"`
	}
	if err := json.Unmarshal(b, &out); err != nil || len(out.Readers) != 2 {
		t.Fatalf("%s %v", b, err)
	}
	if out.Readers[0]["state"] != "ready" {
		t.Fatalf("%s", b)
	}
	if _, has := out.Readers[0]["reason"]; has {
		t.Fatalf("empty reason must be omitted: %s", b)
	}
	if got := readersAnswer(&statusFake{}, "M1", eventPayload{Op: "write", Readers: []string{"rdr_a"}}); string(got) != `{"readers":[]}` {
		t.Fatalf("%s", got)
	}
}

func TestReadersNoMerchantCodeIsUnknown(t *testing.T) {
	f := &statusFake{}
	got := readerStates(f, "", []string{"rdr_a"})
	if got[0].State != "unknown" || got[0].Reason != "no_merchant_code" || len(f.calls) != 0 {
		t.Fatalf("%+v calls %v", got, f.calls)
	}
}

func TestReaderStateLogsMaskedOnly(t *testing.T) {
	var logs []string
	logHook = func(m string) { logs = append(logs, m) }
	t.Cleanup(func() { logHook = nil })
	f := &statusFake{reply: map[string]fakeReply{"rdr_SECRETREADER123": {401, `{"message":"key sup_sk_abc"}`, true}}}
	readerStates(f, "M1", []string{"rdr_SECRETREADER123"})
	for _, l := range logs {
		if strings.Contains(l, "SECRETREADER") || strings.Contains(l, "sup_sk") {
			t.Fatalf("log leaks: %q", l)
		}
	}
}
