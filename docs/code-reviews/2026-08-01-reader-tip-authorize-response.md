# 2026-08-01 — Reader tip auto-sync on the authorize response

ut-docs#43. This repo had no `docs/code-reviews/` before this change — the
ecosystem convention (`ut-docs/CLAUDE.md`: "every substantive change in
any repo lands with a review record") applies here too, so this is the
first one. Companion record with the `universal-till`-side half of this
change (the new `EventBus.PublishAuthorize` response path `completeTender`
reads this plugin's response through) is
`universal-till/docs/code-reviews/2026-08-01-sumup-tip-authorize-response.md`.

## What shipped

`readerCharge`/`pollTransaction` now also extract a `tip_amount` from the
SumUp Transactions API poll response (the same poll already used to learn
approve/decline) and report it — via a new `approveWithTip`, which wraps
`approve` and only emits `tip_amount` in the JSON when non-zero, so every
other `approve()` caller (demo mode, refunds) is byte-for-byte unaffected —
so `universal-till`'s `completeTender` can apply the reader-captured tip to
the payment even though the original tender request carried none.

Pure conversion logic (`minorToMajor`/`majorToMinor`, now also
`parseTipAmountMinor`/`parseTransactionPoll`) moved into a new
`src/convert.go` **without** the `wasip1` build tag `main.go` carries —
this repo's whole `src/` was previously untestable under a plain
`go test` (no host runtime, no `wasip1` toolchain in a normal `go test`
invocation); this file is the first thing in this repo real Go tests can
exercise directly.

## Independent review (opus, different model from implementation)

Ran the plugin's own gates (`scripts/build.sh`, `scripts/validate.sh`,
`go test ./src/...`, `gofmt -l`) plus `universal-till`'s full gate, and
personally re-verified TDD claims by breaking fixes and confirming the
specific tests failed with the expected message before restoring.

**1 blocking finding, fixed.** The original `pollTransaction` decoded
`tip_amount` as `*float64` on the *same* struct used to detect the
transaction's `id`/`status`. `tip_amount`'s real shape on this endpoint is
an explicitly-unverified guess (see "Needs sandbox verification" below) —
if SumUp actually returns it as an object (this file's own reader-checkout
request sends `total_amount` as exactly `{"currency":...,"minor_unit":2,
"value":...}`, at least as plausible a guess for `tip_amount`), the
**whole** `json.Unmarshal` of that transaction would fail, discarding a
real `SUCCESSFUL` transaction's `id`/`status` too — `pollTransaction`
would burn all 60 poll attempts and `readerCharge` would call
`decline(amount, currency, "reader_timeout")`, declining a card the
customer had already successfully tapped. Verified empirically by the
reviewer (a standalone probe comparing old-struct vs. new-struct parsing
against five `tip_amount` shapes).

Fixed by decoupling tip parsing from outcome detection entirely:
`tip_amount` is now decoded as `json.RawMessage` (which cannot fail
regardless of shape — it's a byte-copy, not a typed decode) on a struct
extracted into `convert.go` as `transactionPollEntry`, and interpreted
independently by `parseTipAmountMinor`, which degrades to "no tip
reported" (0) for anything it doesn't recognize rather than erroring. The
outer transaction-poll parsing itself was also extracted to
`parseTransactionPoll` specifically so this exact bug class has a direct
regression test
(`TestParseTransactionPoll_WeirdTipShapeNeverBreaksOutcomeDetection`,
covering object/string/null/absent tip_amount shapes against all three
response envelopes this plugin already defends against — wrapped, bare
array, single object).

**2 should-fix documentation-honesty findings, both fixed:**

- README/CLAUDE.md's "Needs sandbox verification" note for `tip_amount`
  claimed "safe to ship ahead of verification — worst case, a real tip
  silently doesn't sync." False as written before the fix above (it could
  decline a real payment); also didn't call out that a *type* mismatch —
  e.g. if the real field turns out to already be integer minor units, not
  a major-unit decimal — would silently inflate a tip 100x rather than
  simply not sync. Rewritten in both `README.md` and `CLAUDE.md`.
- This plugin's reader-checkout request (`readerCharge`) sends no tipping
  configuration of its own; whether the reader actually prompts for a tip
  depends entirely on tipping already being enabled in the merchant's own
  SumUp device/profile settings, and SumUp's published Cloud API docs had
  no documented per-checkout field to turn it on at write time. This
  wasn't previously disclosed anywhere. Added to `README.md`'s "Tips"
  section and a doc comment on `readerCharge`.

**1 nice-to-have, accepted, not changed:** the request-supplied core `tip`
field (from before this task) and this plugin's reported `tip_amount` are
both unbounded `int64`s with no sanity ceiling. Not a new hole this task
introduced; a shared bound (e.g. reject anything exceeding the sale total)
is reasonable future hardening for both together, not scoped to this repo
alone.

## What was verified beyond automated tests

- `bash scripts/build.sh` — the real `wasip1`/`wasm` cross-compile,
  clean, both before and after the fix.
- `bash scripts/validate.sh` — manifest still valid after the version
  bump (`1.0.0` → `1.1.0`, new backward-compatible capability).
- `gofmt -l src/*.go` and `go vet` under **both** `GOOS=wasip1
  GOARCH=wasm` and the host target — clean both ways, since this repo now
  has files that build under both.
- Personally re-verified `TestParseTipAmountMinor` and
  `TestParseTransactionPoll_WeirdTipShapeNeverBreaksOutcomeDetection`
  actually fail against the pre-fix `*float64` shape (not just that they
  pass against the fix) — confirmed the "object shape" and "string shape"
  cases are exactly what would have zeroed out `found` under the old
  struct.

## Deferred / follow-up (not this task's scope)

- **A real wazero end-to-end harness for this plugin, run from this
  repo or `universal-till`.** This repo's own README/CLAUDE.md claim
  "verified end-to-end against the real host runtime... every branch...
  run through universal-till's actual WasmRuntime.HandleEvent" for the
  demo-mode/refund paths — no test file backing that claim exists in
  either repo as of this change. `src/convert.go`'s tests are real but
  narrow (pure logic only); the actual WASI event-handling paths
  (`approve`/`decline`/`readerCharge`/settle-then-refund) remain
  unverified by any committed test. Should become its own Backlog card
  rather than silently relying on the unbacked claim.
- Reader tip-prompt enablement: no known per-checkout SumUp API field
  exists today; revisit if SumUp's docs gain one.
- A shared tip-amount sanity ceiling across both this plugin and the core
  request-supplied `tip` field (nice-to-have above).

## Safe to merge

Yes. The blocking finding (an unverified API-shape guess turning a real
approved card payment into a false decline) is fixed with a direct
regression test; the plugin's own gates and the consuming repo's full
gate are both green.
