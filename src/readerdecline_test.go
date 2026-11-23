package main

import (
	"strings"
	"testing"
)

// ut-docs#6961: a refused reader start maps to a fixed decline slug, never
// SumUp text.
func TestReaderStartDecline(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		wantCode string
		wantType string
	}{
		{"nested busy", 422, `{"errors":{"type":"READER_BUSY","detail":"There is a pending checkout for the device."}}`, "reader_busy", "READER_BUSY"},
		{"nested offline", 422, `{"errors":{"type":"READER_OFFLINE","detail":"x"}}`, "reader_offline", "READER_OFFLINE"},
		{"nested not connected", 400, `{"errors":{"type":"DEVICE_NOT_CONNECTED"}}`, "reader_offline", "DEVICE_NOT_CONNECTED"},
		{"nested affiliate key", 403, `{"errors":{"type":"INVALID_AFFILIATE_KEY","detail":"x"}}`, "auth_failed", "INVALID_AFFILIATE_KEY"},
		{"merchant mismatch", 400, `{"errors":{"type":"MERCHANT_MISMATCH"}}`, "auth_failed", "MERCHANT_MISMATCH"},
		{"401 empty body", 401, ``, "auth_failed", "none"},
		{"403 empty body", 403, ``, "auth_failed", "none"},
		{"404 empty", 404, ``, "no_reader", "none"},
		{"reader not found", 400, `{"errors":{"type":"READER_NOT_FOUND"}}`, "no_reader", "READER_NOT_FOUND"},
		{"not found type", 422, `{"errors":{"type":"not_found"}}`, "no_reader", "NOT_FOUND"},
		{"409 unknown type", 409, `{"errors":{"type":"WHATEVER"}}`, "reader_busy", "WHATEVER"},
		{"409 empty", 409, ``, "reader_busy", "none"},
		{"422 validation", 422, `{"errors":{"type":"INVALID_REQUEST","detail":"bad"}}`, "reader_start_failed", "INVALID_REQUEST"},
		{"400 empty", 400, ``, "reader_start_failed", "none"},
		{"flat busy", 422, `{"type":"READER_BUSY","message":"There is a pending checkout for the device."}`, "reader_busy", "READER_BUSY"},
		{"flat lowercase busy", 422, `{"type":"reader_busy"}`, "reader_busy", "READER_BUSY"},
		{"known type beats status", 404, `{"errors":{"type":"READER_BUSY"}}`, "reader_busy", "READER_BUSY"},
		{"garbage body 422", 422, `<html>nope`, "reader_start_failed", "none"},
		{"garbage body 401", 401, `<html>nope`, "auth_failed", "none"},
		{"free text never leaks", 400, `{"message":"Card 4111 declined by moon"}`, "reader_start_failed", "none"},
		{"url affiliate", 403, `{"type":"https://developer.sumup.com/problem/invalid-affiliate-key"}`, "auth_failed", "INVALID_AFFILIATE_KEY"},
		{"url busy", 422, `{"type":"https://developer.sumup.com/problem/reader-busy"}`, "reader_busy", "READER_BUSY"},
		{"url offline", 422, `{"type":"https://developer.sumup.com/problem/reader-offline"}`, "reader_offline", "READER_OFFLINE"},
		{"url not found", 400, `{"type":"https://developer.sumup.com/problem/not-found"}`, "no_reader", "NOT_FOUND"},
		{"merchant other type", 400, `{"errors":{"type":"MERCHANT_SUSPENDED"}}`, "reader_start_failed", "MERCHANT_SUSPENDED"},
		{"type sanitised", 400, `{"type":"BAD TYPE!\n-x"}`, "reader_start_failed", "BADTYPE_X"},
	}
	for _, c := range cases {
		code, typ := readerStartDecline(c.status, []byte(c.body))
		if code != c.wantCode || typ != c.wantType {
			t.Errorf("%s: got (%q,%q), want (%q,%q)", c.name, code, typ, c.wantCode, c.wantType)
		}
	}
	// the log shows only the sanitised slug, not the URL
	var logs []string
	logHook = func(m string) { logs = append(logs, m) }
	t.Cleanup(func() { logHook = nil })
	c, ty := readerStartDecline(422, []byte(`{"type":"https://developer.sumup.com/problem/reader-busy"}`))
	logReaderRefusal(422, c, ty)
	if l := strings.Join(logs, ""); !strings.Contains(l, "READER_BUSY") || strings.Contains(l, "sumup.com") {
		t.Errorf("log = %q", l)
	}
	long := strings.Repeat("A", 200)
	if _, typ := readerStartDecline(400, []byte(`{"type":"`+long+`"}`)); len(typ) != 64 {
		t.Errorf("type not capped at 64: %d", len(typ))
	}
}

// run() over the 422 READER_BUSY: reader_busy, and the log line names the
// SumUp type but never its detail text.
func TestReaderBusyDeclineAndLog(t *testing.T) {
	s, clk, _, mk := contractWorld(t)
	s.pending, s.startedAt = true, clk.now()
	var logs []string
	logHook = func(m string) { logs = append(logs, m) }
	t.Cleanup(func() { logHook = nil })
	out := runReader(t, mk("att:0:ab"), clk)
	if out.approved || out.reason != "reader_busy" {
		t.Fatalf("got %+v, want declined reader_busy", out)
	}
	all := strings.Join(logs, "\n")
	if !strings.Contains(all, "422") || !strings.Contains(all, "READER_BUSY") {
		t.Fatalf("log lacks status/type: %q", all)
	}
	if strings.Contains(all, "pending checkout") || strings.Contains(all, "sandbox-key-not-real") {
		t.Fatalf("log leaks detail or key: %q", all)
	}
}
