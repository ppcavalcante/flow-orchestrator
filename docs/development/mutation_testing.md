# Mutation testing

Mutation testing is the rung **above** line/branch coverage. Coverage tells you a
line _executed_ during the suite; mutation testing tells you whether a test would
_notice_ if that line's behavior changed. A tool ([go-gremlins][gremlins])
systematically mutates the source — flips a `>` to `>=`, a `==` to `!=`, `+` to
`-` — recompiles, and re-runs the suite for each mutant:

- **KILLED** — a test failed → the behavior change was caught. Good.
- **LIVED** — every test still passed → a behavior change no test caught. Either
  a real test-quality gap, or an *equivalent mutant* (a change that doesn't alter
  observable behavior and so is unkillable by definition).
- **NOT COVERED** — no test even exercises the mutated line (coverage gap).
- **TIMED OUT** — the mutant caused a hang (often a mutated loop/condition);
  counts as killed-ish (the change was detectable, just via timeout).

The metric is **efficacy = KILLED / (KILLED + LIVED)**. Chasing efficacy to 100%
is an anti-pattern: equivalent mutants are *intentionally* unkillable, so a high
score is only meaningful once survivors have been triaged. We do **not** add
tests to chase the number against equivalent mutants — that is test theater.

## How to run it

go-gremlins is a **dev tool**. It is deliberately NOT a dependency of the library
(it stays out of `go.mod` and the non-test dependency graph). Run it with the
pinned version against the scoped package:

```sh
go run github.com/go-gremlins/gremlins/cmd/gremlins@v0.6.0 \
    unleash --config .gremlins.yaml --timeout-coefficient 60 ./pkg/workflow
```

or with an installed binary (`gremlins unleash --timeout-coefficient 60 ./pkg/workflow`).

### Two gotchas (both are configured/encoded so they don't bite again)

1. **`exclude-files` patterns are relative to the RUN TARGET**, not the repo root.
   Running `unleash ./pkg/workflow` walks `os.DirFS(pkg/workflow)`, so patterns
   must be `action.go` and `metrics/.*\.go` — **not** `pkg/workflow/action.go`.
   The repo-relative form silently matches nothing (the scope no-ops). See the
   comments in `.gremlins.yaml`.
2. **`--timeout-coefficient 60` is required.** Gremlins derives the per-mutant
   timeout from its *coverage* run (~1.1 s here), not the real suite (~13 s, which
   the gopter property suites dominate). The default 3× → ~3.4 s ≪ 13 s, so
   **every** mutant times out and you get a false 0% efficacy. 60× (~69 s budget)
   is comfortably above the real suite. This is also set in the CI job.

### Scope

`.gremlins.yaml` mutates only the **6 core files** where mutation coverage carries
the most signal — the executor + data + chunk-2/5/6 logic:

`dag.go`, `parallel_execution.go`, `node.go`, `workflow_data.go`,
`execution_error.go`, `tracing.go`.

Everything else under `pkg/workflow` (and the `metrics` subpackage) is excluded so
the run stays fast and the survivor report stays focused.

## CI

A **non-blocking, informational** GitHub Actions job (`.github/workflows/ci.yml`,
job `mutation`) runs gremlins on every push/PR with `continue-on-error: true`.
Mutation testing is too slow and too sensitive to equivalent-mutant churn to gate
the build on — it never fails CI. It exists to surface NEW real gaps over time;
read its log, don't trust its exit code.

## Accepted survivors (triaged baseline, M8 Phase B)

The chunk-6 part-B baseline run scored **efficacy 65.3%** (79 killed / 42 lived,
100% mutator coverage). Triage found the survivors are dominated by **equivalent
and low-value** mutants, not real gaps. Two genuine gaps were found and **killed**
with real tests; the rest are documented here so a future "why is efficacy 65%?"
does not re-litigate them.

### Killed (genuine gaps closed)

| Site | Mutant | Kill test |
|---|---|---|
| `dag.go` StartNodes/EndNodes guard | `len(DependsOn) == 0` → `!= 0` (and the EndNodes companion) | `TestValidate_StartAndEndNodes` — asserts the contents of the **public** `DAG.StartNodes`/`EndNodes` fields, previously unasserted public surface. |
| `workflow_data.go` snapshot element-count bound | `len(m) > defaultMaxElements` → `>=` | `TestLoadSnapshot_ElementCountCap/at-exact-cap-accepted` — a section of **exactly** `defaultMaxElements` entries must be accepted; the existing cap+1 case alone left the exact-cap boundary unpinned. A real safety/availability boundary (M5 trust-contract family). |

### Accepted equivalent / low-value (NOT worth a test)

| Family | Sites | Why it survives / why we accept it |
|---|---|---|
| **Capacity-hint arithmetic** (~16) | `dag.go` `nodeCount/4+1`, `/2+1`; `node.go` `currentLen+len(deps)`; ctor `nodeCapacity/4+1` | Pure pre-allocation sizing. Mutating the hint changes only the *initial* slice capacity; slices grow as needed, so behavior is identical. Killing them would require asserting `cap()` — testing the implementation, not the contract. **Equivalent.** |
| **Off-by-default metrics sampling guard** (~18) | `workflow_data.go` `GetSamplingRate() < 1.0 && SecureRandomFloat64() > GetSamplingRate()` (dup'd across Set/IsNodeRunnable/Snapshot/LoadSnapshot fast paths) | Metrics are OFF by default, so `!IsEnabled()` short-circuits before the RHS ever runs; and the RHS is genuinely **random** (`SecureRandomFloat64`), so a boundary shift changes only the *sample fraction*, never correctness. Deterministically killing a probabilistic sampling boundary is not feasible or valuable. **Low-value / equivalent.** (This is metrics/chunk-4 code that happens to live in `workflow_data.go`.) |
| **Unique-key sort comparators** (3) | `execution_error.go:107`, `dag.go:270`, `dag.go:308` — `a.Name < b.Name` → `<=` | Node names are unique within a DAG, so `<` and `<=` produce identical ordering. **Equivalent.** |
| **Running-max boundary** (~4) | `dag.go:248` `nodeLevels[..]+1`, `dag.go:253` `> maxLevel` (GetLevels) | `> maxLevel` is a running maximum (`==` can never raise it), so the boundary mutant is equivalent; the `+1` level increment is exercised by the topological-order property but its boundary variant does not change the partition. **Equivalent.** |
| **Defensive nil-guard, unreachable else** (1) | `workflow_data.go:247` `stringInterner != nil` → `== nil` | `stringInterner` is set non-nil by every public `WorkflowData` constructor, so the `nil` (fallback) branch is unreachable in production. Killing the mutant would require poking the unexported field to nil and asserting a branch that cannot occur — testing dead defensive code. **Equivalent in practice.** (The field is live; this is not dead code to remove — only the fallback arm is unreachable.) |

[gremlins]: https://github.com/go-gremlins/gremlins
