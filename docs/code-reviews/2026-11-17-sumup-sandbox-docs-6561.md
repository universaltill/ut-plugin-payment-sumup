# Code review: ut-docs#6561 SumUp sandbox-verified docs (2026-11-17)
Independent review by an Opus subagent (docs written by Sonnet). Docs and comments only; `git diff` of src/reader.go touches `//` lines only; gofmt, vet, `go test ./src` pass.

Shipped: "NEEDS SANDBOX VERIFICATION" replaced by the facts verified 2026-11-17 in the SumUp sandbox (Virtual Solo, no physical reader) in README, CLAUDE.md and the reader.go comment; Verification status section; manifest 1.5.0 -> 1.5.1 (the version-bump guard treats README.md/src/ as shipped; merge auto-publishes a plugin release).
Findings: LOW "within about a minute" and "app_id accepted" overclaimed — FIXED. NIT refund wording "is read" -> "must be read" — FIXED. NIT heading note on 1.5.1 — FIXED. Tips and the client_transaction_id poll envelope stay marked unverified.
Verdict: approve.

Pre-flight: review record updated; README + CLAUDE.md updated; manifest bumped to 1.5.1; en.json n/a; help/ui n/a; physical-reader test deferred -> ut-docs#6561 / #6440 (optional, owner 2026-11-17).
