# 2026-08-09 — Release-pipeline template fixes (ut-docs#166)

## Context
ut-docs#166 (found during ut-docs#15's review) flagged three pre-existing,
byte-identical defects in this repo's release-pipeline template, shared with
`ut-plugin-integration-webhook` and `ut-plugin-faq` (the reference
implementation `ut-docs/for-plugin-developers.md` tells new plugin authors
to copy):

1. `latest.tar.gz.sha256` — and, found during this fix's own review, the
   versioned artifact's own `.sha256` sidecar too — recorded a
   `dist/`-prefixed path instead of the bare filename, so
   `sha256sum -c` fails for any self-hoster who downloads the artifact +
   checksum pair into one directory (exactly what the release notes tell
   them to do).
2. `scripts/approve.sh` reused `MARKETPLACE_UPLOAD_TOKEN` (a vendor-upload
   credential) as the bearer token for the marketplace's admin
   review/approve endpoints. Fails closed today (unset → empty header →
   `curl --fail` reddens), not currently exploitable.
3. `ci.yml`'s validate job ran no `go vet`/`gofmt` gate — CI only proved
   "compiles to wasm, manifest parses."

## Changes
- **`scripts/package.sh`**: the `sha256sum "$OUT"` line now `cd`s into
  `dist/` and hashes the bare filename, so the recorded checksum matches
  what a self-hoster actually has on disk after downloading. Fixes the
  *versioned* artifact's sidecar (bullet 1's other half, found during
  review — the original ticket only named the `latest.tar.gz` copy).
- **`.github/workflows/release.yml`**: the "Create GitHub Release" step no
  longer `cp`s the versioned `.sha256` file verbatim over
  `latest.tar.gz.sha256`; it regenerates the checksum against the renamed
  `dist/latest.tar.gz` directly (`(cd dist && sha256sum latest.tar.gz > ...)`).
- **`scripts/approve.sh`**: now prefers a `MARKETPLACE_ADMIN_TOKEN` env var,
  falling back to `MARKETPLACE_UPLOAD_TOKEN` (today's unchanged behavior)
  when unset. **This is client-side prep only** — independent review traced
  ut-cloud's actual authorization (`authorizeStaff`) and found it accepts
  *only* the upload-token value; a genuinely distinct
  `MARKETPLACE_ADMIN_TOKEN` would 401 every call today. Filed
  **universaltill/ut-docs#496** for the real ut-cloud-side fix; comments in
  both `approve.sh` and `release.yml` say explicitly not to provision a
  distinct secret until #496 ships. (The original draft printed a runtime
  `WARNING:` on every fallback — removed, since the fallback is the
  permanent expected path until #496 lands, not a transitional condition
  worth logging on every single run.)
- **`.github/workflows/ci.yml`**: added a `go vet` + `gofmt -l .` step to
  the validate job, run **cross-compiled** (`GOOS=wasip1 GOARCH=wasm`) to
  match `scripts/build.sh`'s actual target — independent review found host
  `go vet ./...` silently skips this repo's `//go:build wasip1`-tagged
  `src/main.go` entirely, vetting only `src/convert.go`. Also added
  `permissions: contents: read` (webhook's `ci.yml` already had this;
  sumup's didn't — one-line consistency fix while the file was open).

## Independent review
Fresh-context Opus review (different model from the implementer) found no
blockers; three should-fix items (checksum bug also present in the
versioned sidecar, the vet-command near-inertness above, and the misleading
admin-token warning) were folded into this change before commit. Full
findings, including verification methodology (real `sha256sum -c` repro of
both the bug and the fix, a stubbed-`curl` matrix over all four
admin/upload-token combinations, a planted-bug test proving host `go vet`
misses `src/main.go`), are in the review transcript; this record captures
the resulting diff and rationale.

## Verification
- `scripts/build.sh && scripts/validate.sh && scripts/package.sh` — green.
- `GOOS=wasip1 GOARCH=wasm go vet ./...` and `gofmt -l .` — clean at HEAD
  (confirms the new CI gate won't immediately redden).
- Reproduced the self-hoster failure against the pre-fix code
  (`sha256sum -c` → "No such file or directory") and confirmed both the
  versioned sidecar and the `latest.tar.gz` pair now pass `sha256sum -c`
  post-fix.
- Verified the token-selection logic in isolation across all 4
  admin/upload-token combinations (both unset, only upload, only admin,
  both set) — admin wins when set, falls back with unchanged behavior when
  only upload is set, no auth header when neither is set.
- `bash -n` (shell syntax) and `python3 -c "import yaml; yaml.safe_load(...)"`
  on every edited file.

## Deliberately out of scope
- Building real distinct-admin-credential support in ut-cloud —
  universaltill/ut-docs#496.
- Propagating the same fixes to the other ~12 `ut-plugin-*` repos serving a
  release artifact — universaltill/ut-docs#497.
- Adding `go test` to CI for this repo's real `src/convert_test.go` — doing
  so only here would break template byte-identity with
  `ut-plugin-integration-webhook` (whose `main.go` doesn't even build under
  the host `go test` toolchain, being wasm-import-only); left as a
  known gap, same as the original ticket already called out
  ("real behavioral tests are a bigger lift, per repo").
