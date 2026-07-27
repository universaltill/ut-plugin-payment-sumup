# SumUp Card Payments — Universal Till plugin

Take card payments through **SumUp**, on the authorize-before-tender seam: the
till asks SumUp to charge the amount **before** completing the sale, and only
finishes if SumUp approves (a decline leaves the basket intact).

## Design note: SumUp has no direct equivalent to Stripe's online-charge fallback

`ut-plugin-payment-stripe` (the template this plugin follows) has two modes:
a real card-present flow via a paired Stripe Terminal reader, and a real
online/card-not-present fallback that can be **synchronously confirmed** in
one blocking API call even with no reader. SumUp's public REST API has no
equivalent second path: its Checkouts API produces a hosted payment page the
*customer* completes asynchronously, which cannot be confirmed within a
till's single blocking `payment.sumup.authorize` call without either polling
indefinitely past a reasonable checkout timeout or risking approving a sale
before the money is actually confirmed. SumUp's real product, in any case,
*is* the physical reader — that's what an in-person SumUp merchant actually
uses.

So this plugin has exactly one real production path (**with a reader
configured**) and one **demo-only** fallback (no reader configured), not two
real ones:

- **With `sumup_reader_id` set**: drives a paired SumUp Solo/Air/3G reader
  via SumUp's Cloud API — starts a checkout on the reader, then polls the
  Transactions API for a terminal result (same shape as the Stripe plugin's
  Terminal-reader polling loop).
- **With no reader configured**: deterministic demo outcomes, no network
  call — same contract as `ut-plugin-payment-demo` (amount ending **.13**
  declines, **.99** times out, anything else approves). Useful for staff
  training/E2E tests, **not a real payment path**.

## Configure (plugin settings)

- `sumup_api_key` — the merchant's own SumUp API key (created in the SumUp
  developer portal, one key per merchant — nothing hardcoded).
- `sumup_merchant_code` — the merchant's SumUp merchant code.
- `sumup_affiliate_key` — Universal Till's own integrator/affiliate key
  (**not** merchant-specific — see "What's still manual" below). Required
  for the reader flow only.
- `currency` — ISO code (default `eur` — this plugin's first real target is
  a German prospect, see `ut-docs` memory `prospect-german-shop-migration`).
- `sumup_reader_id` (register-scoped) — the paired reader this specific till
  drives. Leave unset for demo mode.

## Refunds

Refunding a SumUp sale on the till refunds the original transaction
automatically (`POST /v0.1/me/refund/{txn_id}`), looked up by the
transaction id saved when the original sale settled. In demo mode (no
reader), a refund is a no-op approve — nothing real was ever charged, so
there's nothing to refund against SumUp's API, and declining would
incorrectly block a return in a demo/test install.

## What's still manual

**Farshid needs to register Universal Till as a SumUp integrator** (SumUp
developer portal → Affiliate Keys) to get the `sumup_affiliate_key` value —
this is a per-integration credential, not something a merchant configures
themselves, so it should ship as a documented default once obtained rather
than staying an empty per-merchant setting forever. Each merchant separately
creates their own `sumup_api_key` + `sumup_merchant_code` and pairs their own
reader for `sumup_reader_id`.

## Endpoints used (confirmed against developer.sumup.com, 2026-07-27)

- `POST /v0.1/merchants/{merchant_code}/readers/{reader_id}/checkout` —
  start a reader payment. **Needs sandbox verification**: the exact JSON
  placement of the affiliate key (a top-level `"affiliate": {"key": ...}`
  object) follows SumUp's docs description but wasn't confirmed against a
  live response.
- `GET /v2.1/merchants/{merchant_code}/transactions?client_transaction_id=…`
  — poll for the reader payment's outcome (`SUCCESSFUL`/`FAILED`/
  `CANCELLED`). **Needs sandbox verification**: the list response envelope
  (bare array vs. an `items` wrapper) wasn't confirmed; the plugin handles
  both defensively.
- `POST /v0.1/me/refund/{txn_id}` — refund, confirmed request/response shape
  including the major-unit decimal `amount` field and the 204-No-Content
  success contract.

## Test mode (demo, no reader)

Deterministic outcomes, like a demo terminal — no real money, no network
call:
- amount minor units ending in **13** → declined
- amount minor units ending in **99** → timeout (declined)
- anything else → approved

Verified end-to-end against the real wazero host runtime (not just "the Go
code compiles") — every branch above, plus the settle-then-refund path,
run through `universal-till`'s actual `WasmRuntime.HandleEvent`.

## Build

```sh
bash scripts/build.sh   # -> bin/plugin.wasm (GOOS=wasip1 GOARCH=wasm)
```
