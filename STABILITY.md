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
