# 2026-11-19 — Built-in SumUp affiliate key (v1.9.0, ut-docs#6659 slice 1)

**What shipped.** Per the owner's decision of 2026-11-18, the plugin carries Universal Till's SumUp affiliate key, built in at build time. The per-till `sumup_affiliate_key` setting is gone from the manifest and locales.
- `src/affiliate.go` holds `builtinAffiliateKey`, which `scripts/build.sh` sets with `-ldflags -X` from the env var `SUMUP_AFFILIATE_KEY`. The build trims the value and checks it against `^[A-Za-z0-9_.-]+$`. It prints only "present" or "absent", never the value.
- `affiliateConfig()` builds the reader's key and app id, and `main.go` calls it. `sumup_app_id` stays a setting, default `com.universaltill.pos`.
- `release.yml` passes the repo secret `SUMUP_AFFILIATE_KEY` to the build. A guard step fails tag runs and publish runs when the secret is empty. PR CI and dry runs build without a key, and a build without a key still declines `no_affiliate_key` with no call to SumUp.
- Docs updated: README, CLAUDE.md, the till help `payments.md` (universal-till), and in ut-docs `guides/uk-pilot-card-payments.md` and `reference/store-payment-statements.md`.

**Exposure.** The key can be extracted from the public plugin. The owner accepted this on 2026-11-18. The key is bound to the app id, and rotating it means a plugin release. Agents never read, print or commit the key, and every test and run used fake values only.

**Process.** Sonnet did BA/Architect, Dev and Tester. The independent review was by Opus 5.5 (a different model).

**Tester.** PASS. Gate run: gofmt, wasip1 vet, `go test`, shellcheck, validate, i18n guard, package, version-bump guard (1.8.1 → 1.9.0, run in a throwaway clone).
- Mutations, each one turned a test red: reading the setting again, an empty `affiliateKey()`, dropping TrimSpace, re-adding the manifest setting or locale key.
- Driven run: the built wasm ran through universal-till's real `WasmRuntime` (wazero, real egress gate) against a TLS fake of api.sumup.com.
  - Keyed build: checkout `affiliate = {app_id: com.universaltill.pos, key: <fake>}`, approved.
  - Keyless build: declined `no_affiliate_key` with 0 HTTP calls.
- Not covered: the signed bundled-install path, and real SumUp.

**Review findings.** Verdict: approve after fixes. All findings were fixed before commit.
- M1 (major): `reference/store-payment-statements.md` still said the affiliate key is entered in settings and is never in the package. Rewritten.
- M2 (minor): the `main.go` wiring was covered only by a source grep. It moved into the tested `affiliateConfig()` helper; a mutation of the helper turns two tests red.
- M3 (minor, pre-existing): "no reader id = demo mode" was stale since v1.6.0 (test mode is the explicit `sumup_test_mode` setting). Fixed in the till help, the UK pilot guide and the README.
- M4 (minor): a secret with surrounding whitespace passed the guard but failed the build. Both now trim.
- N1: `app_id.help` referred to a key the merchant no longer sees. Reworded.
- N2: renamed the release guard step to cover tag and publish runs.
- The reviewer re-verified a TDD claim: restoring the setting read in `main.go` turns `TestNoSourceReadsAffiliateSetting` red.

**Pre-flight:**
- Plugin settings and locales: updated (manifest, `en.json`, settings-labels test).
- Till user-visible behaviour: `web/help/en/payments.md` updated. No screenshot of plugin settings was affected (text-only topic change).
- `reference/` and guides: updated.
- Plan, price, platform and ADR surfaces: n/a. No ADR was needed (ADR-0125 already builds its own affiliate key from a CI secret).
- New `en.json` keys: n/a (keys removed, one help string reworded).
- Review record: this file.

**Deferred.** All three are tracked on ut-docs#6659, which stays open.
- Slice 2 (SUPERSEDED by v1.9.1: the key is hard-coded, no secret to set): the owner sets the GitHub secret `SUMUP_AFFILIATE_KEY` from Key Vault `sumup-affiliate-key`, then re-runs the v1.9.0 release. Until then the auto-tagged release fails at the guard, as intended.
- Release note.
- Slice 3: sandbox and live reader verification.

A value saved in a till's old setting stays in the host DB and is never read.
