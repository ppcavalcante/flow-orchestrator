# Contributing

The canonical contributor guide now lives at the repository root:

- **[CONTRIBUTING.md](../../CONTRIBUTING.md)** — dev setup (Go 1.24, `make generate-fb`,
  `go test ./... -race`, `make lint` pinned to golangci-lint v2.12.2), the fork-PR-against-`main`
  workflow, conventional commits, and the public-API stability contract.
- **[CODE_OF_CONDUCT.md](../../CODE_OF_CONDUCT.md)** — Contributor Covenant 2.1.

This file used to duplicate that guidance and had drifted (it named tooling and a branch model the
project no longer uses, plus links to files that were moved or removed). Rather than maintain two
copies, it now points at the root `CONTRIBUTING.md` as the single source of truth.

For the deeper testing rationale see [Test Coverage Strategy](./test_coverage_strategy.md).
