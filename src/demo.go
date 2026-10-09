// No //go:build tag, for the same reason as convert.go: the no-reader
// decision has no host-function dependency, so it is unit-testable under a
// plain `go test` as well as the wasip1 build (ut-docs#3057).
package main

import "fmt"

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
