# 2026-10-25 - Selectable currency, default = shop currency (ut-docs#5029)

**Card:** universaltill/ut-docs#5029 - **Branch:** `feat/5029-selectable-currency` - **Author:** Sonnet 5.5 - **Reviewer:** Fable (independent, different model; money-touching)

## What shipped (v1.1.2 -> v1.2.0)
- `currency` is a host `choice` setting, default `auto`; 15 options (symbol + code + name) + Automatic, labels in `locales/{en,de,tr}.json`.
- `src/currency.go` (untagged, `go test`ed): `resolveCurrency` = saved pick > sale currency > shop currency (`shop.context.v2` through `view_query`, `view:shop`, `views_used`); declines `currency_mismatch`, `currency_unknown`, `unsupported_currency`, `unsupported_decimals`. Test mode (no reader) only displays a currency and never declines.
- Zero-decimal CLP/COP/HUF excluded on purpose (SumUp Reader docs: minor_unit 0; unverifiable without a live sandbox). README "Currencies".
- No core change: shop currency comes from the existing neutral view.

## Review findings (Fable)
| # | Sev | Finding | Outcome |
|---|---|---|---|
| 1 | High | The tender authorize event carries no currency (host sends method/amount/reference), so with `view:shop` unavailable `auto` fell to `eur` and billed a UK sale in euros silently | Fixed - `currency_unknown` decline on a real charge; eur only displayed in test mode; tests added |
| 2 | Med | manifest description said a pick "unless you pick another" - contradicts the guard | Fixed |
| 3, 4 | Med | README overstated the guard / legacy `eur` upgrade behaviour | Fixed - states the event carries no currency, the dependence on the view, and the explicit-pick caveat |
| 5 | Low | `shopContext` fixed 8 KiB buffer | Fixed - one re-query with the full length |
| 6 | Low | `min_pos_version` is not enforced by the host (hardcoded) | Accepted; README wording softened; fail-closed path is the real protection |
| 7 | Low | view called in test mode too (audit noise if not granted) | Accepted: test mode then shows the shop currency correctly; permission is requested by the manifest |
| 8, 9 | Low | CHF label doubled code; EN "Euro" case; TR Romen leyi / Çek korunası | Fixed |
| 11, 12 | Info | README: Checkouts enum lacks AUD/CAD/MXN/PEN; localization paragraph run-on | Fixed |
| 13 | Info | `main.go` wiring has no host-independent test | Accepted; covered by a driven run (below) |

Explicit saved pick + view unavailable still charges that pick (requirement: an explicit choice always wins); a legacy install that saved the old default `eur` is therefore only protected while `view:shop` is granted. Documented.

## Verified beyond unit tests
- Mutation checks (reviewer): dropping the mismatch check, changing the fallback, dropping the decimals check, sale-currency precedence and adding HUF each fail the tests.
- Driven run of the built `bin/plugin.wasm` in `universal-till`'s real `WasmRuntime` (wazero, throwaway test, not committed, no network - declines occur before any SumUp call): UK shop auto -> gbp (demo approve; reader reaches the affiliate check as gbp), stale saved eur in a GBP shop -> `currency_mismatch`, event currency mismatch -> decline, CLP shop -> `unsupported_currency`, view not granted + auto + reader -> `currency_unknown` (final build). The test-mode/UK case was run on the build before the last small revert (test mode calls the view again); that revert restores the behaviour verified earlier.
- Host `ParseManifest` accepts the manifest.
- Gate: `GOOS=wasip1 GOARCH=wasm go vet`, `gofmt -l`, `go test ./...`, `build.sh`, `validate.sh`, `guard-plugin-i18n.sh`, `package.sh` clean.

## Not verified
- Anything against a live SumUp account or reader (no credentials used); AUD/CAD/MXN/PEN/BRL/PEN support on the Reader API.
- A real till UI render of the dropdown (no screenshot taken).

## Deferred
- Host: add `currency` to the tender authorize payload (`universal-till/internal/pages/pos_api.go`) - runtime follow-up card; `min_pos_version` is not enforced by the installer.
- Zero-decimal currency support, with a live sandbox.

Verdict: safe to merge after CI.
