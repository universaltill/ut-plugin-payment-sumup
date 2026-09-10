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

## Localization (ut-docs#1883, scoping note)

This plugin ships **no `locales/*.json`** as of this note, despite
ut-docs#1883 asking for `de` strings here alongside `ut-plugin-tax-de`'s.
Investigated and found genuinely blocked, not skipped: the plugin's only
UI-facing string is its `payment` entry's `label` ("Card (SumUp)"), and core
copies that verbatim into the `payment_methods.name` DB column at
install/sync time (`SyncPluginPaymentMethods`) — the Pay-tab tender buttons
(`web/ui/pages/index.html`) then render `.Name` raw, with no call to `T`
anywhere in that path. Unlike the `export`-entry picker in
`ut-plugin-tax-de` (ut-docs#1883's other half, fixed with a small template
change), there is no render-time `T` call here to hook a locale-key
convention into — the string is baked into a DB row at sync time, once, for
every payment plugin in the ecosystem, not just this one. Shipping a
`locales/de.json` mapping a key this plugin's manifest doesn't use would be
inert: nothing would ever look it up. Fixing this needs a cross-cutting
core change (resolve the payment method's display name through the
translator at render time, or re-sync it whenever the locale changes) that
affects every payment plugin, not a per-plugin locale file — tracked as its
own follow-up rather than attempted piecemeal here. `scripts/guard-plugin-i18n.sh`
is still wired into this repo's CI (ut-docs#1882) and passes cleanly with no
`locales/` directory. `scripts/package.sh` was also updated in this same
change (ut-docs#1883 review, F1 — found in `ut-plugin-tax-de`'s sibling
change: its `package.sh` shipped a real German-pilot regression by omitting
`locales/` from the release artifact) to include `locales/` in the packaged
archive whenever the directory exists, so this repo is ready to adopt the
convention the moment core's render path exists, with no packaging gap
waiting to bite on day one.

## Tips

With a reader configured, **and tipping already enabled in the merchant's
own SumUp device/profile settings**, a tip the customer picks on the
reader itself syncs back to the till automatically — no manual re-entry by
staff. The plugin reads the transaction's `tip_amount` back on the same
poll used for the approve/decline outcome and reports it on the approve
response; the till's `completeTender` applies it to the payment even
though the original tender request carried no tip (there's no other way
for the till to learn it before the customer interacts with the reader).
Demo mode (no reader) never reports a tip — there's no real reader
interaction to read one from.

**Not done**: this plugin's reader-checkout request sends no tipping
configuration of its own — it relies entirely on tipping already being on
in the merchant's SumUp profile. If it isn't, the reader simply never
prompts for a tip and this feature silently does nothing (not an error,
just a no-op). SumUp's published Cloud API docs had no documented
per-checkout field to enable tipping at write time; revisit if one turns
up.

**Needs sandbox verification**: `tip_amount`'s presence/type on the
Transactions API response wasn't confirmed against a live merchant account
— assumed to be a decimal major-unit number (matching the Refund endpoint's
`amount` field), converted to minor units. This is decoded independently
from the transaction's own id/status (`parseTipAmountMinor`,
`src/convert.go`) specifically so a wrong guess about `tip_amount`'s shape
can only ever cost a missed tip — it cannot turn a card the customer
already successfully tapped into a false decline, and a wrong *type*
guess (e.g. if the real field turns out to be integer minor units already,
not a major-unit decimal) would silently apply a tip 100x too large rather
than failing loudly. Verify both the presence and the units against a real
merchant account before relying on this in production; until then, treat
any live tip amount on a receipt as unconfirmed.

## Endpoints used (confirmed against developer.sumup.com, 2026-07-27)

- `POST /v0.1/merchants/{merchant_code}/readers/{reader_id}/checkout` —
  start a reader payment. **Needs sandbox verification**: the exact JSON
  placement of the affiliate key (a top-level `"affiliate": {"key": ...}`
  object) follows SumUp's docs description but wasn't confirmed against a
  live response.
- `GET /v2.1/merchants/{merchant_code}/transactions?client_transaction_id=…`
  — poll for the reader payment's outcome (`SUCCESSFUL`/`FAILED`/
  `CANCELLED`) and tip (`tip_amount`, see "Tips" above). **Needs sandbox
  verification**: the list response envelope (bare array vs. an `items`
  wrapper) wasn't confirmed; the plugin handles both defensively.
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
