# Review: SumUp refund storage fails closed (ut-docs#6650)

Card: universaltill/ut-docs#6650 (complexity:easy, money correctness). Version 1.9.1 → 1.9.2 (patch: a bug fix, no new settings or manifest fields).

## Problem
SumUp's refund call (`POST /v0.1/me/refund/{txn_id}`) has no idempotency key. So the plugin writes a `refunds` record with state `sending` before the call. Before this change, a failed write was only logged, and the call went out anyway. If the reply was then lost, core retried with the same event id, found no record and refunded again. That is a double refund.

## What shipped
- `src/refund.go`: `saveRefundRecord` now returns the put error (it still logs it). In `refundSale`, a failed `sending` save declines `refund_storage_error` before any SumUp call.
- `src/refundlegs.go`: `save()` returns the error. A failed `sending` save for a leg stops before the call. It declines `refund_partial` if an earlier leg of the event is already done, and `refund_storage_error` otherwise.
- `done`/clearing saves still ignore the error. Either the call has already happened, or nothing was sent.
- Events with no event id still skip the record (unchanged).
- Review fix F1 (`src/refund.go`): in whole-amount mode, the record is now cleared after each definite "too large", before the next candidate. It is cleared again, best effort, when that candidate's `sending` save fails.
- Tests: `src/refund_storage_test.go`. It covers whole mode, per-leg mode on the first leg, per-leg mode after a done leg (partial), and F1.
- Docs: README refund paragraph and CLAUDE.md "Refund storage fails closed" note. `manifest.json` is 1.9.2.

## Review
Independent review by a different model from the author (Sonnet authored, Opus reviewed). The reviewer also acted as Tester.

- **F1 (high, fixed): false approval with no money moved, made reachable by the fail-closed return.** Whole-amount mode (no `refund_amount`) tries candidates in turn. After a definite "too large" on txn-A, the `sending` record for txn-A stayed in storage. Then the `sending` save for txn-B failed, and the new code returned `refund_storage_error`. The stale `{ref: txn-A, state: sending}` record was left behind. On the retry, the top branch of `refundSale` resent to txn-A. SumUp answered "too large" again, the plugin read that as its own earlier refund, marked it `done` and **approved txn-A with nothing refunded**. Before this change, a single failed put let the call to txn-B go out, so the bug was reachable only with storage failing persistently. The fail-closed return made a single transient failure enough.
  - Reproduced with `TestRefundStorageFailureOnLaterCandidateNoFalseApproval`. The test uses a KV that fails only the put carrying `"ref":"txn-B","state":"sending"`. On the Dev's code it failed with `approved "txn-A" with no money moved: refunded=map[txn-A:1000] calls=[txn-A txn-A]`.
  - Fix: after a definite "too large", clear the record before the next candidate, because nothing was sent on that ref. Also clear it, best effort, in the fail-closed branch. Either clear alone fixes a single failed put; the test passes with either one removed and fails with both removed. Together they also cover a short burst of failures. The redundant clear after the loop was dropped, since the last iteration already clears.
  - Side benefit: a `demo-` candidate approved after a real candidate's "too large" no longer leaves a stale `sending` record behind.
  - Residual risk (accepted): if three puts in a row fail (both clears and the `sending` save), the stale record can still survive. Before this change the same risk existed whenever storage failed persistently. Fixing it fully would need a record that names every candidate tried, which is out of scope for an easy card; for the orchestrator to note on ut-docs#6650.
- **F2 (low, fixed, docs):** CLAUDE.md said "the leg's state is restored". The restore (`rec.Legs[i].State = st`) only changes the in-memory copy, which is dropped on return. A mutation that removes it keeps the whole suite green, so it has no observable effect. What matters is that storage keeps the last saved leg states, and the note now says that. The line is harmless and kept as defensive code.
- **F3 (low, accepted):** in per-leg mode, a `demo-` leg counts as "done", so a storage failure on a later real leg declines `refund_partial`, even though no real money has moved. This matches the existing failure path (`doneBefore(len(legs))`, v1.7.0). Mixed demo/real legs only happen in test setups. The retry is still correct: the demo leg is skipped and the real leg is sent.
- **F4 (info, checked, no change):** the retry branch at the top of `refundSale`, which resends on an existing `sending` record, needs no new save. The record is already stored with the same ref and state. Its outcome saves (`done`, or clear on a definite refusal) are after-call or clearing saves, which may ignore errors.
- **F5 (info, checked, no change): the per-leg `sending` resend path.** If the save fails for a leg that was already `sending`, the plugin declines without resending. Storage still holds `sending`, so the next retry resends as before. Failing closed here is conservative, and it is correct.
- **F6 (info, checked, no change): partial writes.** The host `put` is atomic per key, and all records live in one key (`refunds`). A failed put leaves the previous list whole, so the `refund_partial` path leaves storage with the earlier legs' last saved states and the failed leg unsent.
- **F7 (info, checked, no change): core handling.** universal-till `internal/pages/refund_page.go` `refundDeclineKey` treats any code other than `refund_partial` or `refund_outcome_unknown` as the generic `refund.error.provider_declined`. So `refund_storage_error` shows as a declined refund, not as a success. The code itself is compared, never shown. `refund_partial` gets the "retry the same refund here" text, which is the right advice, since the retry continues with the remaining legs. No core change is needed.

## Verification (Tester)
- `gofmt -l .` (empty), `go vet ./...`, `GOOS=wasip1 GOARCH=wasm go vet ./...`, `go test ./...`, `scripts/build.sh`, `scripts/validate.sh` (`ok com.universaltill.payment-sumup v1.9.2`), `scripts/guard-plugin-i18n.sh`, `scripts/package.sh` (dry run), `scripts/check-version-bump.test.sh` and `check-version-bump.sh` (base HEAD~1: `1.9.1 -> 1.9.2`): all green, before and after the F1 fix.
- **Red → green (Dev's three tests):** `src/refund.go` and `src/refundlegs.go` were reverted to HEAD~1, with only the `declineRefundStorage` const added back so the code compiles. All three failed because the refund was sent:
  - Whole mode: `id="txn-A" dec="" calls=[txn-A]`.
  - Per-leg mode: `calls=[txn-A txn-B]`.
  - Partial case: `calls=[txn-A txn-B]`.
  - After restoring the files, all three passed.
- **Mutation checks:**
  - M1, the `refundSale` error ignored: whole-mode test fails.
  - M2, the `refundLegs` error ignored: both per-leg tests fail.
  - M3, the `doneBefore` → `refund_partial` branch removed: partial test fails.
  - M4, the leg-state restore removed: everything passes (F2).
  - M5, only the after-"too large" clear removed: F1 test passes.
  - M6, only the fail-branch clear removed: F1 test passes.
  - M7, both clears removed (the Dev's code): F1 test fails with the false approval shown above.
- No UI surface was touched, so no screenshots. No live SumUp sandbox run (no key, and no network to SumUp from this session).

## Verdict
Approve with the F1 fix applied. The fail-closed change is correct and its tests are real, not false-passes. F1 was a real money bug (an approved refund with no money moved), and the fail-closed return made it reachable with a single failed storage write. It is now fixed and covered by a test.

Pre-flight: till user-visible behaviour n/a (plugin only; core already maps an unknown decline code to the generic refused-refund text, so no `web/help/` change is needed); `reference/` n/a (no contract change; `decline_code` values are plugin-local); README updated; CLAUDE.md updated; `en.json` key n/a; manifest field n/a (version 1.9.2 only); review record this file; follow-up: F1's three-failures-in-a-row residual accepted, to be noted on ut-docs#6650 by the orchestrator.
