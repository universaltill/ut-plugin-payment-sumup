package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ut-docs#6659: Universal Till's affiliate key is a hard-coded constant
// (src/affiliate.go, v1.9.1). These tests only
// ever use fake values.

func setBuiltin(t *testing.T, v string) {
	t.Helper()
	old := builtinAffiliateKey
	builtinAffiliateKey = v
	t.Cleanup(func() { builtinAffiliateKey = old })
}

func TestAffiliateKeyIsHardCodedAndWellFormed(t *testing.T) {
	got := affiliateKey()
	if got == "" || got != universalTillAffiliateKey {
		t.Fatal("the shipped build must carry the hard-coded affiliate key")
	}
	for _, r := range got {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-') {
			t.Fatal("affiliate key has a character outside [A-Za-z0-9_.-]")
		}
	}
}

func TestAffiliateKeyTrimsBuiltin(t *testing.T) {
	setBuiltin(t, " sup_aff_test\n")
	if got := affiliateKey(); got != "sup_aff_test" {
		t.Fatalf("affiliateKey() = %q, want trimmed sup_aff_test", got)
	}
}

func TestReaderCheckoutUsesBuiltinAffiliateKey(t *testing.T) {
	setBuiltin(t, "builtin-test")
	clk, f, kv := newWorld()
	f.tapAt = 2 * time.Second
	c := newReaderCharger(f, kv, "att:0:ab")
	c.affiliateKey, _ = affiliateConfig(func(string) string { return "" })
	runReader(t, c, clk)
	if got := lastAffiliate(t, f)["key"]; got != "builtin-test" {
		t.Fatalf("affiliate.key = %q, want builtin-test", got)
	}
}

// affiliateConfig is what main.go hands to the reader charger: the built-in
// key, never a setting, plus the app id from settings.
func TestAffiliateConfigWiresBuiltinKeyAndAppID(t *testing.T) {
	setBuiltin(t, " builtin-test\n")
	settings := map[string]string{
		"sumup_app_id":        " com.example.app ",
		"sumup_affiliate_key": "from-setting-must-be-ignored",
	}
	key, appID := affiliateConfig(func(k string) string { return settings[k] })
	if key != "builtin-test" {
		t.Fatalf("affiliate key = %q, want the trimmed built-in key", key)
	}
	if appID != "com.example.app" {
		t.Fatalf("app id = %q, want the trimmed setting value", appID)
	}
}

func TestAffiliateConfigBlankIsEmpty(t *testing.T) {
	setBuiltin(t, "")
	if key, _ := affiliateConfig(func(string) string { return "x" }); key != "" {
		t.Fatalf("blank-key build must give an empty key, got %q", key)
	}
}

func TestBlankKeyStillRefusesWithoutCallingSumUp(t *testing.T) {
	setBuiltin(t, "")
	clk, f, kv := newWorld()
	c := newReaderCharger(f, kv, "att:0:ab")
	c.affiliateKey, _ = affiliateConfig(func(string) string { return "" })
	out := runReader(t, c, clk)
	if out.approved || out.reason != "no_affiliate_key" || len(f.checkouts) != 0 {
		t.Fatalf("blank-key build must decline no_affiliate_key with no checkout, got %+v (%d checkouts)", out, len(f.checkouts))
	}
}

// The key goes to SumUp in the request body only; it must not leak into the
// decline reason (which is logged and shown) for any outcome.
func TestAffiliateKeyNeverInOutcomeReason(t *testing.T) {
	const secret = "SECRET-KEY-XYZ"
	clk, f, kv := newWorld()
	f.tapAt = 0 // nobody taps: times out
	c := newReaderCharger(f, kv, "att:0:ab")
	c.affiliateKey = secret
	out := runReader(t, c, clk)
	if strings.Contains(out.reason, secret) || strings.Contains(out.authCode, secret) {
		t.Fatalf("affiliate key leaked into the outcome: %+v", out)
	}
	for k, v := range kv {
		if strings.Contains(string(v), secret) {
			t.Fatalf("affiliate key persisted in plugin storage key %q", k)
		}
	}
}

// No code path may read the retired per-till setting (ut-docs#6659).
func TestNoSourceReadsAffiliateSetting(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob: %v %d", err, len(files))
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), `"sumup_affiliate_key"`) {
			t.Errorf("%s reads the retired sumup_affiliate_key setting", f)
		}
	}
}

func TestManifestHasNoAffiliateKeySetting(t *testing.T) {
	var m struct {
		Settings []struct {
			Key     string `json:"key"`
			Default string `json:"default_value"`
		} `json:"settings"`
	}
	readJSON(t, "../manifest.json", &m)
	var en map[string]string
	readJSON(t, "../locales/en.json", &en)
	hasAppID := false
	for _, s := range m.Settings {
		if s.Key == "sumup_affiliate_key" {
			t.Error("manifest still declares sumup_affiliate_key")
		}
		if s.Key == "sumup_app_id" {
			hasAppID = true
			if s.Default != "com.universaltill.pos" {
				t.Errorf("sumup_app_id default = %q", s.Default)
			}
		}
	}
	if !hasAppID {
		t.Error("sumup_app_id setting must stay")
	}
	for k := range en {
		if strings.HasPrefix(k, "payment_sumup.affiliate_key.") {
			t.Errorf("locales/en.json still has %s", k)
		}
	}
}
