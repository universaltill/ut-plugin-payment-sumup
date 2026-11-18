# Per-leg refund_amount (ut-docs#6603), SumUp v1.7.0

Design: ut-docs#6603 architect note. ADR-0196 d.4 is applied as written, so no ADR is needed.

- `origPayment.RefundAmount` (`*int64`). `src/refundlegs.go` holds `perLegMode`, `planLegs` and `refundLegs`, and `refundRecord.Legs` is in `src/refund.go`.
- The legacy path is untouched when no leg carries the field, and on a reversal. v1.6.0 records are still read: `done` approves, `sending` gives `refund_outcome_unknown`.
- Tests: `src/refund_legs_test.go`. It has 13 cases, including an invalid table, a bounded record, the host value cap and masked logs. The reviewer added 3 more (below).
- Not verified against a live SumUp account (fakes only).

## Review
The Dev was Sonnet. Opus did this independent review. Verdict: **approve after fixes**. There are no must-fix findings in the plugin.

Checked and clean:
- **No double refund on retry.** Per-leg state is `""`, `sending` or `done`, and done legs are skipped. A `sending` leg is resent only when it is a full refund of that leg. A partial one gives `refund_outcome_unknown`, and a 5xx counts as unknown.
- **Mismatched record.** A stored per-leg record whose legs differ from the plan is never resumed.
- **No leg picking.** Legs run in payload order, with no fallback to the next leg.
- **Validation before any call.** This includes the demo and no-key checks.
- **Record size.** The record is still one entry per event id, 8 at most. With 16 legs it fits the host value cap (`capKV` test).
- **Amounts.** They are sent to SumUp as `minorToMajor`.
- **Logs.** They use `maskRef` and `scrubRefs` only.
- **Reversals are unchanged.**

Findings:
- **F1 (major, accepted → ut-docs#6648; regression vs today, flag on that card):** a second partial refund of a split sale, where leg A was already partly refunded, now declines. Core allocates leg A again without netting, A answers 409/422, and the refund stops. v1.6.0's big-first moved on to B and succeeded. It fails closed and nothing is over-refunded. The fix is core per-leg netting (#6648), which should note this regression.
- **F2 (minor, covered by ut-docs#6647):** if leg order changes between attempts with different event ids, a partial could over-refund (same example as the Stripe record). It is safe under a stable order, because a non-last leg is always full, so a fresh event hits SumUp's "nothing left" on the done leg first.
- **F3 (minor, fixed):** surviving mutants. A `sameLegs` skip and a `sameLegs` check that ignores the amount survived; `TestLegsRecordMismatchUnknown` (amount, ref and count) now kills both, and also asserts that the record is unchanged. The sum-overflow guard had no discriminating test; `TestLegsSumOverflowGuard` now covers it. The `maxRefundLegs` value was unpinned; `TestLegsCapIsSixteen` now pins it. Each new test went red under its mutant, checked in a throwaway copy.
- **F4 (minor, accepted, pre-existing since v1.6.0):** when `kv.put` of the `sending` record fails, the call still goes out and the failure is only logged. A lost-reply retry of a *partial* leg could then resend it. This is not new here, and the size bound is tested. A fail-closed variant would be an epic checklist line (<1 day).
- **F5 (minor, accepted):** mixed `demo-` and real legs: a demo leg counts as done, so a later real-leg failure reads as `refund_partial` although no money moved. This is not reachable in practice, because test mode is all-or-nothing per sale.
- Known and accepted, not re-raised: core shows a generic decline for `refund_partial` and `refund_outcome_unknown` (#6647).

## Verification
- **Tester:** all behavioural mutants were killed except the 3 fixed in F3. A driven run of the real built wasm in a universal-till `WasmRuntime` harness covered two legs, a replay, a partial then a retry, and an invalid split.
- **Reviewer:** removing the `perLegMode` hook turns 8 `TestLegs*` red, checked in a throwaway copy.

Pre-flight: gofmt -l (clean), go vet ./... and GOOS=wasip1 GOARCH=wasm go vet ./... (clean), go test -count=1 -race ./... (ok), validate.sh (ok v1.7.0), guard-plugin-i18n.sh (ok). Till help: n/a for the plugin part; the till text is deferred → ut-docs#6647. `reference/` contract page updated (ut-docs). README and CLAUDE.md updated. Manifest field: n/a. Follow-ups: ut-docs#6647, #6648 (F1 noted there).
