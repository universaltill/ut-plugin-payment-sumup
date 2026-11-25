# Review: only a 4xx reader-checkout reply is a definite refusal (ut-docs#6987)

Card: universaltill/ut-docs#6987 (p3, payments, complexity:easy). Version 1.10.0 → 1.10.1 (patch: defensive hardening).
Follow-up of #6961 (review finding F6).

Pre-flight: branch `fix/6987-reader-start-non4xx` from `main` @ `c8db159`; card claimed by lane:cloud-41; no other branch or PR for #6987.

## Problem
`run()` step 1 treated every non-2xx reply to `POST /v0.1/merchants/{code}/readers/{id}/checkout` that was
not 5xx and not a transport failure as a definite refusal: it cleared the attempt record and declined with
no terminate and no lookup. A 1xx/3xx (for example, the host HTTP client no longer following a redirect)
would drop the record of a checkout that may have reached the reader, so a re-tap could start a second one.
SumUp documents only 201/400/401/404/422 for this call, so nobody has seen this fail. The fix is hardening.

## What shipped
- `src/reader.go`: `definiteRefusal := ok && 400 <= status < 500`. Every other non-2xx (1xx, 3xx, 5xx, out
  of range) takes the existing lost-reply path: terminate the reader, look up by foreign id, approve only on
  SUCCESSFUL, else `network_error`. The attempt record (Ref = foreign id, saved before the POST) is kept.
  `readerStartDecline` / `logReaderRefusal` now run for 4xx only. The hardDeadline rule is unchanged.
- Tests in `src/reader_test.go`:
  - `TestRedirectOnStartIsNotADefiniteRefusal` (302: terminate called, attempt Ref kept, `network_error`).
  - `TestRedirectOnStartWithTapIsApproved` (302 + tap: approved, charged once, only possible through the
    foreign-id lookup).
  - `TestStartStatusBoundaryDecidesRefusalVersusLostReply` (table, added by the Tester): 100, 199, 300, 399,
    500 and 600 take the lost path; 400 and 499 are refusals.
- `manifest.json` 1.10.1; one line each in CLAUDE.md and README.md.

## Process
- Sonnet authored (Dev).
- Sonnet tested as a separate agent. The two new tests fail on the old `reader.go` with
  `reason:reader_start_failed` and pass with the fix. Mutants `< 499` and `>= 300` each fail the boundary
  table. `go test ./... -race`, vet, gofmt, `scripts/build.sh` and `scripts/validate.sh` all pass.
- There was no driven run against a real SumUp reader, because this session has no sandbox credentials. The
  evidence is the fake-server unit tests.
- Opus reviewed independently, in one round.

## Findings
| # | Sev | Finding | Outcome |
|---|---|---|---|
| 1 | Low | The 302 test's comment claimed it checks the foreign-id lookup, but it only checks the POSTed foreign id | Fixed: the comment now says what it asserts and points at the tap test that proves the lookup |
| 2 | Low | Only 302 was covered; the design names more boundaries | Fixed: the Tester's boundary table |
| 3 | Nit | One rewrapped comment line was over-long | Fixed |

The reviewer found no money-correctness problem. The only approval is still `paidReader` after a SUCCESSFUL
lookup. The change only moves 1xx/3xx/out-of-range replies from "drop the record" to "keep the record and
look it up", which is the safer side. After the fixes (comments and tests only), the full suite was re-run
and is green.
