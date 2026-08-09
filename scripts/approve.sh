#!/usr/bin/env bash
# Approves (review-assign + approve, which Ed25519-signs) the release recorded
# in dist/publish-response.json. Intended for the DEV marketplace where the
# pipeline is trusted end-to-end; production keeps a human review.
#
# Environment:
#   MARKETPLACE_BASE_URL, MARKETPLACE_ADMIN_TOKEN (admin gate, preferred once
#   ut-cloud supports it), MARKETPLACE_UPLOAD_TOKEN (vendor-upload
#   credential; today the ONLY value ut-cloud's admin endpoints actually
#   accept — see ut-docs#496), REVIEWER (opt)
set -euo pipefail
cd "$(dirname "$0")/.."

: "${MARKETPLACE_BASE_URL:?MARKETPLACE_BASE_URL is required}"
REVIEWER=${REVIEWER:-release-pipeline}
RELEASE_ID=$(python3 -c "import json;print(json.load(open('dist/publish-response.json'))['data']['release_id'])")
BASE="${MARKETPLACE_BASE_URL%/}/ui/api/admin/releases/${RELEASE_ID}"

# Admin/vendor token conflation (ut-docs#166): MARKETPLACE_UPLOAD_TOKEN is a
# vendor-upload credential, not an admin credential, and prefers a distinct
# MARKETPLACE_ADMIN_TOKEN when one is set. IMPORTANT — this is CLIENT-SIDE
# PREP ONLY: ut-cloud's admin endpoints (authorizeStaff) do not yet accept
# any credential other than the upload token, so setting
# MARKETPLACE_ADMIN_TOKEN to a genuinely different value will 401 today.
# Do NOT provision a distinct secret until ut-docs#496 (the ut-cloud-side
# fix) ships — until then, leave MARKETPLACE_ADMIN_TOKEN unset and this
# falls back to the upload token with no behavior change.
ADMIN_TOKEN="${MARKETPLACE_ADMIN_TOKEN:-}"
if [ -z "$ADMIN_TOKEN" ] && [ -n "${MARKETPLACE_UPLOAD_TOKEN:-}" ]; then
  ADMIN_TOKEN="${MARKETPLACE_UPLOAD_TOKEN}"
fi
AUTH=()
[ -n "$ADMIN_TOKEN" ] && AUTH=(--header "Authorization: Bearer ${ADMIN_TOKEN}")

echo "==> Assigning review for ${RELEASE_ID}"
curl --silent --show-error --fail "${AUTH[@]}" -X POST "${BASE}/assign-review" \
  -d "reviewer_id=${REVIEWER}&priority=P1" && echo ""

echo "==> Approving ${RELEASE_ID}"
curl --silent --show-error --fail "${AUTH[@]}" -X POST "${BASE}/review-decision" \
  -d "decision=approved&reviewed_by=${REVIEWER}&comments=auto-approved by release pipeline (dev marketplace)" && echo ""
