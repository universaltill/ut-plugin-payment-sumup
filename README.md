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

- **With `sumup_reader_id` set**: drives a paired SumUp Solo reader
  via SumUp's Cloud API — starts a checkout on the reader, then polls the
  Transactions API for a terminal result (same shape as the Stripe plugin's
  Terminal-reader polling loop). The till waits 30 seconds for a card
  payment, so the customer has about 18 seconds to tap; then the plugin asks
  SumUp to terminate the reader checkout. SumUp does not confirm a terminate,
  so unless SumUp reports the checkout failed, the till shows the card as
  declined (`outcome_unknown`) and the plugin keeps the checkout on record.
  Tap **Pay** again on the **same basket**: if the earlier payment went
  through, it is approved without a second charge; if it failed, a new
  checkout starts. If SumUp still shows nothing, the plugin declines again
  without starting a new checkout until about 90 seconds after the first one
  started and the reader is idle, then starts fresh. If you change the basket
  instead, check the reader or the SumUp dashboard before charging again
  (ut-docs#6304).
- **With Test mode switched on** (`sumup_test_mode`, default off; v1.6.0): deterministic demo outcomes, no network
  call — same contract as `ut-plugin-payment-demo` (amount ending **.13**
  declines, **.99** times out, anything else approves). Useful for staff
  training/E2E tests, **not a real payment path**.

## Configure (plugin settings)

- **API key** (`sumup_api_key`) — the merchant's own SumUp API key (created in the SumUp
  developer portal, one key per merchant — nothing hardcoded).
- **Merchant code** (`sumup_merchant_code`) — the merchant's SumUp merchant code.
- **No affiliate key setting (since v1.9.0, ut-docs#6659).** Universal Till's
  own integrator/affiliate key (**not** merchant-specific) is built into the
  plugin source (`src/affiliate.go`, a hard-coded constant since v1.9.1: it is
  not a secret, it only attributes transactions to the affiliate account).
- **Affiliate app ID** (`sumup_app_id`) — the application identifier (App ID / Bundle ID) the
  built-in affiliate key was created for. Default `com.universaltill.pos`. Not a
  secret; SumUp wants it sent together with the key. Change it only if told
  to by Universal Till.
- **Card payment currency** (`currency`) — a picker (symbol + code + name; labels in EN/DE/TR). Default
  **Automatic**: the shop's own currency, read from the till through the core
  read view `shop.context.v2` (`view:shop` permission; the manifest asks for
  till 0.31.9+, the first release known to carry both the picker and the
  view). An explicit pick wins, but it must equal the sale's currency (see
  "Currencies"). Installs that saved the old free-text default `eur` keep
  that value: in a non-EUR shop they are now declined with
  `currency_mismatch` instead of silently charged in euros (switch the
  setting to Automatic) - but only while the view is readable; if the
  operator denies `view:shop` the old explicit `eur` is used as before.
- **Card reader ID** (`sumup_reader_id`) (register-scoped) — the paired reader this specific till
  drives. Required for card payments: with no reader id and Test mode off
  (`sumup_test_mode`) a payment is declined `no_reader`; demo behaviour needs
  Test mode switched on.

## Currencies

- Offered: AUD, BRL, CAD, CHF, CZK, DKK, EUR, GBP, MXN, NOK, PEN, PLN, RON,
  SEK, USD — the currencies of SumUp's merchant markets per
  developer.sumup.com "Countries and Currencies" (checked 2026-10-25). SumUp
  publishes no separate list for the Reader/Tap to Pay API; its Reader
  Checkout docs accept any ISO 4217 code with an explicit `minor_unit`.
- **Zero-decimal currencies (CLP, COP, HUF) are deliberately excluded**, and a
  shop whose till currency is one of them (or any currency not 2-decimal) is
  declined (`unsupported_currency` / `unsupported_decimals`). SumUp documents
  `minor_unit` 0 for those three; supporting them needs a currency-aware
  divisor in `minorToMajor` (refunds use major units), the reader request and
  the tip parser, plus agreement with the till's own decimals for them, none
  of which can be verified without a live SumUp sandbox. Money-critical, so
  not guessed; a later card can add them with tests.
- **Mismatch guard:** the charge currency must equal the sale's own currency.
  The till's `payment.sumup.authorize` event carries **no currency today**
  (only method, amount, reference; the refund event does carry one), so on a
  charge the sale currency is the shop's currency from `shop.context.v2`;
  when the host starts sending `currency` it takes precedence automatically.
  A mismatch is declined with `currency_mismatch` - amounts are minor units
  of the sale currency, so charging another would bill the wrong money.
  Applies to reader charges and reader-sale refunds; test mode charges
  nothing.
- **Fail closed:** with Automatic selected and neither the event nor the
  view telling the plugin the currency (`view:shop` not granted, view
  unavailable), a reader charge is declined with `currency_unknown` rather
  than guessed as euros. Test mode (no reader) only displays `eur`.
- Refunds: SumUp refunds in the original transaction's currency by amount;
  a sale charged in euros by the old default can only be refunded after the
  setting is switched to Automatic (the check needs the setting to match the
  sale's currency).
- AUD, CAD, MXN and PEN are offered on SumUp's merchant-market evidence only:
  SumUp's online Checkouts enum lacks them (it lists BRL CHF CLP COP CZK DKK
  EUR GBP HRK HUF NOK PLN RON SEK USD) and the Reader API publishes no list,
  so they fall under this README's "needs live sandbox verification" caveat.
- Decline codes: `currency_mismatch`, `currency_unknown`,
  `unsupported_currency`, `unsupported_decimals`.

## Refunds

Refunding a SumUp sale on the till refunds the original transaction
automatically (`POST /v0.1/me/refund/{txn_id}`), looked up by the
transaction id of the original sale (see "Finding the original transaction"
below). A refund whose reference is
not a `demo-` one always goes to SumUp, whatever the reader setting; a `demo-`
reference (a test-mode sale) is approved with nothing to send back. SumUp's
refund call has no idempotency key, so a refund sent for an event id is
recorded (last 8): a repeat is answered from it, and a retry after a lost
reply goes to the same transaction only and, if it was a partial refund,
declines `refund_outcome_unknown` (check the SumUp dashboard) instead of
risking paying twice (v1.6.0, ut-docs#6598).

**Split-tender refunds (v1.7.0, ut-docs#6603).** When the original sale used
two or more SumUp payments, the till sends each one's own `refund_amount`
(minor units). The plugin then refunds exactly that on each transaction, in
order, and never chooses a transaction itself. Nothing is sent unless the whole
allocation is valid: a missing or non-positive amount, an amount above the
leg's amount plus tip, a total that differs from the refund, or a missing
reference declines `invalid_refund_amount` / `unknown_original_payment`; more
than 16 legs declines `too_many_original_payments`. If a later leg fails after
an earlier one was refunded, the answer is `refund_partial` (money has already
moved; nothing is rolled back). Repeating the same refund finishes only the
legs not yet done, from the same `refunds` record (still one entry per event
id, last 8, now with a per-leg state). A leg whose reply was lost is resent
only if it is a full refund of that leg; a partial one declines
`refund_outcome_unknown`. Without `refund_amount` the single-transaction
behaviour above is unchanged, and records written by v1.6.0 are still read.
Downgrading to v1.6.0 after a split refund is not supported. Until the till
shows these codes, it shows its generic "declined" text for them. Tested
against fakes only, not a live SumUp account.

### Finding the original transaction (v1.4.0, ut-docs#6438)

The plugin finds the transaction in this order: the reference the till itself
stored with the sale (`transaction_ref`, `original_payments`, newer till
versions), then the plugin's own link file, then the per-sale keys written by
v1.3.0 and older (still read, never written again). Until the till stores the
reference itself, the plugin links each sale to its transaction in 256 fixed
files in its own blob store (`blob:own`, at most 8 MiB, oldest first out). A
sale that could not be linked safely (for example two card sales of the same
amount settling at the same moment) is **not** linked; refunding it declines
with `unknown_original_payment` and must be done in the SumUp dashboard. The
plugin never guesses a transaction. Before v1.4.0 a till stopped linking new
sales after about 1,000 card sales (the host allows a plugin 1,024 storage
keys); update the plugin by hand on each till, plugins do not auto-update.

## Reader per call and reader state (v1.8.0, ut-docs#6566)

A till with a reader registry names the reader for each card sale (`reader_id`
in the authorize payload). The plugin uses it instead of `sumup_reader_id`;
with no `reader_id` (an older till, or a single-reader till) it uses the
setting exactly as before, so v1.x stays backward compatible, and with no id
anywhere the decisions are unchanged (`no_reader`, or demo in test mode). The
id must be 1 to 64 letters, digits, `_` or `-` (ids seen are `rdr_...`; the
prefix is not enforced because it is unverified against the live API). A
payload id that does not match is declined with `invalid_reader` before any
SumUp call, and the setting is **not** used instead (that would charge a
different physical reader). The id goes into a SumUp URL path, which is why it
is checked.

The plugin also answers the read-only hook `payment.sumup.readers`
(`{"op":"state","readers":["rdr_..."]}`, at most 16 per call) from
`GET /v0.1/merchants/{code}/readers/{id}/status`: ONLINE and IDLE is `ready`,
ONLINE in any other state is `busy`, OFFLINE is `offline`, a 404 is `offline`
with `reader_unreachable`, and anything unclear (or a rejected key) is
`unknown`. Nothing is changed on the reader. The status shape is the one the
plugin already relies on for its retry check; it is not verified against a
physical reader.

Refunds still go by the original transaction (`transaction_ref`). A `reader_id`
on `original_payments[]` is accepted and ignored: a removed or offline reader
never blocks a refund. Amounts to the reader checkout stay integer minor
units; reader ids are logged masked at most.

## What's still manual

The SumUp affiliate key is generated in the SumUp developer portal
(Affiliate Keys) for the application identifier `com.universaltill.pos` and
is a hard-coded constant in `src/affiliate.go` (owner decision 2026-11-19,
ut-docs#6659; not a secret). A new key means a new constant and a new release.
Each merchant separately creates their own
`sumup_api_key` + `sumup_merchant_code` and pairs their own reader for
`sumup_reader_id`.

## Localization (ut-docs#1883, scoping note)

Since v1.5.3 every setting also has a `label` and `help` key
(`payment_sumup.<name>.label` / `.help`) in `locales/en.json`, so the settings
page shows names instead of raw keys (hover shows the key). These new keys are
English only: other languages come later through Weblate (ADR-0160 §2), and
German/Turkish tills show English for them meanwhile (the till falls back to
`en.json`). `scripts/guard-plugin-i18n.sh` only notes such missing keys.

As of v1.2.0 the plugin ships `locales/{en,de,tr}.json` for the currency
setting's label, help and option labels (the settings page resolves them).
The payment entry's own label ("Card (SumUp)") is still not translated, for
the reason below.

History (ut-docs#1883 note, before v1.2.0): this plugin shipped **no
`locales/*.json`**. Investigated and found genuinely blocked, not skipped: the plugin's only
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
  start a reader payment. The body carries `total_amount` and an
  `affiliate` object `{app_id, key, foreign_transaction_id}`; `foreign_transaction_id`
  is `ut-<till request id>-<start time>`. **Verified 2026-11-17 in the SumUp
  sandbox** (Virtual Solo only, no physical reader tested): this body is
  accepted with HTTP 201. It follows SumUp's create-checkout reference
  (developer.sumup.com/api/readers/create-checkout, `app_id` and `key` both
  required once `affiliate` is sent), which is authoritative: SumUp's
  affiliate-keys doc page still shows an outdated `{checkout:{amount}}`
  wrapper. The sandbox affiliate key is separate from the live one. A second
  checkout while one is pending answers 422 `READER_BUSY`.
  If the reply to this call is lost, the reader is terminated and the sale is
  looked up with `GET /v2.1/merchants/{code}/transactions?foreign_transaction_id=…`;
  a payment found that way is approved, otherwise a re-tap of Pay settles it.
  Verified in the sandbox: that lookup answers 404 `NOT_FOUND` while the
  checkout is pending, then 200 with the status. A declined card (11.00 GBP)
  and a terminate (HTTP 202) both end `FAILED` with no reason, so they cannot
  be told apart.
- `GET /v2.1/merchants/{merchant_code}/transactions?client_transaction_id=…`
  — poll for the reader payment's outcome (`SUCCESSFUL`/`FAILED`/
  `CANCELLED`) and tip (`tip_amount`, see "Tips" above). **Needs sandbox
  verification**: the list response envelope (bare array vs. an `items`
  wrapper) wasn't confirmed; the plugin handles both defensively.
- `POST /v0.1/me/refund/{txn_id}` — refund, confirmed request/response shape
  including the major-unit decimal `amount` field and the 204-No-Content
  success contract. Sandbox 2026-11-17: the refund answers 204 and shows as a
  separate history entry; the original transaction keeps `SUCCESSFUL`, so
  refund state must be read from the history, not from the original's status.
  Units re-checked 2026-11-19 (ut-docs#6686): SumUp's OpenAPI-generated Go SDK
  (`sumup/sumup-go`, synced from SumUp's spec) typed this endpoint's `amount`
  as a float with no unit stated, like its sibling fields that are major
  units (`amount`, `tip_amount`, `vat_amount`; refund `max_amount` "in major
  units"), so we send 12.50 as `12.5`. In late April 2026 the spec dropped the
  path for `POST /v1.0/merchants/{code}/payments/{id}/refunds`, whose `amount`
  is an **integer in minor units** (`500` = EUR 5.00). The legacy path still
  answered 204 to a full refund in the sandbox on 2026-11-17; no partial
  refund has been sent there, so the partial-amount unit is inferred from the
  schema. `src/refund_units_test.go` pins 12.50, a partial leg and a reversal.
  Moving to v1.0 (needs the merchant code and payment id, integer minor units)
  is ut-docs#6695.

## Test mode (setting `sumup_test_mode`, default off)

Without test mode, no reader id declines `no_reader` (API key set) or
`no_api_key`; it is never a silent demo approve (ut-docs#6599).

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

The affiliate key is the constant in `src/affiliate.go`; the build has no key
injection. For sandbox checks edit the constant locally and never commit it;
the sandbox key is separate from the live one.

## Verification status (2026-11-17)

Verified in the SumUp **sandbox only**; no physical reader has been tested.
Sandbox readers are Virtual Solo (virtual-solo.sumup.com), which is not a
substitute for a physical Solo. It came online only in a clean browser window
(a normal session got a 403 on its WebSocket) and only for a short while, so
pair and run all tests right after getting the pairing code. Still unverified: tips, the
`client_transaction_id` poll's response envelope, and the live affiliate key
on a live reader.
