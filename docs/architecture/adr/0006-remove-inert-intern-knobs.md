# 0006. Remove inert string-interning config knobs

## Status

Accepted (milestone M3, 2026-06-15).

## Context

`WorkflowDataConfig.MaxInternStringLength` and `InternStringCapacity` were written by three
config presets but **never read**: `NewWorkflowDataWithConfig` constructs the interner with
no arguments (`NewStringInterner()`). They were lying knobs — they looked configurable but
had no effect. Honoring them would be net-new feature work: capacity threading plus a
length-gate that does not exist in the `Intern` hot path.

## Decision

**Remove both fields and the three preset assignments.** Adding real behavior (capacity +
a length gate) is a feature, not a cleanup, and belongs in a later feature milestone — not
mis-scoped into a surface-truthfulness pass.

## Consequences

- The config no longer advertises a knob that does nothing.
- **Not a behavior change**: the fields were inert, so removing them changes nothing at
  runtime. Removal breaks only code that assigned to the fields (and the config tests, which
  were updated). Migration note in the CHANGELOG.
- Configurable length-gating can return as an honest feature (e.g. M4+).

## Alternatives Considered

- **Honor them** (wire into `NewStringInterner` + add a length gate) — net-new behavior +
  benchmarking; a feature, not a wire-up.

## References

- `pkg/workflow/workflow_data_config.go`
