# 0001. FlatBuffers Load trust contract

## Status

Accepted (milestone M1, 2026-06-13). Hardening follow-up (full verifier + size cap) deferred to a later milestone (tracked as HARD-01).

**Superseded in part by [ADR-0008](0008-layered-bounds-guard-trust-reratify.md) (milestone M4,
2026-06-16).** The `recover()`-only crash-stop described below was the M1 interim state. M4
landed the layered bounds guard (size cap + root/offset sanity pre-check + element caps) that
rejects malformed input *before* structural traversal — HARD-01 is closed to the availability
ceiling. The posture below (caller-controlled, ceiling = availability, NOT adversarial-proof) is
**unchanged and re-ratified**; only the strength of enforcement improved. This record is retained
as-written for history.

## Context

`FlatBuffersStore.Load` decodes `.fb` files from disk. A malformed or truncated file could
panic the decode path (a crash / DoS edge), and there is no full `flatbuffers.Verifier`
pass or size cap on input. M1 needed to close the crash edge without taking on the full
verifier scope in that milestone.

## Decision

Wrap the decode in `recover()` so a malformed file returns a "corrupt workflow file" error
rather than panicking. Accept and **explicitly disclose** that this is crash-stop only: a
full `flatbuffers.Verifier` pass, element/size bounds, and fuzzing are NOT in scope for M1
and remain a documented deferred item. The store's input is trusted up to "won't crash the
process"; callers persisting untrusted `.fb` files should know the stronger validation is
not yet present.

## Consequences

- A malformed `.fb` no longer panics; `Load` returns an error. The crash/DoS edge is closed.
- The trust boundary is documented, not silently assumed — consumers know the limit.
- A future milestone (HARD-01) can add the full verifier + size cap; until then, treat
  `.fb` input as semi-trusted.

## Alternatives Considered

- **Full `flatbuffers.Verifier` + size/element bounds + fuzz in M1** — correct end state,
  but larger than M1's scope; deferred rather than rushed.
- **Leave the panic** — unacceptable: a crash on malformed input is a DoS edge.

## References

- `pkg/workflow/workflow_store.go` (Load recover path)
- M2/M3 carry-forward: HARD-01 (full verifier + size cap + fuzz)
