# Review: SumUp reader-start refusals get specific decline codes (ut-docs#6961)

Card: universaltill/ut-docs#6961 (bug, p2, complexity:easy). Version 1.9.2 → 1.10.0 (minor: new decline codes).

## Problem
On a definite 4xx from `POST /v0.1/merchants/{code}/readers/{id}/checkout`, `run()` declined with
`firstNonEmpty(checkout.Message, "reader_start_failed")`. SumUp answers
`{"errors":{"type":"READER_BUSY","detail":"..."}}` (seen live 2026-11-23, three declines on one till), so
every refusal became `reader_start_failed`. When a body did carry `message`, SumUp's free text became the
`decline_code`.

## What shipped
- `src/readerdecline.go` (untagged): `readerStartDecline(status, body)` reads the nested `{errors:{type}}`,
  flat `{type}` and RFC 9457 problem+json URL types (last path segment, `-`→`_`, uppercased). It returns
  only fixed slugs: `READER_BUSY`/409 → `reader_busy`; `*OFFLINE*`/`*NOT_CONNECTED*` → `reader_offline`;
  `*AFFILIATE*`/`API_KEY`/`UNAUTHORIZED`/`FORBIDDEN`/`MERCHANT_MISMATCH`/401/403 → `auth_failed`;
  `NOT_FOUND`/`READER_NOT_FOUND`/404 → `no_reader`; else `reader_start_failed`. A known type beats the
  status. These are the provider-neutral codes the till maps to cashier text (universal-till PR #2699,
  ut-docs#6962), so the plugin adds no locale strings.
- One log line: HTTP status, code and the sanitised type (`[A-Za-z0-9_]`, ≤64), with a ~60 s hint for
  READER_BUSY. Never the detail, the body, the key or ids.
- `src/reader.go`: the 4xx branch uses the mapper. The attempt is still cleared, and the 2xx, 5xx and
  transport paths are unchanged.
- Tests: `TestReaderStartDecline` (table test with live-style bodies), `TestReaderBusyDeclineAndLog`,
  `TestNestedReaderBusyThroughRun`, `TestContractReaderBusyIsRefusal`. `TestRejectedStartClearsAttempt`
  now expects `reader_start_failed` instead of free text.
- README and CLAUDE.md notes; `manifest.json` is 1.10.0.

## Review
Sonnet authored. Opus reviewed independently (one round plus a re-check of the fixes). Sonnet tested as a separate agent.
- **Safety, checked:** no refusal can map to `outcome_unknown`, and no started checkout is read as a refusal.
  SumUp's OpenAPI (sumup-go, CreateReaderCheckout) lists only 201/400/401/404/422, and every one of those
  4xx is a refusal. So clearing the attempt on a 4xx is still correct.
- **F1 (medium, fixed):** problem+json URL types (`.../problem/reader-busy`) never matched, because the URL was stripped
  to junk. They are now normalised, with table rows for busy, affiliate, offline and not-found.
- **F2 (low, fixed):** the nested live body now goes through `run()` (`TestNestedReaderBusyThroughRun`), which
  also asserts that the attempt Ref is cleared.
- **F3 (low, documented):** a 404 has no type, so a wrong merchant code also shows `no_reader`. Nothing is charged.
- **F4 (low, fixed):** "contains MERCHANT" was too broad. Only `MERCHANT_MISMATCH` matches now (`MERCHANT_SUSPENDED` gives the generic code).
- **F5 (info, fixed):** a comment on the 409 mapping. 409 is undocumented for this call, and "already started" is ruled out because
  the foreign id includes the start second.
- **F6 (info, pre-existing, out of scope):** `status < 200 || status >= 300` also treats a 3xx as a definite
  refusal. Recorded on ut-docs#6961 as a follow-up.

## Verification
- `gofmt -l` empty; `go vet ./...` native and `GOOS=wasip1 GOARCH=wasm`; `go test -race ./...`;
  `scripts/build.sh`; `scripts/validate.sh` (`ok … v1.10.0`). Tester and orchestrator both ran these.
- Red → green: with `readerStartDecline` stubbed to return `reader_start_failed`, all four new tests fail.
  The orchestrator re-checked this in a throwaway copy. A second mutation (detail written into the log)
  fails `TestReaderBusyDeclineAndLog`.
- Not done: a driven run of the built `.wasm` against a fake SumUp. `apiBase` is a compile-time
  `https://api.sumup.com` and the manifest allows only that host, so it would test a patched artifact. No live
  sandbox key in this session.

Pre-flight: till user-visible behaviour n/a here (cashier text lives in the till, universal-till PR #2699 /
ut-docs#6962; `docs/plugin_guidelines.md` there lists the codes); `reference/` n/a (no contract change);
README updated; CLAUDE.md updated; `en.json` key n/a; manifest field n/a (version only); review record this
file; follow-up: F6 (3xx counts as a definite refusal) noted on ut-docs#6961.
