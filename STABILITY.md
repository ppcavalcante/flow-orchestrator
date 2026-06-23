# Stability & Compatibility Policy

This document states what Flow Orchestrator considers public, how it versions, and what
guarantees a consumer can rely on. It is the contract behind every CHANGELOG entry.

## Current status: pre-1.0 alpha

Flow Orchestrator is **`0.x` alpha**. Until **`v1.0.0`**:

- The public API **MAY change between `0.x` minor releases.** We are deliberately getting
  the surface right *before* committing to long-term stability.
- **Breaks are deliberate, documented, and batched** — never silent. Every breaking change
  is listed in [`CHANGELOG.md`](CHANGELOG.md) with a migration note, and grouped into a
  minor release rather than dribbled out. (The `v0.3.0` "API Truth & Surface Cleanup"
  release exercised exactly this discipline: package moves, unexports, a deleted executor,
  and removed inert config — all inventoried with migration notes.)
- Do not pin a production system to a `0.x` minor expecting source compatibility across the
  next minor. Pin an exact version and read the CHANGELOG before upgrading.

After `v1.0.0`, this project follows [Semantic Versioning](https://semver.org/): breaking
API changes will require a major-version bump.

## What is "public"

The public API is the **exported surface of the `github.com/ppcavalcante/flow-orchestrator/pkg/workflow`
package** and its intended-public subpackages:

- `pkg/workflow` — the builder, `DAG`/`Workflow` execution, `WorkflowData`, the
  `WorkflowStore` interface and its implementations, `Action`/middleware, `Node`/`NodeStatus`,
  the `*WorkflowDataConfig` constructors, `ExecutionConfig`, the error sentinels, and
  `Version`/`VersionInfo`.
- `pkg/workflow/metrics` — the metrics collector and its configuration.
- `pkg/workflow/arena` — the arena allocator perf knob.

**`internal/**` is NOT public API.** The Go toolchain forbids importing it from outside this
module, and we make no compatibility promise about it. As of `v0.3.0` this includes the
generated FlatBuffers code (`internal/workflow/fb/...`) and the misc helpers
(`internal/workflow/utils`) — these
were previously under `pkg/` by accident and never belonged to the contract.

A symbol being *exported* inside an intended-public package does not by itself make it part
of the supported surface if it is plainly internal infrastructure; the API reference and
this document are authoritative over what we commit to.

## Data compatibility is a STRONGER guarantee than API compatibility

The on-disk persistence formats — the FlatBuffers `.fb` format and the JSON snapshot
format — are kept **backward-compatible across versions.** A file written by an older
version loads in a newer one. (The `v0.2.0` integer-fidelity change demonstrates this: the
schema gained an additive `value_long` field, and golden fixtures written by the prior
format still load.)

The practical consequence: **an API break does NOT imply a data break.** You can adopt a
new minor that renames or removes Go symbols without re-serializing your stored workflow
state. If we ever need to break the on-disk format, that will be called out far more loudly
than an API break — it is the stronger promise.

## Trust & Safety — the persistence contract

This is the one authoritative statement of what the persistence layer does and does **not**
defend against. It is deliberately honest about its ceiling: the guarantees are about
*availability* (the library will not crash or balloon memory on bad input), **not** about
*adversarial integrity* (a well-formed-but-forged file still loads as valid data).

### The trust boundary

**The persistence directory is in the caller's trust boundary.** Both the `JSONFileStore`
and the `FlatBuffersStore` read and write files under the `baseDir` you supply, named by the
workflow ID you supply. The library **does not authenticate, sign, or structurally verify**
stored data — it has no notion of who wrote a file. Anyone who can write to `baseDir` can
influence what a `Load` returns, within the robustness bounds below. Treat that directory the
way you would treat any input channel: if an untrusted party can write to it, validate and
sandbox the contents before loading.

There is **no network, process-exec, template, or query surface** anywhere in the
persistence path — loading state cannot trigger an outbound request, run a command, or inject
into a downstream interpreter. The only attack surface a persisted file presents is what it
decodes *into* your own workflow state.

### Robustness guarantees that ARE made

These hold for **both** stores, symmetrically (the JSON paths were brought to parity with the
FlatBuffers path — see the `v0.7.x` change closing M5-SEC-01):

- **`Load` never panics on malformed input.** A corrupt, truncated, or version-skewed file is
  rejected as `ErrCorruptData` with `data == nil` — it does not crash the host process.
  - For `FlatBuffersStore`, whose accessors index into the file's own offsets with no bounds
    checking, this is a layered bounds guard *ahead of* the decode (size cap, root-offset and
    minimum-length sanity check, per-element count caps), with the M1 `recover()` left only as
    a residual backstop for deep-offset cases the cheap pre-walk cannot reach.
  - For the JSON paths (`JSONFileStore.Load`, `WorkflowData.LoadFromJSON`/`LoadFromFlatBuffer`),
    the `encoding/json` decoder returns an error rather than panicking by construction; the
    added guards are the size and element-count caps below.
- **Oversized input is rejected, not loaded.** Every load path reads through an
  `io.LimitReader(cap+1)` and rejects anything over `defaultMaxFileSize` (64 MiB) as
  `ErrCorruptData`. The cap is enforced **atomically with the read** — there is no separate
  `os.Stat`, so there is no stat-then-read TOCTOU: the reader simply never consumes more than
  `cap+1` bytes, so the file's on-disk size cannot change the outcome between a check and the
  read (there is no check to race). A decoded JSON section (`data`/`nodeStatus`/`outputs`)
  over `defaultMaxElements` (~1M entries) is likewise rejected before the maps are populated,
  so a small-on-disk-but-huge-decoded document cannot drive an unbounded allocation.
- **`workflowID` is path-traversal-guarded.** Every store entry point (Save/Load/Delete on
  both stores) rejects an ID that is empty, contains a path separator, is non-local
  (`..`, absolute, volume name), or does not survive a `filepath.Base` round-trip. An ID like
  `../../etc/passwd` is refused with an error, never joined onto `baseDir`.
- **int64 fidelity is exact.** A save/load round-trip preserves the full `int64` range with no
  silent float64 precision loss, on every platform (both the JSON `UseNumber` decode and the
  FlatBuffers `value_long` field). See *Data compatibility* above.

### The ceiling — what is NOT promised

The guarantees are **availability, not adversarial-proofing.** Concretely:

- A **well-formed-but-forged file still loads.** The bounds guards check *structure and size*,
  not *meaning or provenance*. Go's FlatBuffers runtime ships no `flatbuffers.Verifier`, so the
  FB guard is hand-rolled and not a full structural verifier; a structurally-valid file can
  carry semantically-hostile content, and a near-complete truncation can still decode its
  in-range scalar fields as in-bounds data. The JSON path will load any schema-permitted state
  a valid (sub-cap) document encodes.
- Loading is **not free even when bounded** — a valid file may allocate up to the size/element
  caps.
- The library does **not** sign, encrypt, or integrity-check persisted state, and does not
  defend against a determined attacker who controls `baseDir`.

If persistence input can cross a trust boundary, validate and sandbox it before loading. See
[`docs/guides/persistence.md`](docs/guides/persistence.md) for the worked detail and ADR-0008
for the FlatBuffers-hardening decision and its residual.

## Error contract — two domains, intentionally not aliased

Flow Orchestrator exposes two distinct families of sentinel errors. They are matched with
`errors.Is` and are **deliberately separate** — they describe different domains, and we do
not alias one onto the other.

### Store / persistence domain (`pkg/workflow/errors.go`)

Returned by `WorkflowStore` implementations and the validation guards that feed them.
Detail is wrapped with `%w`; the category stays stable.

| Sentinel | When | Branch with |
|---|---|---|
| `ErrNotFound` | the requested workflow does not exist (no file/entry) | `errors.Is(err, workflow.ErrNotFound)` |
| `ErrValidation` | invalid input rejected before any I/O (empty/unsafe ID, nil data) | `errors.Is(err, workflow.ErrValidation)` |
| `ErrCorruptData` | persisted data could not be decoded (malformed/truncated/version-skewed) | `errors.Is(err, workflow.ErrCorruptData)` |
| `ErrIO` | transient/environmental I/O failure (permissions, full disk, unavailable dir) | `errors.Is(err, workflow.ErrIO)` |

`ErrCorruptData`'s public message is kept generic so it does not leak file paths or raw
decode internals; the underlying detail is available via `errors.Unwrap`/`errors.As`.
A permission failure on a read is `ErrIO`, **not** `ErrNotFound`.

### Action-execution domain (`pkg/workflow/action.go`)

Describes an `Action`'s runtime behavior — a different concept from store I/O.

| Sentinel | When |
|---|---|
| `ErrInputNotFound` | a required data input was absent within an action |
| `ErrInvalidInput` | an input value was invalid |
| `ErrExecutionFailed` | an action's execution failed |

`ErrNotFound` (a workflow is not on disk) is **not** the same as `ErrInputNotFound` (a data
key is absent inside a running action). Use the sentinel from the domain you are handling.

## Where the contract lives

- [`CHANGELOG.md`](CHANGELOG.md) — every release's additions and breaking changes + migration notes.
- [`docs/reference/api-reference.md`](docs/reference/api-reference.md) — the exported surface.
- [`docs/architecture/adr/`](docs/architecture/adr/) — the decisions behind the contract.
