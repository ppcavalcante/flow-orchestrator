# 0002. Integer fidelity contract (int64-widen-additive)

## Status

Accepted (milestone M2, 2026-06-15).

## Context

The FlatBuffers store clamped integers to `int32` on save, silently corrupting values
outside the `int32` range. The JSON store, by contrast, already round-tripped `int64`
faithfully — so the two backends disagreed for the same value. The `int32` width was a bare
FlatBuffers default with no deliberate rationale (the same schema already used `long` for
`timestamp`). The fix had to restore fidelity without breaking older `.fb` files.

## Decision

**int64-widen-additive.** Add an additive `value_long:long` field to the `KeyValueInt`
FlatBuffers table (keeping the legacy `value:int`). Save writes the full `int64` to
`value_long` with no clamp; Load reads `value_long`, falling back to the legacy `value` for
files written before the change. Add a new public `WorkflowData.GetInt64(key) (int64, bool)`
accessor; keep `GetInt` as `(int, bool)` (platform width) with its 32-bit limit documented.

## Consequences

- The FlatBuffers store now stores/restores the full `int64` range and matches the JSON store.
- **Data-compatible**: older `.fb` files load via the legacy fallback; the schema change is additive.
- The only public-API change is the additive `GetInt64`; no existing signature changed.
- Caveat (documented): a foreign FB writer that set the legacy `value` while leaving
  `value_long` at 0 would have the legacy value masked. All in-tree producers write
  `value_long`, so this only affects out-of-tree writers.

## Alternatives Considered

- **Error-on-overflow** — makes the FB store *stricter* than JSON for the same value,
  re-splitting backend behavior rather than unifying it.
- **Replace `value:int` with `long`** (non-additive) — would break older `.fb` files; rejected.

## References

- `pkg/workflow/schema/workflow_data.fbs`, `pkg/workflow/workflow_store.go`, `pkg/workflow/workflow_data.go`
- Supersedes the M1 fidelity caveat in `docs/guides/persistence.md`.
