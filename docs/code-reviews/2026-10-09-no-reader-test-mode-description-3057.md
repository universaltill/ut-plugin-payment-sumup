# 2026-10-09 — No-reader description says test mode, not a hosted checkout link (ut-docs#3057)

**Card:** universaltill/ut-docs#3057 · **Branch:** `fix/3057-no-reader-demo-description` · **Author:** Opus 5.5 · **Reviewer:** Fable (independent, different model)

## What shipped
- `manifest.json` description no longer promises "a SumUp hosted checkout link" with no reader. It now says that without `sumup_reader_id` the plugin runs in test mode: no card is charged, nothing is sent to SumUp, amounts ending .13 decline, .99 time out, anything else approves. The refund sentence names reader sales and says a test-mode refund is approved with nothing to send back. Version 1.1.1 → 1.1.2 so auto-tag-release publishes the corrected listing copy.
- Implementing a hosted link was not the fix: README "Design note" — SumUp has no synchronous online charge a blocking `authorize` could confirm.
- The no-reader decision moved from `main.go`'s inline switch into untagged `src/demo.go` (`demoAuthorize`), so host `go test` covers it.
- `src/demo_test.go`: `TestDemoAuthorize` pins the outcomes and codes; `TestManifestDescribesNoReaderAsTestMode` fails on any "hosted checkout" / "checkout link" / "payment link" promise and requires the test-mode signals.
- CI (`ci.yml`) now runs `go test ./...` — before this, the existing `convert_test.go` never ran in CI.
- `CLAUDE.md` test-coverage note updated.

## Review findings
| # | Sev | Finding | Outcome |
|---|---|---|---|
| 1 | Low | Test over-pinned the copy (five exact substrings) | Fixed — positive pins reduced to "test mode", ".13", ".99"; banned list kept |
| 2 | Low | Refund sentence unqualified; in test mode a refund is a no-op approve | Fixed — qualified in the description |
| 3 | Low | `CLAUDE.md` said nothing but `convert.go` has tests | Fixed |
| 4 | Info | ".13/.99" is major-unit phrasing, code checks minor % 100 — differs only for zero-decimal currencies; pre-existing, README states minor units | Accepted |
| 5 | Info | Review record missing from WIP commit | This file |
| 6 | Info | ut-docs ADR-0125's follow-up list names this mismatch | Noted on the card at close-out; a follow-up list entry, not a decision |
| 7 | Info | Release path: tag → release.yml → publish → ut-cloud listing copies the description | Listing copy is corrected by this release |

## Verified beyond automated tests
- TDD: reviewer reverted `manifest.json` to 1.1.1 — the manifest test fails on the hosted-checkout wording; restored, passes.
- Reviewer swept `demoAuthorize` against the old switch over −100,000…100,000 plus edge values: 0 mismatches (codes `demo_declined`, `demo_timeout`, `demo-<amount>`; negatives/zero unchanged).
- Driven run: the built `bin/plugin.wasm` loaded in `universal-till`'s real `WasmRuntime` (wazero) via a throwaway test (not committed): authorize 1050 → approved `demo-1050`; 1013 and 1099 → exit code 2 (declined); refund with no reader → approved `demo-refund`.
- Gate: `GOOS=wasip1 GOARCH=wasm go vet ./...`, `gofmt -l .`, `go test ./...`, `scripts/build.sh`, `validate.sh`, `guard-plugin-i18n.sh`, `package.sh` — all clean.
- No till UI surface changed (marketplace copy only), so no visual check.

## Verdict
Safe to merge once PR CI is green.

## Deferred
None.
