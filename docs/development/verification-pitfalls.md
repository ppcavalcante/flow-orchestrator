# Verification pitfalls & deferred residuals

Reusable engineering knowledge for anyone changing or reviewing this codebase. It was learned
the hard way during M23 and previously lived only in `.planning/` (gitignored, so absent from a
clean clone). It is promoted here because it is **generalizable** — not milestone trivia.

Two kinds of thing live here:

1. **Gates that have produced greens that don't mean what they appear to** — so you distrust the
   right signal.
2. **Deliberately-deferred in-code residuals** — invariants that are *not* fully enforced, recorded
   here so a future refactor cannot silently erase the reasoning that lives next to the code.

---

## 1. Gates that produced misleading greens

Each of these was a real incident: a gate reported success while the property it was supposed to
prove did not hold. Each was found by a different person, none of them looking for it.

| Gate | How the green lied | Guard now in place |
|---|---|---|
| **Package tests** | **Sampling** — one `--- FAIL` and one `--- PASS` at the *same* commit; a `-run` subset deselected the package-population guards that would have failed. | A `-run` filter is blind to package-wide censuses — run `./...` (or the full package) before trusting a suite verdict, never a narrow `-run` alone. |
| **`-race` coverage targets** | **Truncation** — a run killed by a timeout emits a *partial* coverage profile that looks like a smaller-but-valid result, not a failure. | **Always `-timeout 30m`** on the heavy suites; capture the exit status directly (`$?`), never pipe a gate through `tail`/another stage that masks the non-zero exit. A killed gate looks exactly like a green one (exit non-zero, zero `--- FAIL`). |
| **`.planning/codebase/` records** | **Never ran** — records that were apparently never validated green were quoted as if they had been. | Prove a control by content, not by narration — a record that asserts a gate ran is not the gate running. |
| **A worker/agent spawn** | **Fabrication** — a spawn that never executed left a stub artifact reporting "succeeded" in ~1 ms with 0 tokens. | Read the provenance block (duration, tokens, exit), never the mere existence of an output file. |
| **Doc examples** | **Parses without compiling** — a shipped snippet is syntactically valid but does not build. | The doctest gate now *compiles* every fenced Go block, not just parses it. But see the fragment blind spot below. |

**The fragment-compile blind spot (generalizable).** Even "the compiler is the only witness" is too
strong for *fragments*. Go marks an expression containing an **undefined identifier** invalid and
*stops type-checking it* — so an illegal type assertion downstream of an undefined symbol is never
checked at all, and a raw `grep` of the compiler output finds zero errors. A compile sweep over
fragments therefore has a blind region **proportional to how fragmentary the examples are**, and it
reports clean over that region. When verifying doc snippets, ensure fragments declare their
identifiers at correct types before you trust a clean compile.

**The through-line:** a green is evidence, not proof. Before trusting one, ask *what would this gate
look like if the property were false?* — if the answer is "the same," the gate isn't a gate.

---

## 2. Deliberately-deferred in-code residuals

These are known, accepted gaps. They are recorded here (in tracked content) so they cannot be lost
to a refactor — a residual whose only home is a code comment is one edit away from silent removal.

### `Validate()` ≠ `build()` (in-package edge-append can bypass structural validation)

`validateReconvergence` and `validateBoundaries` run **only inside `build()`**. `DAG.Execute` calls
`Validate()` on every drive, and `Validate()` is **cycle-detection only**. So an *in-package* caller
that appends an edge after `build()` can create a reconvergence/boundary violation that `build()`
would have refused, and no per-drive check will notice.

- **Why it's safe in practice:** the M23 SEAL work closes this from *outside* the package — both
  public post-`build` mutators (`(*DAG).addDependency`, `(*Workflow).addDependency`) are unexported,
  and the builder token is checked at drive time. An external consumer cannot construct the
  violation.
- **Why it's deferred, not fixed:** re-running reconvergence/boundary validation on every drive is
  an `O(V+E)` cost on every timer wake — an architect ruling declined that cost for an in-package
  hazard the seal already contains.
- **The invariant a refactor must preserve:** keep the post-`build` dependency mutators unexported,
  or add per-drive re-validation if that ever changes. This corresponds to reconciliation finding
  `F-M23-MAP-02`.

---

*Sources: the misleading-greens table is M23's cross-seat verification record; the `Validate()` ≠
`build()` residual is documented at its site in `dag.go`/`node.go` and in the `built` field's doc.*
