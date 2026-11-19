# Review: reader_id per call + readers state hook (ut-docs#6566), SumUp v1.8.0

Card: universaltill/ut-docs#6566 (plugin part, PR 3 of the architect's order). Design: `/tmp/tabletfix/pm-architect.md` section 4. Status: Dev done; combined Tester PASS and independent Reviewer done 2026-11-19 (sections at the end).

## What shipped
- `src/readers.go`: `validReaderID` (`^[A-Za-z0-9_-]{1,64}$`), `chooseReader`, `readerStatus` (now also behind `readerIdle`), `readerStates`, `readersAnswer`.
- `src/main.go`: authorize uses the payload `reader_id`, else `sumup_reader_id`; a malformed payload id declines `invalid_reader` (no API call, no fallback). New `payment.sumup.readers` hook, read-only, exit 0. Refunds do not read the payload reader.
- `src/host.go`: `reader_id`/`reader_kind`/`op`/`readers` on the payload; `reader_id` on `original_payments[]` (accepted, never used).
- Manifest 1.7.0 to 1.8.0, hook `payment.sumup.readers`; README and CLAUDE.md updated.

## Checks to re-verify
- Malformed ids (`rdr_1/../x`, `../`, slash, space, 65 chars, `%2f`, newline) decline with zero calls.
- No id anywhere: `no_reader` and test-mode demo unchanged (`TestChooseReaderFallsBackToSettingWhenMissing`). All existing tests unchanged and green.
- Refund with a malformed `reader_id` refunds by `transaction_ref` (`TestRefundIgnoresReaderIDOnLegs`).
- Checkout amount stays integer minor units; logs carry masked reader ids only.
- `readerIdle` refactor keeps its behavior (existing reader tests green).

## Legacy refund units (architect-flagged, verify only, NOT changed)
The plugin sends `POST /v0.1/me/refund/{txn}` with a major-unit decimal `amount` (`minorToMajor`), as in README/CLAUDE.md, and the 2026-11-17 sandbox note records a 204. developer.sumup.com today documents only the newer `POST /v1.0/merchants/{merchant_code}/payments/{transaction_id}/refunds` with `amount` as an integer in MINOR units; the v0.1 page could not be fetched, so its unit and any deprecation are unconfirmed. Result: a possible endpoint migration, separate bug card (architect F8), not this PR.

## Not verified
No physical reader: the status mapping reuses the documented `data.status`/`data.state` shape against fakes only.

## Tester (combined pass, 2026-11-19)
PASS. gofmt, `go vet` (host and `GOOS=wasip1 GOARCH=wasm`), `go test -race ./...`, `scripts/build.sh`, `validate.sh`, `guard-plugin-i18n.sh`, `package.sh`, `check-version-bump.test.sh` green. Two mutants killed (format check made permissive; invalid id falling back to the setting).

## Reviewer (independent, 2026-11-19)
Reviewed by: Fable 5.2 (independent Reviewer agent; the code was written by a Sonnet Dev agent). One combined review for ut-docs#6566 across core, both plugins and ut-docs.

Re-run personally: `go test -count=1 ./...` ok; `GOOS=wasip1 GOARCH=wasm go vet ./...` clean. Read in full: `src/readers.go`, the `main.go`/`host.go` diff, the reader id's path into every provider URL.

Findings:
- None blocking. No change made to this repo by the review.
- L1 (accepted, pre-existing): the legacy `sumup_reader_id` setting is still put into the provider URL path without a format check (only `TrimSpace`) when the till sends no `reader_id`. Unchanged behaviour, manager-entered value; core now refuses to import a malformed one into the registry. Follow-up card, not this PR.
- L2 (accepted): the `main.go` wiring (the `decline(invalid_reader)` call and the `.readers` exit path) is `wasip1`-tagged, so `go test` does not execute it; it was read line by line, and `chooseReader` / `readerStates` behind it are unit-tested. A sandbox or simulated-reader run of the packaged wasm is still owed before the release tag.
- Checked and fine: a refund never reaches `chooseReader` (the refund branch skips it), so core's top-level `reader_id` on a reversal cannot decline a refund; the `.readers` branch runs before any key check and always exits 0; the hook is read-only (GET only); per-request plugin state is keyed by the till's request id, so two readers never share an attempt record; an old till (no `reader_id`) behaves exactly as before.

Security exposure (what this change exposes):
- New input: a reader id supplied per call by the till. It is format-checked (1..64 of letters, digits, `_`, `-`) before any URL is built; a malformed one declines `invalid_reader` with zero provider calls and no fallback to another reader. No new permission, no new host function, no new secret; the manifest only gains the `payment.sumup.readers` hook.
- New outbound call: `GET` reader status, at most 16 per `.readers` call, only for ids that pass the format check.
- Logs: reader ids masked to the last 4 characters; the provider's error text is never logged on 401. No card data anywhere.

Verdict: ready to commit and open the PR. Safe to release alone (an older core never fires the hook and never sends `reader_id`). Release after the ut-docs amendments PR; multi-reader only becomes active once the core PR is released too.
