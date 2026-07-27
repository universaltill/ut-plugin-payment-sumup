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
up to 60s for a terminal status. Refund: `POST /v0.1/me/refund/{txn_id}` with
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

Build: `scripts/build.sh`.
