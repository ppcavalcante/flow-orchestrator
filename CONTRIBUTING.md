# Contributing to Flow Orchestrator

Thank you for your interest in contributing! This document is the canonical contributor
guide. (`docs/development/contributing.md` is a short pointer to this file.)

Flow Orchestrator is **pre-1.0 alpha** — see [STABILITY.md](STABILITY.md) for what is and
is not covered by compatibility guarantees before you build on a given surface.

## Code of Conduct

This project adheres to the [Contributor Covenant](CODE_OF_CONDUCT.md). By participating you
are expected to uphold it. Report unacceptable behavior to `research@martila.io`.

## Development setup

Requirements:

- **Go 1.24** (matches `go.mod` and CI).
- The **FlatBuffers compiler** (`flatc`) — needed to regenerate the serialization code.
  On Debian/Ubuntu: `sudo apt-get install -y flatbuffers-compiler`.

Common tasks are driven through the `Makefile`:

```bash
make generate-fb        # regenerate FlatBuffers code (run after touching *.fbs)
make build              # go build ./...
make test               # go test ./...
go test ./... -race     # the race detector run CI and reviewers expect to be green
make lint               # golangci-lint (pinned v2.12.2, run via `go run` — no local install needed)
```

`make lint` pins golangci-lint to **v2.12.2** and runs it via `go run`, so it is reproducible
regardless of which (if any) `golangci-lint` binary you have installed locally. The lint config
is [`.golangci.yml`](.golangci.yml) (v2 schema).

## Branching & pull requests

The default branch is **`main`**. There is no `develop` branch.

1. Fork the repository and create a topic branch off `main`
   (`git checkout -b feature/your-feature-name main`).
2. Make your changes; keep them focused.
3. Ensure the gate is green locally: `make build`, `go test ./... -race`, `make lint`.
4. Update documentation for any public-API or behavior change, and add a `CHANGELOG.md`
   entry under `[Unreleased]`.
5. Commit using [Conventional Commits](https://www.conventionalcommits.org/)
   (e.g. `fix(store): reject oversize .fb before decode`).
6. Open a pull request **against `main`** and fill in the PR template.

## Code guidelines

- Follow standard Go style; format with `gofmt` (CI checks `gofmt`/`goimports`/`gci`).
- Run `make lint` and resolve findings; if a lint must be suppressed, use a narrowly-scoped
  `//nolint:<linter> // reason` with a real justification (see the existing `gosec` G304
  suppressions on controlled-path file I/O for the expected style).
- Document exported functions, types, and constants.
- Public API stability is governed by [STABILITY.md](STABILITY.md) — a breaking change to a
  covered surface must be called out in the PR and the CHANGELOG.

## Testing

- Add tests for new features and bug fixes.
- Keep `go test ./... -race` green — the race run is the project's Decision-grade gate.
- Property-based tests use `gopter`; serialization changes need round-trip/fidelity coverage.
- See [Test Coverage Strategy](docs/development/test_coverage_strategy.md).

## Reporting bugs & suggesting features

Use the GitHub [issue tracker](https://github.com/ppcavalcante/flow-orchestrator/issues) and
the provided issue templates (bug report / feature request).

## License

By contributing, you agree that your contributions are licensed under the project's
[MIT License](LICENSE).
