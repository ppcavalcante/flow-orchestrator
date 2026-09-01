# Evidence and method

## 1. Snapshot discipline

The code subject is `c9aef0c499f21f90a168190c75bdf310182cf001` on `anvil-m1`.

The worktree was concurrently active. At the beginning, four documentation files were modified and two root helper binaries were untracked. During the audit, additional documentation, `.gitignore`, and `pkg/workflow/boundary_vb07_test.go` became modified without this auditor editing them. The latter adds further phase-119 findings/instrument repairs but remains uncommitted and is outside the committed subject. This audit therefore treats:

- committed Go code at `c9aef0c` as the implementation subject;
- documentation as a mixed worktree snapshot, explicitly not a committed final-doc attestation;
- evidence as valid only for the command and SHA stated.

The new `audits/2026-08-10/` directory is the only repository content authored by this audit. It was moved from singular `audit/` after another seat concurrently added an uncommitted `audit/*` ignore rule.

## 2. Repository census

Measured locally:

| Item | Measurement |
|---|---:|
| Tracked files | 522 |
| Tracked size | ~6.6 MiB |
| Workspace size | ~149 MiB |
| Production Go LOC (`pkg`, `internal`) | 24,271 |
| Test Go LOC | 73,405 |
| Test functions (`pkg`, `internal`) | 1,230 |
| Test functions in `pkg/workflow` | 1,160 |
| Benchmarks | 70 |
| Fuzz functions | 8 |
| `pkg/workflow` production files | 67 |
| `pkg/workflow` test files | 214 |
| TLA+ modules | 19 |
| TLC configs | 38 |
| Planning Markdown | 114,908 lines |
| Public docs Markdown | 12,638 lines |
| Production comment-only lines in `pkg/workflow` | 8,989 |
| Production comment-only / all production lines in that package | ~42.3% |
| Comment-only / non-comment production lines | ~0.73 |

Planning grew by roughly twelve thousand lines between the earlier 2026-08-04 review and this snapshot, mostly through the phase-119 review/fix tail.

## 3. Commands and results

### Passed

```text
go build ./...
go vet ./...
CGO_ENABLED=0 go build ./...
CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build ./...
go mod verify
go mod tidy -diff
git diff --check
gofmt scan of pkg/ and internal/
go test ./pkg/workflow -run 'TestVB09|TestVB07|TestBoundary_J1|TestBARM23' -count=1 -timeout 10m
./scripts/testing/run_tagged_adversarial.sh
```

The targeted phase-119/BAR run completed in 10.6 seconds.

### Failed

#### Clean-cache lint

`golangci-lint v2.12.2` reported **22 findings**:

- 16 `errcheck`;
- 1 `gci` formatting/import-order issue;
- 5 `staticcheck`/quickfix findings.

Several are intentionally ignored test results, but the project’s configured gate does not know that until narrowly justified `nolint` annotations or code fixes are added. Current `SECURITY.md` says configured lint is zero; that is false for this tree.

Notably, phase 119 itself contributes:

- unchecked `dag.Execute` in `boundary_vb07_test.go`;
- import-order failure in `boundary_vb07_ext_test.go`;
- multiple `WriteString(fmt.Sprintf(...))` findings;
- one VB-09 formatter finding.

#### Vulnerability scan

Local Go is `1.25.1`. `govulncheck ./...` found two called standard-library vulnerabilities:

- `GO-2026-4602` through `os.ReadDir`, fixed in Go 1.25.8;
- `GO-2026-4341` through `net/url` parsing on the SQLite path, fixed in Go 1.25.6.

CI’s release arm uses 1.25.11 and should be clean. The result shows that the repository allows contributors to use a vulnerable floor/default toolchain without a `toolchain` directive steering ordinary development to the patched release.

## 4. Executed external-consumer probes

The probes were built as separate temporary modules with a `replace` directive to the repository. They used only public API except where explicitly noted.

### Builder purity

```text
first Build:  target dependencies = 1
second Build: target dependencies = 2
```

Cause: `build()` appends choice/merge-generated edges into retained `NodeBuilder.dependencies`.

### Clone and InMemoryStore isolation

```text
clone-after-source-mutation x=2
inmem-after-original-mutation x=11
inmem-after-loaded-mutation x=12
```

The original values were 1, 10, and 11 respectively. Nested map identity is shared through `Clone`, Save, and Load.

### JSON lookup-key identity

A valid snapshot with payload ID `B` was placed at `A.json`:

```text
json-load-key-A payload-id="B" err=<nil>
```

`JSONFileStore.Load("A")` initializes ID `A`, then `loadSnapshotInternal` overwrites it from payload field `id` without checking equality.

### Metrics on durable resume

```text
first execute:  enabled=true
second execute: enabled=false
```

A workflow with `MetricsConfig.Enabled=true` lost metrics after JSON Load replaced the configured `WorkflowData` with one constructed using default-disabled metrics.

### Legacy action

A legacy callback returned `("result", errors.New("lost-error"))`:

```text
build=<nil> execute=<nil> status=completed
```

Both return values are deliberately discarded by the adapter.

### Boundary/J1 semantics

A graph `D -> V -> S` with `V.WithContinueOnError()` structurally satisfies “V occurs before S,” but Build returned:

```text
verifier "V" carries ContinueOnError ... the declaration would constrain order alone
```

That reason directly conflicts with the surrounding contract saying the boundary is “exactly” precedence/order and nothing more.

### Public nil handling

```text
FromBuilder(nil): panic: nil pointer dereference
RunNext(... nil registry ...): panic: nil pointer dereference
```

This is inconsistent with nearby constructors that return typed `ErrValidation` for nil dependencies.

### Multi-process owner identity

Two independent SQLiteStore instances opened the same DB in multi-process mode and called `Claim("claim-wf", "")`:

```text
store 1: token=1 error=nil
store 2: token=1 error=nil
```

The second process was treated as a re-entrant claim by the same owner. Both store-local token maps hold the same current fencing token, so owner uniqueness is a correctness precondition, not cosmetic metadata.

### Pool misconfiguration

A `Pool` factory returned a valid SQLite store without `WithMultiProcess`. The registry was non-empty:

```text
NewPool error=nil
Pool.Run error=nil
context=context deadline exceeded
```

`ClaimNext` repeatedly returns a permanent validation error; `runWorker` discards it as if it were idle/transient and eventually reports clean shutdown.

### Re-entrant iteration

A `WorkflowData.ForEach` callback called `Set` on the same object:

```text
callback deadlocked
```

The callback executes while `RLock` is held. `ForEachWait` documents this caveat; the other public iteration methods mostly do not.

### Wide static level

A 5,000-node root level used the default concurrency of 16. Actions blocked on a gate:

```text
nodes=5000 maxConcurrency=16 started=16 goroutines=5002
```

The semaphore bounds executing actions, not goroutine count.

### Copied mutex

The repository’s own tagged gate reported:

```text
REPRODUCED: a stamped copy that captured a held write lock hangs forever in Validate
REPRODUCED with EXPORTED API ONLY: 1/3000 copies ... were born permanently wedged
```

The target returns success because reproduction is its expected result.

## 5. Remote/release evidence

- Latest release/tag CI inspected: `v0.21.0-alpha`, successful release workflow.
- The corresponding `main` CI run was marked cancelled after the informational mutation job ran for six hours; all blocking jobs had succeeded on that older tree.
- There is no remote CI run for current M23 HEAD.
- Current branch is one commit behind and 243 commits ahead of `origin/main` in left/right count.
- `v0.21.0-alpha` is not an ancestor of current HEAD.
- There are zero open GitHub issues despite numerous accepted internal findings.
- Open PRs are old Dependabot updates, not M23 integration/review PRs.

## 6. Corrections and cautions on the reviewer briefing

The briefing is valuable and unusually candid, but it is not self-proving.

1. It says every number was measured at `c9aef0c`; it reports 1,155 `pkg/workflow` tests, while the source at that SHA contains 1,160 by a direct function census.
2. It correctly identifies `test-coverage-focused` as lacking a timeout, then says CI is safe because the race test step has `-timeout 30m`. The CI **coverage job invokes `make test-coverage-focused` itself**; one job’s timeout flag does not protect another process. CI coverage is therefore not established safe.
3. It omits the copied-mutex hang from its ordered open-issue list even though M24 requirements call it a direct hard-bar violation and the tagged gate reproduces it.
4. It names `pkg/workflow/suspendable_routing_test.go`; the current file is `internal/doctest/suspendable_routing_test.go`.
5. “Formally verified engine” is stronger than `specs/README.md`’s honest scope. The TLA+ models verify abstractions; model-to-Go faithfulness remains human-reviewed.
6. The phase status is correctly described as a fix tail in the briefing, but `SUMMARY.md` says COMPLETE while the last independent review/QA records are failures over earlier SHAs. No final independent pass covers `047bb68..c9aef0c`.

These are not reasons to discard the briefing. They demonstrate its own central lesson: a reported site or number is a sample until independently derived.

## 7. Limits of this audit

- A full final-SHA `go test -race ./...` was not rerun; project evidence puts it at 16–21 minutes under current load, and the worktree was concurrently changing in docs. This audit does not claim a fresh full-race pass.
- TLC was not rerun because the local default Java is a stub and no repository CI target automates model checking. Existing models and run instructions were reviewed structurally.
- Concurrent uncommitted changes, including the phase-119 test edits that appeared near the end, are not part of the `c9aef0c` subject and were not certified by this audit.
- The four persistence/dispatch families were sampled deeply, not every SQL branch manually re-proven.
- Findings marked **inferred risk** in the register are explicitly distinct from executed defects.
