# Code review: scoping note on why no locales/ ships here yet (ut-docs#1883)

**Branch:** `docs/1883-localization-scoping-note`
**Author:** Farshid Mirza (pipeline, `lane:cloud-24`)
**Reviewer:** independent Opus subagent (different model from the Sonnet
build pass)

## What changed

README-only. ut-docs#1883 asked for German (`de`) strings in this plugin
alongside its sibling `ut-plugin-tax-de` change (same card). Investigated
and found genuinely blocked, not skipped: this plugin's only UI-facing
string is its `payment`-type entry's `label` ("Card (SumUp)"), which core
copies verbatim into the `payment_methods.name` DB column at install/sync
time (`internal/data/plugin_repo.go`) and renders raw on the POS Pay-tab
buttons (`web/ui/pages/index.html:318,349`) — no `T` call anywhere in that
render path. Unlike `ut-plugin-tax-de`'s export-entry picker (fixed by a
small `universal-till` template change in this same card), there is no
render-time hook here for a locale-key convention to attach to. Shipping a
`locales/de.json` mapping a key this plugin's manifest doesn't use would be
inert. Documented as a "Localization (ut-docs#1883, scoping note)" section
in README.md, with a cross-cutting core follow-up tracked separately rather
than attempted piecemeal on this one repo.

## Independent review

The reviewer verified this plugin's technical claims directly against
`universal-till` source rather than taking them on faith:

- Confirmed `internal/data/plugin_repo.go` copies `pe.label` verbatim into
  `payment_methods.name` at sync time (`INSERT INTO payment_methods (id,
  name, …) SELECT pe.key, pe.label … WHERE pe.type = 'payment'`).
- Confirmed `web/ui/pages/index.html:318,349` render `.Name`/`.Name` with
  no `T` call.
- Confirmed `web/ui/pages/plugin_settings.html:14,42` render `{{ .Key }}`
  raw for every plugin's settings-field labels, corroborating this README's
  other claim.
- Conclusion: **the technical reasoning is accurate in every particular
  checked, and the conclusion (a per-plugin locale file would be inert) is
  correct.**

**One correction made after review:** the original note's closing sentence
claimed `scripts/package.sh` was "ready to adopt the moment core's render
path exists." The reviewer found this repo's `scripts/package.sh` had the
same `locales/`-omission bug as `ut-plugin-tax-de`'s (F1 in that repo's own
review — its `entries=(...)` array never included `locales/`, so a future
`locales/de.json` here would silently never reach the packaged artifact
either). Fixed pre-emptively in this same branch (`[ -d locales ] &&
entries+=(locales)`, same guard shape as the sibling fix) and the closing
sentence corrected to say so, rather than leaving a second copy of the same
latent bug for whoever adopts this convention next.

Also synced `scripts/guard-plugin-i18n.sh` from the canonical
`ut-docs/scripts/templates/guard-plugin-i18n.sh` (updated in this same card
per the tax-de review's F2/F5 findings) — no functional change for this
repo today (no `locales/` directory, no key-shaped manifest labels), but
keeps the drop-in copy current so the new manifest-label-resolution check
is active here too the moment this plugin adopts the convention.

## Verification

- `go test ./...` — unaffected (`src` package, no Go source touched),
  confirmed still green.
- No wasm/manifest change, so `scripts/build.sh`/`scripts/validate.sh`
  behavior is unchanged.
- `bash scripts/guard-plugin-i18n.sh` — `no locales/ directory — nothing
  further to check` (correct, unchanged outcome).

## Go/no-go

Reviewer: GO, with the one correction above (applied before this record was
written).
