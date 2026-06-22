# 0007. Error taxonomy — store sentinels + `%w`

## Status

Accepted (milestone M3, 2026-06-15).

> Note (M4, 2026-06-16): the `ErrCorruptData` *category* and exposure rules below are unchanged,
> but its FlatBuffers production path was strengthened — see [ADR-0008](0008-layered-bounds-guard-trust-reratify.md).
> The "malformed FlatBuffers (recover path)" cell now reads "guard-rejected before decode, with the
> recover() path as residual backstop." The taxonomy itself is not re-opened.

## Context

The store/persistence and validation boundaries returned ad-hoc string errors
(~111 bare `fmt.Errorf`), so callers could not branch on failure category without
string-matching. The action-execution domain already had sentinels
(`ErrInputNotFound`/`ErrInvalidInput`/`ErrExecutionFailed`), but the persistence domain had
none. A corrupt-data error also risked leaking file paths or raw decode internals to callers.

## Decision

Add **exported store sentinels** matched with `errors.Is`, wrapped with `%w` for detail, per
an enumerated category→site contract:

| Sentinel | Category | Where |
|---|---|---|
| `ErrNotFound` | not-found | `store.Load` when the underlying read is missing-file (`errors.Is(readErr, fs.ErrNotExist)`); a permission error is `ErrIO`, not not-found |
| `ErrValidation` | validation | empty/unsafe workflow ID, invalid request rejected before I/O |
| `ErrCorruptData` | corrupt-data | malformed FlatBuffers (recover path) and JSON parse failure |
| `ErrIO` | transient-I/O | non-not-found read failures + write/delete failures |

**Exposure rules:** identify by validated `workflowID`, never by file path / base dir;
`ErrCorruptData`'s public message is generic ("corrupt workflow data"), with raw panic/parse
detail behind `%w` only. The two sentinel families (store vs action) are **kept distinct and
not aliased** — `ErrNotFound` (no workflow on disk) is a different concept from
`ErrInputNotFound` (a data key absent in an action).

## Consequences

- Callers branch on category with `errors.Is` instead of string-matching.
- Security-neutral-to-improving: corrupt-data and path detail no longer leak at the boundary.
- Mostly additive (new sentinels); the action-domain sentinels are unchanged and intentionally separate.

## Alternatives Considered

- **Typed error structs** — heavier than callers need for category branching; sentinels +
  `%w` cover the requirement.
- **Alias the two domains onto one set** — conflates distinct concepts (store not-found vs
  action input not-found).

## References

- `pkg/workflow/errors.go`, `pkg/workflow/workflow_store.go`
- See STABILITY.md "Error contract — two domains".
