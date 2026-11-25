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
developer.sumup.com. Re-checked 2026-11-19 (ut-docs#6686, `src/refund_units_test.go`):
the legacy schema types `amount` as a float with no unit stated (major units
inferred from its sibling fields; only a full refund was sandbox-tested), and
SumUp's spec dropped this path in late April 2026 for
`/v1.0/merchants/{code}/payments/{id}/refunds` (integer minor units, other
path params) — migration: ut-docs#6695. Refund success is a 204 (checked via the host's
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

**Affiliate app_id + lost start (ut-docs#6561/#6440, v1.5.0; sandbox-verified, docs in 1.5.1):** the checkout
body's `affiliate` is `{app_id, key, foreign_transaction_id}` (setting
`sumup_app_id`, default `com.universaltill.pos`; empty key still refuses
`no_affiliate_key`). The Affiliate Keys docs page shows a `{affiliate, checkout}`
wrapper, the create-checkout reference (authoritative) is flat with
`total_amount` — we follow the reference. `foreign_transaction_id` =
`ut-<request id>-<start unix>`; a lost start reply stores it as the attempt Ref
(prefix `ut-` -> lookup by `foreign_transaction_id`, UUID client ids never
start with it). Verified in the sandbox 2026-11-17 (Virtual Solo only; see README).
The attempt (Ref = foreign id) is saved BEFORE the checkout POST, so a host
kill during a hung reply still lets the re-tap settle it; a 5xx or id-less 2xx
is treated as a lost reply (terminate + lookup, nothing after 24 s); a 4xx
clears it. Sandbox 2026-11-17: the lookup by
`foreign_transaction_id` answers 404 while pending, then 200; a matching `app_id`/key pair was accepted (a mismatch was not tested); foreign id has no documented length limit.

**Built-in affiliate key (v1.9.0; hard-coded constant since v1.9.1, ut-docs#6659):** the `sumup_affiliate_key` setting is gone (manifest + locales). `src/affiliate.go` holds the constant `universalTillAffiliateKey` (owner decision 2026-11-19: not a secret, it only attributes transactions to the affiliate account; a new key = new constant + new release). No ldflags/env/GitHub-secret injection any more (`scripts/build.sh` and release.yml are plain). `builtinAffiliateKey` (var, initialised from the constant) is only a test seam; a blank key still declines `no_affiliate_key`. `sumup_app_id` stays a setting. **Do not print the key in logs, PR text or review records.** A value saved in a till's old setting stays in the host DB, unread.

**Wire-level contract suite (ut-docs#6565, v1.5.2):** `src/contract_test.go` runs the reader flow and refund over real HTTP (paths, bodies, status codes; not headers/auth, the adapter is the test's) against a fake server modelled on the 2026-11-17 sandbox exchanges (lookup by `client_transaction_id` and the reply id field are assumed, not recorded) (flat `total_amount` + `affiliate{app_id,key,foreign_transaction_id}`, lookup 404 then 200, FAILED for decline and terminate, READER_BUSY 422, refund 204 as a separate entry). The suite caught that the checkout reply was only read as `data.client_transaction_id` while the sandbox note records it flat; `run()` now accepts both. Keep the fake in step with ut-docs#6561 when SumUp behaviour changes; no key is in the repo.

**Settings labels (ut-docs#6570, v1.5.3):** each setting's `label`/`help` are `payment_sumup.<name>.label`/`.help` keys in `locales/en.json`, English only (ADR-0160 §2; de/tr are not hand-written). `src/settings_labels_test.go` pins that every setting and option resolves.

**Refund safety + explicit test mode (v1.6.0, ut-docs#6598/#6599/#6606):** `src/refund.go` keeps a bounded `refunds` record (last 8, by event id): repeat of a done refund answered from it; lost-reply retry goes to the same leg only, full refund -> recorded, partial -> `refund_outcome_unknown`; a 5xx counts as unknown. Demo only with setting `sumup_test_mode`=on (`authorizeMode` in `demo.go`); refunds route by reference (`demo-` never calls the API). The stdout answer never carries `recent` (`updateLastTxn` copies the map); `declineAnswer` prints `decline_code` (incl. `outcome_unknown`) for core to read on exit 2.

**Per-leg refunds (v1.7.0, ut-docs#6603):** `src/refundlegs.go`. If any `original_payments[]` leg of a non-reversal refund has `refund_amount` (`*int64`, absent != 0), refund exactly that per leg in payload order; never pick a leg, never fall back to "next leg". `planLegs` validates everything before any call (`invalid_refund_amount`, `unknown_original_payment`, `too_many_original_payments` above `maxRefundLegs` = 16). Per-leg state lives in `refundRecord.Legs` (still one entry per event id, 8 max): `sending` + full leg -> resend, `sending` + partial -> `refund_outcome_unknown`; a failure after a done leg is `refund_partial`. A v1.6.0 record has no `legs`: `done` -> approve its ref, otherwise unknown. Amounts to SumUp stay major-unit decimals (`minorToMajor`). A reversal never reads `refund_amount`. Logs: `maskRef`/`scrubRefs` only.

**Reader per call + state (ut-docs#6566, v1.8.0):** `src/readers.go` (untagged, `readers_test.go`). `chooseReader(payload reader_id, sumup_reader_id)`: payload wins, missing/blank falls back to the setting, a present id not matching `^[A-Za-z0-9_-]{1,64}$` declines `invalid_reader` (also in test mode) with no API call and NO fallback (the id goes into URL paths; strict, no trimming of a non-empty id). `authorizeMode` is unchanged and gets the effective id. A refund never reads or validates `reader_id` (also on `original_payments[]`): it goes by `transaction_ref`. Hook `payment.sumup.readers` (the manifest declaration is the capability core checks) answers `{"readers":[{reader_id,state,reason?}]}` via `readerStatus` (shared with `readerIdle`): ONLINE+IDLE ready, ONLINE other busy, OFFLINE offline, 404 offline `reader_unreachable`, else unknown; max 16 refs.

**Refund storage fails closed (v1.9.2, ut-docs#6650):** `saveRefundRecord` returns the put error. The "sending" save before a refund call (`refundSale`, and each leg in `refundLegs`) must succeed or SumUp is NOT called: declines `refund_storage_error` (`refund_partial` if an earlier leg of the event is already done; storage keeps the last saved leg states, so a retry continues from there). Otherwise a lost reply plus a retry would find no record and refund twice. "done"/clearing saves may ignore the error. Whole-amount mode clears the record after each definite "too large" before trying the next candidate (and again, best effort, when that candidate's "sending" save fails): a stale "sending" record on an earlier candidate would make a retry resend there, read "nothing left" as its own refund and approve with no money moved (review F1). Events with no event id keep skipping the record. Tests: `src/refund_storage_test.go`.

**Reader start refusals (v1.10.0, ut-docs#6961):** `src/readerdecline.go` (untagged, `readerdecline_test.go`). A definite 4xx on the reader checkout becomes a FIXED decline slug via `readerStartDecline(status, body)`, never SumUp text: type (nested `{errors:{type}}` or flat `{type}`; a problem+json URL type keeps its last path segment, `-` -> `_`; case-insensitive) `READER_BUSY` -> `reader_busy`; `*OFFLINE*`/`*NOT_CONNECTED*` -> `reader_offline`; `*AFFILIATE*`/`API_KEY`/`UNAUTHORIZED`/`FORBIDDEN`/exactly `MERCHANT_MISMATCH` or HTTP 401/403 -> `auth_failed`; `NOT_FOUND`/`READER_NOT_FOUND` or 404 -> `no_reader`; 409 -> `reader_busy`; else `reader_start_failed`. Known type beats status. A 404 carries no type, so a wrong merchant code also shows as `no_reader`. The till maps these codes to cashier text (no plugin locale strings); `outcome_unknown` is never used for a refusal. One log line: HTTP status + sanitised type only, never `detail`, body, key or ids.
**Only 4xx is a refusal (v1.10.1, ut-docs#6987):** any other non-2xx on the checkout start (1xx/3xx/5xx/out of range) takes the lost-reply path (terminate, lookup by foreign id, attempt kept); only 400-499 clears the attempt and uses `readerStartDecline`.
