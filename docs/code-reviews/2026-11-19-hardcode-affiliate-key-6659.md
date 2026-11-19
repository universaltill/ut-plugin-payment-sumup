# 2026-11-19 — Hard-coded SumUp affiliate key (v1.9.1, ut-docs#6659)


**What shipped.** Owner decision 2026-11-19: the SumUp affiliate key is not a secret (it only attributes transactions to the affiliate account), so it is a constant (`universalTillAffiliateKey`) in `src/affiliate.go` instead of an -ldflags value from the `SUMUP_AFFILIATE_KEY` GitHub secret. `scripts/build.sh` is a plain wasip1 build; `release.yml` lost the secret env and the "require key" guard step; README and CLAUDE.md updated; manifest 1.9.1. `builtinAffiliateKey` stays as a var initialised from the constant, a test seam; a blank key still declines `no_affiliate_key` without calling SumUp. Supersedes slice 2 of the v1.9.0 record (no secret to set).

**Review.** Independent reviewer (different model from the author). Ran vet, tests (-count=1), wasm build, YAML parse. No leftover injection or secret references; nothing logs the key; tests use the constant only for presence/charset and fake values elsewhere.

**Findings.** 1 low: stale test-file header comment (fixed/accepted per triage). 1 low: v1.9.0 record's slice-2 note superseded (annotated). Info: key now in public git history by owner decision.

**Verdict.** Approve. Deferred: none.

Pre-flight: walked; no UI, no file writes, no SQL; key never in record/PR text.
