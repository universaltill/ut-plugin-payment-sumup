package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The no-reader path is test mode only (ut-docs#3057): pin its contract so
// a change to it can't slip past the manifest description that promises it.
func TestDemoAuthorize(t *testing.T) {
	cases := []struct {
		amount   int64
		approved bool
		code     string
	}{
		{1013, false, "demo_declined"},
		{13, false, "demo_declined"},
		{1099, false, "demo_timeout"},
		{1050, true, "demo-1050"},
		{1000, true, "demo-1000"},
		{1012, true, "demo-1012"},
	}
	for _, c := range cases {
		ok, code := demoAuthorize(c.amount)
		if ok != c.approved || code != c.code {
			t.Errorf("demoAuthorize(%d) = (%v, %q), want (%v, %q)", c.amount, ok, code, c.approved, c.code)
		}
	}
}

// The marketplace shows manifest.json's description to a merchant choosing
// this plugin. It once promised "a SumUp hosted checkout link" with no
// reader while the code ran demo mode (ut-docs#3057) — a merchant could
// believe they were taking real payments. Keep the copy honest.
func TestManifestDescribesNoReaderAsTestMode(t *testing.T) {
	raw, err := os.ReadFile("../manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	d := strings.ToLower(m.Description)
	for _, banned := range []string{"hosted checkout", "checkout link", "payment link"} {
		if strings.Contains(d, banned) {
			t.Errorf("description promises %q, but with no reader the plugin runs test mode only", banned)
		}
	}
	// Positive pins stay loose so a reword doesn't go red: say it's a test
	// mode and name the two non-approving amounts.
	for _, want := range []string{"test mode", ".13", ".99"} {
		if !strings.Contains(d, want) {
			t.Errorf("description should mention %q for the no-reader test mode; got %q", want, m.Description)
		}
	}
}
