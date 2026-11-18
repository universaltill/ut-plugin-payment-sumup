// No //go:build tag, for the same reason as convert.go: the no-reader
// decision has no host-function dependency, so it is unit-testable under a
// plain `go test` as well as the wasip1 build (ut-docs#3057).
package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// demoAuthorize is the authorize outcome when no `sumup_reader_id` is set.
// SumUp has no synchronous online charge a till can confirm inside one
// blocking call (README "Design note"), so the no-reader path is a test
// mode, never a real payment: no network call, and the same deterministic
// contract as ut-plugin-payment-demo — minor units ending 13 decline, 99
// time out (decline), anything else approves. manifest.json's description
// must say the same; demo_test.go pins both.
func demoAuthorize(amount int64) (approved bool, code string) {
	switch amount % 100 {
	case 13:
		return false, "demo_declined"
	case 99:
		return false, "demo_timeout"
	default:
		return true, fmt.Sprintf("demo-%d", amount)
	}
}

// testModeSetting is the explicit switch for demo behaviour (ut-docs#6599):
// no reader or key alone never turns a real till into a demo one.
const testModeSetting = "sumup_test_mode"

func isTestMode(v string) bool { return strings.EqualFold(strings.TrimSpace(v), "on") }

// demoRefPrefix marks a reference the demo path made: nothing real behind it.
const demoRefPrefix = "demo-"

// authorizeMode decides how an authorize runs: "demo" only when test mode is
// explicitly on; otherwise a reader and an API key are required and a missing
// one is a decline (never a silent approve).
func authorizeMode(test bool, apiKey, readerID string) (mode, decline string) {
	switch {
	case test:
		return "demo", ""
	case apiKey == "":
		return "", "no_api_key"
	case readerID == "":
		return "", "no_reader"
	}
	return "reader", ""
}

// declineAnswer is the stdout JSON of a declined authorize. The till reads
// decline_code from it on the non-zero exit (outcome_unknown must reach it).
func declineAnswer(amount int64, currency, reason string) []byte {
	b, _ := json.Marshal(declineFields(amount, currency, reason))
	return b
}

func declineFields(amount int64, currency, reason string) map[string]any {
	return map[string]any{
		"provider": "sumup", "amount": amount, "currency": currency,
		"outcome": "declined", "decline_code": reason,
	}
}
