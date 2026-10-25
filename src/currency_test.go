package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestParseShopContext(t *testing.T) {
	code, dec, ok := parseShopContext([]byte(`[{"store_name":"x","currency_code":"GBP","currency_decimals":2,"locale":"en-gb"}]`))
	if !ok || code != "GBP" || dec != 2 {
		t.Fatalf("got %q %d %v", code, dec, ok)
	}
	for _, bad := range []string{``, `[]`, `{}`, `[{"currency_code":""}]`, `nonsense`} {
		if _, _, ok := parseShopContext([]byte(bad)); ok {
			t.Errorf("%q should not parse", bad)
		}
	}
}

func TestResolveCurrency(t *testing.T) {
	gb := shopInfo{Code: "GBP", Decimals: 2, Known: true}
	cases := []struct {
		name, saved, sale string
		shop              shopInfo
		want, decline     string
	}{
		{"UK shop, nothing saved -> GBP", "", "", gb, "gbp", ""},
		{"UK shop, default auto -> GBP", "auto", "", gb, "gbp", ""},
		{"auto is case-insensitive", " AUTO ", "", gb, "gbp", ""},
		{"nothing known -> decline, never guess", "", "", shopInfo{}, "", "currency_unknown"},
		{"auto, nothing known -> decline", "auto", "", shopInfo{}, "", "currency_unknown"},
		{"saved choice wins when it matches the shop", "GBP", "", gb, "gbp", ""},
		{"saved eur in a GBP shop is declined, not charged", "eur", "", gb, "", "currency_mismatch"},
		{"saved choice wins when shop unknown", "usd", "", shopInfo{}, "usd", ""},
		{"sale currency present and matching", "auto", "GBP", gb, "gbp", ""},
		{"sale currency present, saved differs", "eur", "gbp", shopInfo{}, "", "currency_mismatch"},
		{"sale currency beats shop for default", "", "eur", gb, "eur", ""},
		{"sale currency only", "", "eur", shopInfo{}, "eur", ""},
		{"zero-decimal saved is unsupported", "huf", "", shopInfo{}, "", "unsupported_currency"},
		{"zero-decimal shop is unsupported", "auto", "", shopInfo{Code: "CLP", Decimals: 0, Known: true}, "", "unsupported_currency"},
		{"garbage saved value is unsupported", "xx", "", shopInfo{}, "", "unsupported_currency"},
		{"shop currency SumUp lacks", "auto", "", shopInfo{Code: "IRR", Decimals: 0, Known: true}, "", "unsupported_currency"},
		{"shop with non-2 decimals declines", "gbp", "", shopInfo{Code: "GBP", Decimals: 3, Known: true}, "", "unsupported_decimals"},
	}
	for _, c := range cases {
		got, decline := resolveCurrency(c.saved, c.sale, c.shop)
		if got != c.want || decline != c.decline {
			t.Errorf("%s: got (%q,%q), want (%q,%q)", c.name, got, decline, c.want, c.decline)
		}
	}
}

// The picker list: exactly SumUp's glossary currencies minus the 0-decimal
// ones, and the manifest select offers exactly that, in the same order.
func TestPickerMatchesManifestAndLocales(t *testing.T) {
	if got := strings.Join(supportedCurrencies, " "); got != "AUD BRL CAD CHF CZK DKK EUR GBP MXN NOK PEN PLN RON SEK USD" {
		t.Fatalf("supportedCurrencies = %s", got)
	}
	for _, z := range []string{"CLP", "COP", "HUF"} {
		if isSupportedCurrency(z) {
			t.Errorf("%s is zero-decimal and must not be selectable", z)
		}
	}
	raw, err := os.ReadFile("../manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Locales  []string `json:"locales"`
		Settings []struct {
			Key, Type, Label, Help string
			DefaultValue           string `json:"default_value"`
			Options                []struct{ Value, Label string }
		}
		ViewsUsed   []string `json:"views_used"`
		Permissions []string
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, s := range m.Settings {
		if s.Key != "currency" {
			continue
		}
		found = true
		if s.Type != "choice" || s.DefaultValue != "auto" {
			t.Fatalf("currency must be a choice defaulting to auto, got %q/%q", s.Type, s.DefaultValue)
		}
		want := append([]string{"auto"}, lower(supportedCurrencies)...)
		if len(s.Options) != len(want) {
			t.Fatalf("options = %d, want %d", len(s.Options), len(want))
		}
		locales := loadLocales(t, m.Locales)
		keys := []string{s.Label, s.Help}
		for i, o := range s.Options {
			if o.Value != want[i] {
				t.Errorf("option %d = %q, want %q", i, o.Value, want[i])
			}
			keys = append(keys, o.Label)
		}
		for loc, kv := range locales {
			for _, k := range keys {
				if strings.TrimSpace(kv[k]) == "" {
					t.Errorf("locale %s lacks %q", loc, k)
				}
			}
		}
		// symbol + code + name in the label
		for _, c := range supportedCurrencies {
			for loc, kv := range locales {
				l := kv["payment_sumup.currency.opt."+strings.ToLower(c)]
				if !strings.Contains(l, c) || len(strings.Fields(l)) < 3 {
					t.Errorf("%s label for %s lacks symbol/code/name: %q", loc, c, l)
				}
			}
		}
	}
	if !found {
		t.Fatal("no currency setting")
	}
	if len(m.ViewsUsed) != 1 || m.ViewsUsed[0] != "shop.context.v2" {
		t.Errorf("views_used = %v", m.ViewsUsed)
	}
	has := false
	for _, p := range m.Permissions {
		has = has || p == "view:shop"
	}
	if !has {
		t.Error("view:shop permission missing")
	}
}

func lower(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(s)
	}
	return out
}

func loadLocales(t *testing.T, manifestLocales []string) map[string]map[string]string {
	t.Helper()
	out := map[string]map[string]string{}
	for _, l := range manifestLocales {
		code := strings.ToLower(strings.SplitN(l, "-", 2)[0])
		raw, err := os.ReadFile("../locales/" + code + ".json")
		if err != nil {
			t.Fatalf("manifest ships %s but %v", l, err)
		}
		kv := map[string]string{}
		if err := json.Unmarshal(raw, &kv); err != nil {
			t.Fatal(err)
		}
		out[code] = kv
	}
	for _, need := range []string{"en", "de", "tr"} {
		if _, ok := out[need]; !ok {
			t.Fatalf("locale %s must ship", need)
		}
	}
	return out
}

func TestDeclinedCurrencyNeverReportsAuto(t *testing.T) {
	gb := shopInfo{Code: "GBP", Decimals: 2, Known: true}
	for _, c := range []struct {
		saved, sale string
		shop        shopInfo
		want        string
	}{
		{"auto", "", gb, "gbp"}, {"eur", "", gb, "gbp"}, {"eur", "usd", gb, "usd"},
		{"huf", "", shopInfo{}, "huf"}, {"auto", "", shopInfo{}, "eur"},
	} {
		if got := declinedCurrency(c.saved, c.sale, c.shop); got != c.want {
			t.Errorf("%+v: got %q want %q", c, got, c.want)
		}
	}
}
