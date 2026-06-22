# 0008. Layered bounds guard for FlatBuffers Load; trust-contract re-ratification

## Status

Accepted (milestone v0.4-M4, 2026-06-16). Supersedes the interim crash-stop posture of
[ADR-0001](0001-flatbuffers-load-trust-contract.md); closes HARD-01. Records two locked
decisions: `DEC-M4-mechanism` (the guard) and `DEC-M4-trust-reratify` (the contract wording).

> Note (M5, 2026-06-18, SEC-01): the size-cap layer described below as an `os.Stat` gate
> before `os.ReadFile` was **made atomic** — `Load` now reads through an
> `io.LimitReader(f, cap+1)` and checks the resulting length, eliminating the stat-then-read
> TOCTOU (a file could grow between the `os.Stat` and the `os.ReadFile`). The guard's posture,
> bounds, and residual are unchanged; only the enforcement is race-free. The "File-size cap"
> wording in the Decision section is retained as-written for history.

## Context

`FlatBuffersStore.Load` is the single FlatBuffers root-deserialize in the repository
(`pkg/workflow/workflow_store.go`; the `LoadSnapshot` / `LoadFromFlatBuffer` paths decode JSON,
a separate bounds-safe trust domain). After M1 it was protected only by a `recover()` that
converted an accessor panic into an error. That is crash-stop, not rejection: a malformed,
truncated, or absurd-count file still entered the decode and relied on a panic to be caught.
HARD-01 set out to reject malformed input *before* the traversal, to the availability ceiling
the M1 trust contract already promised — without over-claiming "verified."

Go's FlatBuffers runtime (`flatbuffers v25.2.10`) ships **no `flatbuffers.Verifier`** — there is
no `verifier.go`, no `Verify` symbols, and the generator emits none (engineer and qa confirmed
independently via SMTC). So a verifier-based design was not available; the mechanism had to be
hand-rolled.

## Decision

**`DEC-M4-mechanism` — ship a hand-rolled layered bounds guard, not a Verifier.** At the single
decode site in `FlatBuffersStore.Load`, ahead of `fb.GetRootAsWorkflowState`:

1. **File-size cap** — `os.Stat` gate before `os.ReadFile`, so an oversize file is rejected
   before its bytes are buffered (`defaultMaxFileSize`, 64 MiB, internal const).
2. **Root-offset + minimum-length sanity pre-check** — reject a buffer shorter than a root
   offset, or a root `UOffsetT` that points past the buffer, as typed `ErrCorruptData`.
3. **Per-element count caps** — before each load loop, reject any `*Length()` over
   `defaultMaxElements` (1 Mi, internal const), stopping a tiny header that claims billions of
   elements before the allocation.
4. **The M1 `recover()` stays** as the residual backstop for deep-offset cases the cheap
   pre-walk cannot cover.

Configuration is **internal default consts, zero new public surface** — no new option landed
this milestone. (A variadic functional option was held in reserve for a consumer that needs to
tune the caps; none did.)

**`DEC-M4-trust-reratify` — re-ratify the trust contract; do not re-open it.** The trust
boundary and posture are unchanged; only enforcement is stronger. The contract reads:

> Caller-controlled persistence; **malformed input is rejected before structural traversal**
> (size cap + root/offset sanity pre-check + element caps at the read boundary), not merely
> recovered-from-panic; traversal-guarded; ceiling = availability; **NOT adversarial-proof.**

## Consequences

- HARD-01 is **closed to the availability ceiling**: no panic, no unbounded allocation, typed
  `ErrCorruptData` + `data == nil` rejection of malformed-detectable shapes. The single read path
  is dominated by the guard (SMTC `get_callers(GetRootAsWorkflowState)` = exactly one caller,
  `Load`; the three guard layers precede the lone decode — Decision-grade, LSP warm).
- A strengthened oracle (`errors.Is(ErrCorruptData)` AND `data == nil` AND no-panic), a forall
  malformed corpus, a `FuzzFlatBuffersStoreLoad` harness with regression-seed replay, and a
  data-compat positive pin (golden fixture + fresh round-trip pass the guard and Load clean) all
  ship with the guard.

## The residual that STAYS (honest scope — not "verified")

The guard checks **structure, not meaning.** Even guarded, a *well-formed* file can:

- **(a)** allocate up to the size cap — bounded, not free;
- **(b)** carry semantically-hostile-but-structurally-valid content the guard cannot detect;
- **(c)** drive `WorkflowData` into any schema-permitted state — no confidentiality/integrity
  guarantee.

Empirically demonstrated: a near-complete truncation of the 144-byte golden fixture (e.g.
`[:142]`) still decodes its in-range scalar fields and **loads clean as in-bounds data**, because
FlatBuffers front-loads the root and vtable. That is not a crash and the guard never promised to
reject it. The promise is **"won't panic / won't unbounded-alloc," NOT "won't load
malicious-but-valid data."** Callers persisting input that can cross a trust boundary must still
validate and sandbox it before loading.

## Alternatives Considered

- **Bump FlatBuffers and regenerate with a verifier flag** — the Go generator emits no `Verify`
  helpers; unproven for Go, not pursued.
- **Keep `recover()`-only** — that was the M1 state; too weak to claim hardening. The pre-check
  makes rejection deterministic and typed.
- **Claim "fully verified / hardened against malicious input"** — a layered bounds guard is not a
  structural verifier; over-claiming would break the honesty floor.

## References

- `pkg/workflow/workflow_store.go` (`FlatBuffersStore.Load` — size cap, sanity pre-check, element
  caps, recover backstop)
- `pkg/workflow/workflow_store_fuzz_test.go` (`FuzzFlatBuffersStoreLoad`)
- [ADR-0001](0001-flatbuffers-load-trust-contract.md) (the M1 interim crash-stop this supersedes)
