# ut-plugin-payment-sumup — notes

A WASM (`GOOS=wasip1 GOARCH=wasm`) **payment** plugin, built from
`ut-plugin-payment-stripe`'s template (its `CLAUDE.md` says as much: "Template
for other API-based gateways ... swap the endpoint, auth, and request/response
shaping"). Hooks `payment.sumup.authorize` (blocking, pre-tender): exit 0 =
approved, non-zero = declined.

Unlike Stripe, SumUp has no synchronous online-charge API a till can confirm
within one blocking call — see README's "Design note" for why. So there are
two branches, not two *real* payment paths: reader-driven (real, needs
`sumup_reader_id`) and demo (no reader, deterministic, same contract as
`ut-plugin-payment-demo`).

Reader flow: `POST /v0.1/merchants/{code}/readers/{id}/checkout` with
`total_amount{currency,minor_unit,value}` (minor units — no conversion needed,
the till's event payload amount is already minor units) + a top-level
`affiliate{key}` object, then polls `GET
/v2.1/merchants/{code}/transactions?client_transaction_id=…` once a second for
a terminal status inside the till's 30 s gate (see the 2026-11-16 note below). Refund: `POST /v0.1/me/refund/{txn_id}` with
`{"amount": <major-unit decimal>}` — note the unit mismatch with the reader
endpoint (minor vs. major units) is real, not a bug; SumUp's own API is
inconsistent about this across endpoints, confirmed against
developer.sumup.com. Refund success is a 204 (checked via the host's
`status` field on the `http_request` response envelope — see
`internal/plugins/wasm_hostfns.go` in `universal-till`, NOT an empty-body
heuristic).

**Verified 2026-07-27** against the real host runtime (`universal-till`'s
`WasmRuntime.HandleEvent`, wazero, not just a Go build): demo-mode
approve/decline/timeout, settle-then-refund storage round-trip, and — the one
real bug this verification pass caught before shipping — a `.refund` event
with no reader configured used to fall into the demo authorize-decision
switch (checking `amount % 100` as if it were a fresh charge) instead of
being treated as "nothing real to refund, no-op approve"; fixed by handling
`.refund` before the demo/reader branching, not inside it.

**NOT verified against a live SumUp sandbox** (no credentials available at
write time) — the reader-checkout affiliate-key JSON placement and the
transactions-list response envelope are both best-effort against SumUp's
public docs, flagged in code comments and the README. Verify both against a
real merchant account before a live reader ships.

**2026-08-01 — tip auto-sync (v1.1.0, ut-docs#43):** the reader flow now
reports a tip back on the approve response's `tip_amount` field (integer
minor units) — `universal-till`'s `completeTender` reads this and applies it
to the payment even though the tender request itself carried no tip (the
customer only picks a tip on the reader, after the till already sent the
charge amount). Depended on `universal-till`'s core `payments.tip_amount`
field (already shipped 2026-07-28) and a new response-carrying path on the
plugin event bus (`EventBus.PublishAuthorize`, added alongside this). The
transaction's `tip_amount` field/units are a **NEW unverified assumption**,
same caveat class as the two above — see `pollTransaction`'s doc comment.
Pure conversion/parsing logic (`minorToMajor`/`majorToMinor`,
`parseTipAmountMinor`/`parseTransactionPoll`) now lives in `src/convert.go`
(no `wasip1` build tag) specifically so it has real `go test` coverage —
as does the no-reader test-mode decision, `demoAuthorize` in `src/demo.go`
(ut-docs#3057; `demo_test.go` also keeps manifest.json's description honest
about it). CI runs `go test ./...` since then. Everything else in `src/`
still has none (see below); this repo's
"verified against the real host runtime" claims above describe `universal-
till`-side verification this repo itself has no test file for, which is
worth closing with an actual harness here, not just relying on the host
repo's suite — see the follow-up note in this change's review record.

**Independent review caught a real bug before this shipped**: the first
draft decoded `tip_amount` as a fixed `*float64` on the SAME struct used
to detect the transaction's own id/status — since `tip_amount`'s shape is
an unverified guess, a wrong guess (this file's own `total_amount` field a
few lines up is an *object*, not a bare decimal — an equally plausible
shape for `tip_amount`) would have failed the whole parse, silently
discarding a real `SUCCESSFUL` transaction and declining a card the
customer had already been charged on. Fixed by decoupling tip parsing
from outcome detection (`json.RawMessage` + a separate, always-degrading
`parseTipAmountMinor`) — full record:
`docs/code-reviews/2026-08-01-reader-tip-authorize-response.md`.

**2026-10-25 — selectable currency (v1.2.0, ut-docs#5029):** `currency` is a
`choice` setting (default `auto`); `src/currency.go` (untagged, tested)
resolves saved choice > sale currency > `shop.context.v2` shop currency,
and declines `currency_mismatch` / `currency_unknown` / `unsupported_*` rather
than charge a guessed or wrong currency (the authorize event carries no
currency today). 0-decimal CLP/COP/HUF excluded on purpose (README
"Currencies"). Needs `view:shop` + `views_used`, `min_pos_version` 0.31.9.

Build: `scripts/build.sh`.

**2026-11-16 — reader timeout (v1.3.0, ut-docs#6304):** the reader flow lives
in `src/reader.go` (untagged, `go test`-covered by `reader_test.go`, whose fake
follows SumUp's docs: transactions lookup 404 when none, terminate always 202,
async, no transaction left behind). The till kills an authorize call at 30 s
(`paymentGateTimeout`), so the flow polls 18 s, then `POST
/v0.1/merchants/{code}/readers/{id}/terminate` and re-checks until 24 s (no call
starts after that): SUCCESSFUL -> approve (with tip); FAILED/CANCELLED ->
`reader_timeout`; anything else -> `outcome_unknown` (a 202 proves nothing).
Checkouts are kept in ONE bounded storage key (`attempts`, last 8; host cap is
1024 keys, no delete) under the till's request id, which the till re-sends on a
re-tap of Pay for the same basket (ut-docs#1762). A retry settles the old
checkout first: paid -> approve; failed -> fresh; no transaction -> fresh only
once 90 s have passed since it started and `GET .../readers/{id}/status` says
ONLINE + IDLE, else decline again without starting anything. A lost
checkout-start reply also terminates the reader. Never move the poll past the
host gate, treat a terminate 202 as "not charged", or drop a payment that
succeeded late.

**Refund links (ut-docs#6438, v1.4.0):** never one storage key per sale or
event (host cap 1,024 keys, no guest delete). `src/links.go` keeps sale ->
transaction in 256 fixed `blob:own` buckets (32 KiB each, FIFO);
`src/recent.go` keeps the last 8 approvals in `last_txn` (`recent`) and links a
sale only on a unique match (amount/window) or exactly by `authorize_request_id`,
never by the bare last approval. Core's `transaction_ref` / `original_payments`
are used first and need no plugin storage. Refund order and the decline
`unknown_original_payment` live in `src/refund.go`. All of it is untagged and
tested against fakes that enforce the real host caps (`refundlink_test.go`).
