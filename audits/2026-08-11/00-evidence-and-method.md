# Evidence and method

## 1. Snapshot discipline

The audit began on local `anvil-m1` at `f617542`. Three concurrent commits landed while evidence/reporting was in progress:

```text
d570940 feat(api): AUD-025 -- store-only ApprovalNonceFromStore helper
f172c80 docs(examples): rebuild the examples suite as a capability progression
e215f47 docs: fix API drift vs HEAD + add the M22/M23 ADRs
```

`d570940` committed the helper files already present as initial working-tree changes. The later two commits replaced the examples and changed CI/public documentation. The audit cancelled the first mixed-snapshot race run, re-anchored after each commit, and reran the affected build, vet, lint, doctest, examples, formal, external-probe, and full-race gates at the final commit.

Final audited code subject:

```text
branch: anvil-m1
HEAD:   e215f47a6d9841ac0cadbca1b9a0bf79551cf58e
Go:     go1.25.11 darwin/arm64
```

Evidence has three explicit scopes:

- **Committed code:** source, tests, docs, and workflows as stored in `e215f47`.
- **Local repository state:** ignored planning content, disk/line counts, worktree changes, and untracked files observed on 2026-08-11.
- **External repository state:** GitHub issues, tags, and workflow history queried on 2026-08-11.

At the final code-verification checkpoint, the worktree contained no tracked changes; untracked entries were the pre-existing `anvil-cc`, `anvil-up-ollama`, and the new `audits/` bundle.

This remains weaker than an audit in a frozen worktree: the committed source content was reconciled and final gates rerun, but the moving-worktree process itself is recorded as a residual.

## 2. Sources reviewed

Evidence priority:

1. executable behavior on the final source content;
2. source/control-flow and repository history;
3. committed tests, CI, and release workflows;
4. public docs and `STABILITY.md`;
5. ignored planning records;
6. comments or hand-maintained claims.

Reviewed material included:

- `.planning/STATE.md`, roadmap/requirements, reviewer briefing, architecture/dataflow/public-API maps, phase summaries, and audit/remediation records;
- public README, stability contract, architecture, getting-started, reference, guide, development, and release docs;
- all 2026-08-10 audit documents and all 71 register entries;
- core DAG/builder/node/executor, WorkflowData, durable workflow, store codecs, SQLite queue/fencing, signal/approval, pool/scheduler, action mediation, boundaries, digest, and CI/formal paths;
- current repository/tag ancestry and public GitHub workflow history.

A full code knowledge graph was built at the initial core-code snapshot as a navigation aid (6,521 nodes / 36,641 edges). That snapshot already included the helper content later committed by `d570940`; the later two commits changed docs/examples/CI rather than core workflow code. Graph results were still not treated as proof where direct source or execution was available.

## 3. Executed verification ledger

### Build, static checks, and module integrity

| Command | Result |
|---|---|
| `go build ./...` | Pass |
| `CGO_ENABLED=0 go build ./...` | Pass |
| `CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build ./...` | Pass |
| `go vet ./...` | Pass |
| `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run --config .golangci.yml` | Pass, `0 issues` |
| `go mod verify` | Pass, all modules verified |
| `go mod tidy -diff` | Pass, no diff |
| `gofmt -l pkg internal examples playground` | Pass, no files listed |
| `go generate ./pkg/workflow/...` followed by generated-file diff check | Pass, no generated diff |

### Full race suite

Final-SHA command:

```text
go test -race -timeout 30m ./...
```

Result: **fail** in 1,087.13 seconds. All packages completed except `pkg/workflow`, which failed after 1,073.097 seconds in `TestVB09_TerminalWriterSetMatchesTheGolden`. The deriver reported six `REMOVED` golden entries: `SetNodeStatus(Completed)` and `SetOutput` in each of the deleted `examples/new_simple` actions `fetchAction`, `processAction`, and `saveAction`. Those removals are the desired mediation cleanup; the checked-in golden is stale.

A targeted non-race reproduction, `go test ./pkg/workflow -run '^TestVB09_TerminalWriterSetMatchesTheGolden$' -count=1`, failed on the same changed writer set. A prior complete race run at `d570940` passed in 1,059.02 seconds but was superseded when the examples changed; an earlier mixed-snapshot run was intentionally cancelled. Neither is counted as final-SHA evidence.

### Targeted regressions and stability checks

| Command / scope | Result |
|---|---|
| Prior-audit regression regex covering AUD tests, approval store helper, boundary nested-DAG rejection, TopologicalSort, composite nested-DAG rejection, merge rejection, compensation nested-DAG rejection, and sealed view | Pass |
| `TestNoDelayRetryMiddleware/Context_cancellation` + `TestQueueAdversarial_TwoWorkersReclaimParkedChild_ExactlyOneResumes`, `-count=20` | Pass |
| `TestPerfCeiling_DetTax`, `GOGC=800` | Pass |
| `TestApprovalNonceFromStore...` after the helper commit | Pass |
| `go test ./internal/doctest -count=1` at `e215f47` | Pass |
| targeted `TestVB09_TerminalWriterSetMatchesTheGolden` at `e215f47` | **Fail**; golden still expects six removed `examples/new_simple` writer sites |
| `go test ./...` in `examples/12-observability` | Pass |

The doctest pass is not accepted as API-freshness proof because `docs/architecture/dag-execution.md` still contains the removed `TopologicalSort` signature.

### Vulnerability scan

Command:

```text
go run golang.org/x/vuln/cmd/govulncheck@v1.4.0 ./...
```

Observed result:

```text
No vulnerabilities found.
Your code is affected by 0 vulnerabilities.
The scan also found 1 vulnerability in packages you import and
2 vulnerabilities in modules you require, but your code doesn't appear to call these vulnerabilities.
```

This establishes no currently reachable advisory under govulncheck’s symbol analysis. It is not a proof of absence of logic vulnerabilities.

### Formal capstones

Command:

```text
./scripts/formal/run_tlc.sh
```

Result: **8/8 pass** using pinned `tla2tools v1.8.0`, including:

- `Executor.cfg` — 13 states;
- `ExecutorHardFail.cfg` — 14 states;
- `ExecutorConc1.cfg` — 12 states;
- `DurableExecutorClean.cfg` — 426 states;
- `DurableExecutor.cfg` — 426 states;
- `DurableExecutorHardFail.cfg` — 318 states;
- `M10DurableExecutor.cfg` — 14,380 states;
- `M10FailResume.cfg` — 915 states.

These checks prove the bounded models under their configurations, not refinement of the Go implementation.

### End-to-end examples

Command:

```text
make examples
```

Result: **pass** in 23.92 seconds. The rebuilt suite executed 13 numbered capability examples plus `capstone-document-pipeline`, including a real child-process kill/resume scenario.

CI’s separate loop now uses `set -euo pipefail` and a 300-second per-example timeout. The local Make target and CI remain duplicate implementations, but the prior `new_simple` mediation failure is gone at `e215f47`.

## 4. External behavioral probes

A temporary external module imported the public package via a local module replacement. It did not use package-private access.

Observed output:

```text
clone nested-slice="mutated" typed-map="mutated" pointer="mutated"
typed-nil store panic=runtime error: invalid memory address or nil pointer dereference
typed-nil workflow store panic=runtime error: invalid memory address or nil pointer dereference
action-body resume first=<nil> second=<nil> load=<nil> persisted="v1"
nil DAG data panic=runtime error: invalid memory address or nil pointer dereference
flatbuffers signal deliver=<nil> take=<nil> count=1 enqueued-at=0
```

Interpretation:

- Clone recursively copies generic-map branches but only shallow-copies each `[]any` container; the probe’s inner `[]any`, typed map, and pointer therefore remain shared.
- Interface `store == nil` checks do not catch typed-nil concrete stores.
- structurally identical graphs with changed action bodies share a digest and resume without a mismatch; this matches the documented structural-only digest scope and is treated as an architectural residual, not a current defect;
- nil DAG data is not rejected at the public boundary;
- FlatBuffers drops the delivery timestamp stamped before encoding.

## 5. Repository and process measurements

Observed at the final snapshot:

- production Go files excluding generated bindings: 91;
- production Go lines: 24,796;
- comment-only production lines: 10,075 (40.6%);
- `.planning` Markdown lines: 115,822;
- `.planning` disk use: roughly 154 MiB;
- tracked files under `.planning`: 0;
- open GitHub issues returned for the repository: 0;
- current `HEAD` has no tag;
- `v0.21.0-alpha` points at `4108150` and shares merge base `bee7258` with current `HEAD`, rather than being its ancestor.

The visible latest public GitHub runs are the July 24 `main`/release runs. No remote workflow run exists for local `anvil-m1`, so local results are not represented as public CI evidence.

## 6. Tool limitations and discarded evidence

- Workspace LSP diagnostics incorrectly routed this Go repository to TypeScript and then reported no Go language server. This was reported to harness QA and not used as evidence.
- SMTC orientation reported zero files and its security summary omitted the test surface; those results were structurally incomplete and were not used to claim security coverage.
- Code-graph call edges were used for navigation only; direct source and executed behavior controlled conclusions.
- No long-duration fuzz campaign or mutation campaign was rerun. Existing bounded-fuzz and mutation workflow design was reviewed.
- Focused coverage thresholds were not recomputed in this audit. The gate implementation was reviewed, but no current percentage is claimed.
- No live service penetration test was applicable: this repository is an embedded Go library with no listener by default.
- Local filesystem/process trust boundaries were reviewed; no hostile multi-host deployment was available for dynamic testing.

## 7. Decision rule

A green package/race/formal suite does not override a directly reproduced public or backend contract failure outside test coverage. Conversely, an architectural ceiling or accepted alpha limitation is not labeled a runtime defect without a violated stated contract.

Release blockers are high-severity findings, failures of a declared blocking gate, release-identity risks, and medium findings explicitly required to make published documentation/evidence truthful. Under that rule the release-blocking scope of all seven current findings must close before GO; accepted planning-size/hygiene debt remains separately identified. Those criteria yield the NO-GO decision recorded in the bundle.