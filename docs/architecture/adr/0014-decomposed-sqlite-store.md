# 0014. Decomposed SQLite store — per-node rows, indexed visibility, and the deep-durable structural fix

## Status

Accepted (milestone M15 "SQLite-backed WorkflowStore", 2026-07). Records the locked M15
scope decision (`DEC-M15-STORE`): add a **decomposed, row-based** `SQLiteStore` — one row
per node rather than one blob per run — as a **fully additive** third first-class store. It
is the **WIDEN-not-freeze** outcome of the 1.0-freeze intake: the SQLite store is the enabler
for durable-visibility queries (a 1.x feature) *and* the structural fix for the M14
deep-durable `O(N²)` re-serialization tail, and it forecloses nothing about the 1.0 freeze
because every part of it is additive over the frozen `WorkflowStore` base.

## Context

Two problems converged at the M15 intake:

1. **The deep-durable `O(N²)` tail.** The FB/JSON stores persist the **whole** workflow as
   one blob per checkpoint. At each of the DAG's *N* levels a checkpoint re-serializes all
   *N* nodes → `O(N²)` serialize work across a deep run. ADR-0012's group-commit amortized
   the *`fsync`* cost but left the *serialize* shape (`Strict`'s per-level fsync still
   dominated at moderate depth). The cause is structural: the durable unit is a monolithic
   blob, so nothing smaller than the whole run can be written.

2. **Durable visibility.** Answering operability questions — "which workflows have a
   `Waiting`/`Failed` node", "which are rolling back" — was only possible via the
   composition-only fallback (`ListWorkflows` → `Load`-each → decode-and-filter), which
   deserializes *every* run per query. The data was all present (the M9 per-node journal),
   but there was no **index**.

Both point at the same lever: **decompose the durable record into rows.** A row-per-node
store makes a checkpoint a per-node `UPSERT` (fixing the `O(N²)` serialize by writing only
what changed) *and* makes "which runs have a Failed node" an indexed scan instead of a full
decode.

The 1.0-freeze intake asked whether to **freeze first** (ship 1.0, add SQLite in 1.x) or
**widen first**. The decomposed store is cleanly additive over the frozen 4-method
`WorkflowStore` base — the same proven **base-interface + additive-optional-capability**
pattern already used by `Checkpointer` (M9) and `Syncer` (M14) — so widening does not delay
or complicate the freeze. It was ratified as **Option B: decomposed per-node rows** (over a
hybrid alternative), front-loading a crash-atomicity TLA arm because the row-decomposition is
the new risk.

## Decision

Add `SQLiteStore` (`NewSQLiteStore(path string, opts ...SQLiteOption)`), a decomposed
row-based `WorkflowStore` backed by **pure-Go `modernc.org/sqlite`** (no cgo — `CGO_ENABLED=0`
is preserved), with a **decomposed schema**:

- `workflows` — one row per run: the run-level scalars (`rolling_back`, `trigger_cause`,
  `updated_at`). **There is no `status` column** — a run's status is **DERIVED per-node**,
  it is not a stored run-level field.
- `nodes` — one row per node: `status` (a `NodeStatus` string) + `output`. An **output-only
  node** (a node with an output but no status entry) is persisted with a `''` sentinel
  status, *not* a phantom `Pending` — the decomposed store must reproduce the blob store's
  "no status" state faithfully.
- `data_kv` — typed data entries (a `kind` discriminator mirrors the FB store's typed vectors
  so `Load` reconstructs the same Go type; `int64` rides an INTEGER-affinity column, never a
  float scan destination — the type-affinity landmine the spike proved).
- `waits` — durable timer `fireAt` per parked node (M10).

**Fidelity contract:** `SQLiteStore.Load` reconstructs a `WorkflowData` whose `Snapshot()` is
**byte-identical** to the same data through the FB/JSON path (verified by a round-trip oracle
and a gopter substitution of the SQLite store into the existing property suite).

**Additive optional capabilities** (all type-asserted, exactly like `Checkpointer`/`Syncer`):

- **`IncrementalCheckpointer`** (`SaveDeltaCheckpoint(ChangeSet, *WorkflowData)`) — the
  **structural fix for the `O(N²)` tail.** The executor passes the per-level *changed-set*;
  the store re-reads and `UPSERT`s only those keys → `O(Δ)` compute **and** writes = a genuine
  `O(N)` forward drive. (A store without it falls back to the full `SaveCheckpoint`.)
- **`WorkflowQuery`** (`ListByNodeStatus`, `ListRollingBack`) — indexed visibility.
  **Option A honest primitives**: the interface exposes primitives over the real (derived)
  data model — "which runs have a `Waiting` node" = `ListByNodeStatus(Waiting)` — and leaves
  run-level buckets ("terminally failed", "completed") to the caller's composition, rather
  than baking a contested run-status taxonomy into the interface.
- **`Syncer` + SQLite durability modes** (`WithSQLiteDurability(SQLiteStrict() | SQLiteBatched(k))`)
  — the SQLite analogue of ADR-0012 (see ADR-0012 and the durability semantics in the
  Persistence guide).

**Single-process only.** The lease is an in-process `Locker` and the store uses a
single-writer connection (`SetMaxOpenConns(1)` + `busy_timeout`); it is **not**
multi-process-safe and is documented as such. (`modernc.org/sqlite` is the pure-Go driver;
multi-process access is out of scope for M15.)

## Consequences

- **The `O(N²)` deep-durable tail is structurally fixed** via `IncrementalCheckpointer` —
  a measured **`O(N)`** forward-drive shape (the executor passes the per-level changed-set, so
  the store does `O(Δ)` compute *and* writes rather than re-scanning all `N`/level). Even the
  *delta-free* `Checkpointer` fallback is already a large absolute win over M14's blob store
  (the committed deep benchmark: deep-4000 ≈1.86s vs M14's ≈44.8s, ~24× faster / ~1000× less
  I/O) — but that fallback path keeps the `O(N²)` compute *shape* (see the honesty note below);
  only the `IncrementalCheckpointer` fast path is true `O(N)`.
- **Durable visibility ships** — `WorkflowQuery` answers the operability questions from two
  covering indexes (`idx_nodes_status`, `idx_workflows_rolling_back`) with no per-run decode.
- **Fully additive; the freeze is not foreclosed.** The frozen `WorkflowStore` base and every
  existing signature are unchanged; the new capabilities are optional type-asserted
  interfaces. A consumer that never constructs a `SQLiteStore` is unaffected, and the moat
  (durable format byte-unchanged, gopter, TLA, determinism tax) is unregressed.
- **Crash-atomicity is machine-checked.** A checkpoint is one SQLite transaction — a crash
  leaves either the prior committed frontier or the new one, never a partial level. The
  set-of-rows decomposition is modeled in TLA+ (`specs/DecomposedCheckpoint.tla`,
  `INV_NoPartialLevel`) and bite-proven.
- **Honesty floor (do not overstate):** the durability contract is **exactly-once state
  PERSISTENCE, at-least-once side EFFECTS** (idempotency-keyed) — *never* unqualified
  exactly-once. Run status is **DERIVED per-node** — there is no `workflows.status` column.
  Under `SQLiteBatched(K)`, a power loss can lose **≤`K` levels** (bounded loss, re-run
  idempotently), which is a real weaker-durability bound, not "durable".

## Alternatives Considered

- **Freeze first, add SQLite in 1.x.** Rejected: SQLite is additive over the frozen base, so
  widening costs the freeze nothing, and the `O(N²)` fix + visibility are wanted *now*.
- **A hybrid (blob + sidecar index).** Rejected in favor of full per-node-row decomposition
  (Option B) — the hybrid keeps the monolithic-blob `O(N²)` serialize shape it was meant to
  fix.
- **A `workflows.status` column** (a stored run-level status). Rejected: run status is a
  *derivation* over per-node statuses; a stored column would be a second source of truth that
  can disagree with the journal. `WorkflowQuery` derives it (Option A primitives) instead.
- **cgo SQLite (`mattn/go-sqlite3`).** Rejected: `modernc.org/sqlite` keeps `CGO_ENABLED=0`,
  preserving the zero-cgo, cross-compile-clean build.

## References

- [Persistence guide → SQLite store](../../guides/persistence.md#sqlite-store-details-decomposed-row-based)
  and [→ Durability modes](../../guides/persistence.md#durability-modes-strict-vs-batched).
- [ADR-0012](0012-group-commit-durability-modes.md) (the FB group-commit precedent) and
  [ADR-0009](0009-durable-continuations-waiting-status.md).
- `specs/DecomposedCheckpoint.tla` and the M15 CHANGELOG.
