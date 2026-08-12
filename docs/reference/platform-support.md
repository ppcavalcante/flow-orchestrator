# Platform support & store capability matrix

This page states, precisely, what runs where. It exists because "the stores are
interchangeable" and "multi-process works" are both **too broad**: multi-process is a
SQLite-only capability, and the file stores' cross-process signal mailbox is safe only on
Unix. Use this matrix rather than assuming parity.

## Toolchain & architecture

- **Go**: module floor **1.25.0** (the `go` directive in `go.mod`). Build and test on 1.25.x.
- **Reference architecture**: `arm64` — the ratified measurement arch for the performance
  ceilings (`perf_ceiling_test.go`). CI additionally runs `amd64` (linux), which is the
  release-gating architecture. Numeric fidelity is exercised on both (an int64-via-float64
  JSON bug once appeared only on amd64), so cross-arch behavior is a tested property, not an
  assumption.

## Store capability matrix

All four stores implement `WorkflowStore` (Save/Load/ListWorkflows/Delete) and, optionally,
the interfaces below. A capability a store does not implement is simply absent — the engine
type-asserts and degrades (e.g. a non-`SignalStore` store offers no wait-for-signal).

| Capability (interface) | InMemory | JSON file | FlatBuffers file | SQLite |
|---|:---:|:---:|:---:|:---:|
| Checkpoint (`Checkpointer`) | ✅¹ | ✅ | ✅ | ✅ |
| Signal mailbox (`SignalStore`) | ✅ (in-proc) | ✅ | ✅ | ✅ |
| Multi-process execution / competing consumers (`ClaimStore`) | — | — | — | ✅ |
| Indexed visibility queries (`WorkflowQuery`) | — | — | — | ✅ |

¹ InMemory implements `Checkpointer` but its state lives in a map — it is **not
crash-durable** (a process exit loses it). The JSON, FlatBuffers, and SQLite stores persist
checkpoints to disk and survive a crash.

- **`InMemory`** is process-local: its signal mailbox is a map, never shared across
  processes. Use it for tests and single-process runs.
- **File stores** (`JSONFileStore`, `FlatBuffersStore`) are durable and single-process for
  *execution* — they carry no lease table, so they do not do competing-consumers. Their
  signal mailbox lives on the filesystem and *can* be written by more than one process; see
  the platform note below for when that is safe.
- **`SQLite`** is the multi-process store: the durable `leases` table + fencing tokens make
  competing consumers and cross-process signaling safe on every supported platform.

> Values are **not** guaranteed byte-identical across stores. `int64` magnitude is preserved
> on all durable stores (FlatBuffers `value_long`, JSON via `UseNumber`), but complex/nested
> values can reload with different concrete types across backends. Do not treat the stores as
> drop-in equivalent for value fidelity — see the [persistence guide](../guides/persistence.md).

## Multi-process by platform

Multi-process safety splits into two channels — the execution lease and the signal mailbox —
and they have different platform stories.

| | Unix (linux, darwin, …) | Non-Unix (Windows) |
|---|---|---|
| **SQLite** competing consumers (execution) | ✅ safe | ✅ safe |
| **SQLite** cross-process signal delivery | ✅ safe | ✅ safe |
| **File-store** cross-process signal delivery | ✅ safe (`flock`) | ⚠️ **NOT process-safe** |

**The Windows file-mailbox limitation (be explicit):** the file stores' mailbox directory
lock is implemented with `flock(2)` and is present only on Unix builds
(`signal_store_lock_unix.go`, `//go:build unix`). On non-Unix builds the lock is a **no-op**
(`signal_store_lock_other.go`, `//go:build !unix`), so concurrent cross-process delivery can
exceed the mailbox cap, race an ack against a re-delivery, or resurrect an entry into a
just-deleted workflow. Windows is **cross-compiled** in CI but the suite is not run there, so
this is a documented limitation, not a tested guarantee.

**Guidance:** on non-Unix, or whenever more than one process delivers signals to the same
workflow, use the **SQLite** store. Single-process use of the file stores (including their
signal mailbox) is fine on every platform.
