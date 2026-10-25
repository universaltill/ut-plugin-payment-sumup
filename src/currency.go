// No //go:build tag: pure currency logic, testable under a plain `go test`
// (see convert.go for why this plugin keeps its logic out of main.go).
package main

import (
	"encoding/json"
	"strings"
)

// supportedCurrencies is the picker: the currencies of SumUp's merchant
// markets (developer.sumup.com "Countries and Currencies", checked
// 2026-10-25) that use 2 minor-unit decimals. SumUp's Reader Checkout API
// documents "For the currencies CLP, COP and HUF, the minor unit is 0";
// those three are deliberately NOT offered (see README "Currencies"):
// minorToMajor and the reader request assume 2 decimals and a
// 0-decimal path cannot be checked without a live SumUp sandbox.
var supportedCurrencies = []string{
	"AUD", "BRL", "CAD", "CHF", "CZK", "DKK", "EUR", "GBP",
	"MXN", "NOK", "PEN", "PLN", "RON", "SEK", "USD",
}

// currencyAuto is the setting's default: "use the shop's own currency".
const currencyAuto = "auto"

// fallbackCurrency is shown in test mode (nothing is charged) when nothing
// at all is known; a real charge declines instead (currency_unknown).
const fallbackCurrency = "eur"

func isSupportedCurrency(code string) bool {
	code = strings.ToUpper(strings.TrimSpace(code))
	for _, c := range supportedCurrencies {
		if c == code {
			return true
		}
	}
	return false
}

// shopInfo is what the core view shop.context.v2 reports about the till.
type shopInfo struct {
	Code     string // ISO 4217, upper case
	Decimals int
	Known    bool
}

// parseShopContext decodes shop.context.v2's one-row JSON array.
func parseShopContext(body []byte) (code string, decimals int, ok bool) {
	var rows []struct {
		Code     string `json:"currency_code"`
		Decimals int    `json:"currency_decimals"`
	}
	if err := json.Unmarshal(body, &rows); err != nil || len(rows) == 0 {
		return "", 0, false
	}
	code = strings.ToUpper(strings.TrimSpace(rows[0].Code))
	if code == "" {
		return "", 0, false
	}
	return code, rows[0].Decimals, true
}

// resolveCurrency picks the currency to charge (lower case, as this plugin
// has always reported it) or returns a decline code. Order: an explicit
// saved choice, else the sale's own currency, else the shop's currency;
// with none of them known it declines currency_unknown (callers that
// charge nothing, i.e. test mode, use fallbackCurrency for display). Whatever is chosen must be one SumUp supports here and must
// equal the sale's currency (the event's if it carries one, else the
// shop's): amounts are minor units of THAT currency, so charging another
// would bill the wrong money.
func resolveCurrency(saved, saleCurrency string, shop shopInfo) (code, decline string) {
	saved = strings.ToUpper(strings.TrimSpace(saved))
	sale := strings.ToUpper(strings.TrimSpace(saleCurrency))
	chosen := saved
	if chosen == "" || chosen == strings.ToUpper(currencyAuto) {
		switch {
		case sale != "":
			chosen = sale
		case shop.Known:
			chosen = shop.Code
		default:
			// Nothing tells us the sale's currency (the authorize event
			// carries none today and shop.context.v2 was unavailable):
			// never guess a currency for real money.
			return "", "currency_unknown"
		}
	}
	if !isSupportedCurrency(chosen) {
		return "", "unsupported_currency"
	}
	expected := sale
	if expected == "" && shop.Known {
		expected = shop.Code
	}
	if expected != "" && expected != chosen {
		return "", "currency_mismatch"
	}
	if shop.Known && shop.Decimals != 2 {
		return "", "unsupported_decimals"
	}
	return strings.ToLower(chosen), ""
}

// declinedCurrency names the currency to report on a currency decline: the
// sale's own, else the shop's, else the saved pick (never the word "auto").
func declinedCurrency(saved, saleCurrency string, shop shopInfo) string {
	saved = strings.TrimSpace(saved)
	if strings.EqualFold(saved, currencyAuto) {
		saved = ""
	}
	switch {
	case strings.TrimSpace(saleCurrency) != "":
		return strings.ToLower(strings.TrimSpace(saleCurrency))
	case shop.Known:
		return strings.ToLower(shop.Code)
	case saved != "":
		return strings.ToLower(saved)
	}
	return fallbackCurrency
}
