# Contributing

The canonical contributor guide now lives at the repository root:

- **[CONTRIBUTING.md](../../CONTRIBUTING.md)** — dev setup (the Go toolchain version is the one
  in [`go.mod`](../../go.mod); `make generate-fb`, `go test ./... -race`, `make lint` pinned to
  golangci-lint v2.12.2), the fork-PR-against-`main` workflow, conventional commits, and the
  public-API stability contract.

  > This page deliberately does **not** restate the Go version. It named `Go 1.24` while `go.mod`
  > required `1.25.0` — a value transcribed from the root guide, which carries the same error.
  > A pointer cannot drift; a copy does. **Root `CONTRIBUTING.md` is still wrong and is outside
  > `docs/`** (tracked as `F-DOCVERIFY-05`).
- **[CODE_OF_CONDUCT.md](../../CODE_OF_CONDUCT.md)** — Contributor Covenant 2.1.

This file used to duplicate that guidance and had drifted (it named tooling and a branch model the
project no longer uses, plus links to files that were moved or removed). Rather than maintain two
copies, it now points at the root `CONTRIBUTING.md` as the single source of truth.

For the deeper testing rationale see [Test Coverage Strategy](./test_coverage_strategy.md).
