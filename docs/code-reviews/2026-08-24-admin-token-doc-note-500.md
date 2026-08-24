# 2026-08-24 — MARKETPLACE_ADMIN_TOKEN doc note (ut-docs#500)

## Context
Follow-up to ut-docs#496 (server-side `authorizeStaff` in `ut-cloud` now
honors a distinct `MARKETPLACE_ADMIN_TOKEN`, checked exclusively ahead of
`MARKETPLACE_UPLOAD_TOKEN` for staff-only admin routes — no server-side
fallback once it's configured). The client side (`scripts/approve.sh`
preferring `MARKETPLACE_ADMIN_TOKEN`) already shipped in ut-docs#166, but
its comment carried a stale warning telling operators not to provision a
distinct secret because the server didn't support it yet. This repo has no
env-var table in its README (unlike `ut-plugin-faq`), so `scripts/approve.sh`'s
own comment is the sole doc location touched. Docs-only.

## Changes
- `scripts/approve.sh`: replaced the "CLIENT-SIDE PREP ONLY, will 401
  today" warning with a note that `MARKETPLACE_ADMIN_TOKEN` is honored
  server-side as of #496.
- No script logic changed — `bash -n` clean, diff is comment-only.

## Independent review
Fresh-context Sonnet subagent reviewed the diff across all three affected
repos (`ut-plugin-faq`, `ut-plugin-payment-sumup`,
`ut-plugin-integration-webhook`) together, and independently re-read
`ut-cloud/internal/api/claims_pages.go`'s `authorizeStaff` to verify the
claim: confirmed the server checks `MARKETPLACE_ADMIN_TOKEN` exclusively
once configured, with no fallback to the upload token. One wording nit
(the first draft said "checked ahead of" the upload token, which reads as
try-then-fallback rather than exclusive) — fixed in this version to say
"replaces ... entirely, rather than being tried first with a fallback".
Verdict: safe to merge.

## Testing
- `bash -n scripts/approve.sh` — clean.
- No functional/behavioral change; nothing else to test.
