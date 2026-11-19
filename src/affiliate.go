package main

import "strings"

// builtinAffiliateKey is Universal Till's SumUp affiliate key, set at link
// time by scripts/build.sh (-ldflags "-X main.builtinAffiliateKey=...") from
// the SUMUP_AFFILIATE_KEY build environment variable (ut-docs#6659). It is ""
// in CI, PR and test builds: a keyless build declines every reader payment
// with no_affiliate_key and sends nothing to SumUp. The value must never be
// logged, echoed or committed.
var builtinAffiliateKey string

// affiliateKey returns the built-in key, trimmed ("" when keyless).
func affiliateKey() string { return strings.TrimSpace(builtinAffiliateKey) }

// affiliateConfig is the affiliate pair a reader checkout sends to SumUp:
// the built-in key (never a setting) and the app id the merchant's settings
// carry (default com.universaltill.pos). Untagged so a plain `go test` pins
// what main.go wires into readerCharger.
func affiliateConfig(setting func(string) string) (key, appID string) {
	return affiliateKey(), strings.TrimSpace(setting("sumup_app_id"))
}
