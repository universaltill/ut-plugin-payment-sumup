package main

import "strings"

// universalTillAffiliateKey is Universal Till's SumUp affiliate key. It is NOT
// a secret: it only attributes transactions to the affiliate account (owner
// decision, 2026-11-19, ut-docs#6659), so it is a plain constant in source. A
// new key means a new constant and a new release.
const universalTillAffiliateKey = "sup_afk_WZUeAmqLiavIAMycWzM7aa7CQuTmLoS7"

// builtinAffiliateKey is the value affiliateKey() reads. It starts as the
// constant; tests override it to exercise the blank-key decline path.
var builtinAffiliateKey = universalTillAffiliateKey

// affiliateKey returns the built-in key, trimmed ("" only if the constant
// were ever blanked: reader payments then decline no_affiliate_key).
func affiliateKey() string { return strings.TrimSpace(builtinAffiliateKey) }

// affiliateConfig is the affiliate pair a reader checkout sends to SumUp:
// the built-in key (never a setting) and the app id the merchant's settings
// carry (default com.universaltill.pos). Untagged so a plain `go test` pins
// what main.go wires into readerCharger.
func affiliateConfig(setting func(string) string) (key, appID string) {
	return affiliateKey(), strings.TrimSpace(setting("sumup_app_id"))
}
