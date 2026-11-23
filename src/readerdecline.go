package main

// Reader-start refusals (ut-docs#6961). A definite 4xx from the reader
// checkout call becomes one of a FIXED set of provider-neutral decline
// codes the till maps to cashier text; SumUp's own text is never used as a
// code and never logged (R3/R4).

import (
	"encoding/json"
	"strings"
)

// readerStartDecline maps a 4xx checkout reply to a decline code plus the
// sanitised SumUp error type ("none" when absent). It reads both the
// documented {"errors":{"type","detail"}} and the flat {"type","message"}.
// A recognised type wins over the HTTP status.
func readerStartDecline(status int, body []byte) (code, sumupType string) {
	var b struct {
		Errors struct {
			Type string `json:"type"`
		} `json:"errors"`
		Type string `json:"type"`
	}
	_ = json.Unmarshal(body, &b) // unparseable: fall back on the status
	raw := b.Errors.Type
	if raw == "" {
		raw = b.Type
	}
	sumupType = sanitizeType(raw)
	if sumupType == "" {
		sumupType = "none"
	}
	t := strings.ToUpper(sumupType)
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(t, s) {
				return true
			}
		}
		return false
	}
	switch {
	case t == "READER_BUSY":
		return "reader_busy", sumupType
	case has("OFFLINE", "NOT_CONNECTED"):
		return "reader_offline", sumupType
	case has("AFFILIATE", "API_KEY", "UNAUTHORIZED", "FORBIDDEN", "MERCHANT"):
		return "auth_failed", sumupType
	case t == "NOT_FOUND" || t == "READER_NOT_FOUND":
		return "no_reader", sumupType
	case status == 401 || status == 403:
		return "auth_failed", sumupType
	case status == 404:
		return "no_reader", sumupType
	case status == 409:
		return "reader_busy", sumupType
	}
	return "reader_start_failed", sumupType
}

// sanitizeType keeps [A-Za-z0-9_] and caps the result at 64 characters.
func sanitizeType(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			sb.WriteRune(r)
			if sb.Len() >= 64 {
				break
			}
		}
	}
	return sb.String()
}

// logReaderRefusal writes the one diagnostic line: status and type only.
func logReaderRefusal(status int, code, sumupType string) {
	if code == "reader_busy" {
		logf("sumup reader start refused: http %d type %s -> %s (the reader frees itself within about 60 s, the device start window)", status, sumupType, code)
		return
	}
	logf("sumup reader start refused: http %d type %s -> %s", status, sumupType, code)
}
