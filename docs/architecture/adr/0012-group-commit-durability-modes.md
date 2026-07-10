# 0012. Group-commit durability modes — `Strict` vs `Batched(K)`

## Status

Accepted (milestone v0.13-M14, 2026-07). Records the locked M14 REM-01 durable-perf
decision (`DEC-M14-GROUPCOMMIT`): reduce deep-durable checkpoint time with an **additive,
opt-in** group-commit flush mode over **full snapshots** — the default (`Strict`) is the
pre-M14 durable contract, bit-identical on disk. **Delta/incremental checkpointing was
evaluated and dropped** (see Context); group commit is the shipped mechanism.

## Context

Through M12 the durable path fsynced the **whole** workflow snapshot at every completed
level barrier. That is cheap for shallow/wide DAGs but material at depth: a 1000-level
durable run measured ~10.6s. M13's 1.0-readiness validation flagged this deep-durable cost
as a before-1.0 gap (REM-01).

The obvious fix — **delta/incremental checkpointing** (persist only the nodes changed since
the last flush) — was prototyped first and **measured**. The measurement overturned the
premise: the per-level cost is **`fsync`, not bytes** (~10ms fixed per level). A delta still
writes one `fsync` per level, so it does **not** reduce the dominating cost. Worse, `fsync`
is per-file-descriptor, so a delta *sidecar* file cannot be group-committed with the base.
Delta was reverted (the computation kept in git history for a possible post-1.0 WAL).

The real lever is **how often we `fsync`, not how much we write.** M14 is the last
breaking-change window before the 1.0 three-axis freeze, so the mechanism had to be
**additive** (no `.fbs`/format change — none shipped) and had to keep the default contract
byte-identical.

## Decision

Add an **opt-in group-commit flush cadence** to `FlatBuffersStore`, selected at construction
with a variadic option (`NewFlatBuffersStore(dir, opts...)` — old single-arg calls unchanged):

- **`WithDurabilityMode(Strict())`** — the default (also the behavior when the option is
  omitted). One `fsync` per completed level. **Bit-identical to the pre-M14 durable path.**
- **`WithDurabilityMode(Batched(K))`** — write + `fsync` a **full snapshot** only every `K`th
  `SaveCheckpoint`. The `K-1` intervening levels are **not written to disk at all** (state
  stays live-in-memory, retained in the store's `pending` buffer) — so the only on-disk
  writes are complete, fsync'd snapshots, making a torn read **structurally impossible**
  (chosen over double-buffering: the ≤`K`-level loss window re-runs idempotently anyway).

Supporting surface (all additive):
- A **`Syncer`** capability (`Sync(workflowID)`) — **not** a `Checkpointer` signature change.
- The **suspend park** and **run completion** force an immediate `Sync()`/`Save` even under
  `Batched`, so a parked or finished run is always durable on disk (the floor holds in every
  mode).
- `Batched(0)` / `k<1` is guarded to `≡ Strict`. Only `FlatBuffersStore` batches;
  `JSONFileStore` stays full-snapshot / `Strict`-only (not the deep-durable perf path).

## Consequences

- **Deep-durable cost drops ≈43-48× (hardware-varying):** the 1000-level run goes from
  ~10.6s (`Strict`) to ~0.25s (`Batched(64)`). A residual `O(N²)`-serialize tail only bites
  past ~3k sequential levels (pathological for a width-parallel engine); a delta-WAL remains
  a possible post-1.0 option if deeper scale is ever needed.
- **`Batched(K)` weakens power-loss durability by design** — a crash can lose up to `K`
  levels of completed-but-not-yet-fsynced progress. Those levels **re-run** on resume, which
  is safe under the existing **at-least-once idempotency contract** (M9): side effects must
  already be idempotent, so re-running ≤`K` levels is harmless. This is a **security-relevant
  durability contract** — a caller opts into a weaker loss bound consciously; the default
  never does. (See the M14 security review.)
- **No moat regression:** the default path is byte-identical (durable format + gopter + TLA +
  ph55 format goldens hold by construction); **no determinism tax** (283/277 unchanged on the
  non-durable path). The batched-crash semantics are machine-checked in TLA+
  (`specs/GroupCommit.tla`): correct green, and bite-proven to redden under a wrong cadence
  (`WrongCadence → INV_Loss`) and a missing park sync (`NoParkSync → INV_ParkDurable`).
- **No format change** — delta was dropped; the `.fbs`/generated tree is unchanged this
  milestone, so the format axis stays frozen-candidate-clean.

See [the Persistence guide → Durability modes](../../guides/persistence.md#durability-modes-strict-vs-batched),
[`specs/README.md`](../../../specs/README.md), and the M14 CHANGELOG.
