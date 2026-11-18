# Per-leg refund_amount (ut-docs#6603), SumUp v1.7.0

Design: ut-docs#6603 architect note (ADR-0196 d.4 applied as written, no ADR).

- `origPayment.RefundAmount` (`*int64`); `src/refundlegs.go` (`perLegMode`, `planLegs`, `refundLegs`); `refundRecord.Legs` in `src/refund.go`.
- Legacy path untouched when no leg carries the field or on a reversal; v1.6.0 records are read (done -> approve, sending -> `refund_outcome_unknown`).
- Tests: `src/refund_legs_test.go` (13 cases incl. invalid table, bounded record, host value cap, masked logs).
- Not verified against a live SumUp account (fakes only).
- Review: pending (orchestrator's reviewer step).
