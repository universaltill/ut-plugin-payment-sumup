# Code review: ut-docs#6561/#6440 SumUp affiliate app_id + foreign id lookup (2026-11-17)
Independent review by an Opus subagent (code written by Sonnet); tester report /tmp/tabletfix/tester-sumup-stripe.md (mutation-tested, 7/9 caught, the 2 gaps closed or equivalent).

Shipped: affiliate {app_id (setting sumup_app_id, default com.universaltill.pos), key, foreign_transaction_id}; empty key still refuses; lost start looked up by foreign_transaction_id.
Findings: HIGH attempt only saved after a failed POST (host kill during hung reply -> double charge on re-tap) — FIXED: saved before the POST, cleared on 4xx. MED 5xx on start not treated as lost — FIXED. MED late calls on lost path past 24 s — FIXED (guards). MED foreign lookup answering non-404 keeps retries outcome_unknown — ACCEPTED, sandbox item in CLAUDE.md. LOW app_id must match the registered id, foreign id length unknown — documented.
Verdict: approve after fixes.

Pre-flight: review record updated; README + CLAUDE.md updated; manifest version bumped to 1.5.0; en.json keys n/a (no new labelled setting); help/ui n/a (plugin settings only); live sandbox/reader verification deferred -> ut-docs#6561 / #6440 (blocked:env items).
Re-verified myself: removing the pre-save of the attempt makes TestLostStartReplyRemembersForeignIDForRetry and TestAttemptRecordedBeforeCheckoutRequest fail; restored, pass.
