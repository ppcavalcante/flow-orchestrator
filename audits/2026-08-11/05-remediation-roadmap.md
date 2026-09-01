# Remediation roadmap

## Guiding rule

Do not add another feature until the next release can be derived from one immutable commit and one honest finding ledger. Runtime fixes, public docs, tests, and release metadata must describe the same tree.

## Stage 0 — establish a tracked remediation baseline

1. Create an isolated remediation worktree from the selected base commit.
2. Prevent concurrent unrelated commits in that worktree.
3. Move the minimal canonical release decision/finding statuses into tracked content.
4. Record the base SHA for before/after comparison; do not call it the release candidate.
5. Keep implementation status (`fixed`, `partial`, `open`, `superseded`) separate from release disposition (`block`, `accepted-risk`). Do not use “remediated” as an umbrella that collapses those dimensions.

**Exit:** a clean clone contains the strict ledger and identifies the remediation baseline without local `.planning` state. The final candidate is frozen only after Stages 1–2 change the tree.

## Stage 1 — close runtime release blockers

### 1. Restore FlatBuffers freshness fidelity (`CUR-001`, `AUD-025`)

- add `enqueued_at:int64` to the FlatBuffers Signal schema;
- regenerate committed bindings;
- encode and decode `Signal.EnqueuedAt`;
- add FlatBuffers to the shared delivery/re-delivery timestamp test;
- add a backward-compatibility test showing old entries decode to zero/unknown;
- document that freshness-enforcing consumers reject zero.

**Exit:** the same conformance test passes for SQLite, JSONFile, FlatBuffers, and InMemory stores; external probe returns a non-zero timestamp.

### 2. Finish exported nil policy (`CUR-002`, `AUD-001`, `AUD-031`)

- reject nil `WorkflowData` in `DAG.Execute` with `ErrValidation` before any work;
- reject typed-nil `WorkflowStore` in `ApprovalNonceFromStore`;
- validate required store/DAG/locker dependencies at construction or public drive entry;
- inventory exported entry points accepting interfaces or pointers and add nil/typed-nil tables.

**Exit:** ordinary invalid inputs return typed errors; no public misuse test recovers a nil-pointer panic.

### 3. Make Clone honest (`CUR-003`, `AUD-013`)

Choose one explicit contract:

- **Preferred:** support only the documented canonical value algebra, deep-copy every supported composite recursively with cycle/size bounds, and reject unsupported mutable shapes; or
- define Clone as a shallow/partially structural snapshot and remove every “deep” and isolation claim.

Do not silently stringify in direct Clone and do not implement unconstrained reflection copying.

**Exit:** mutation-after-clone tests cover nested slices, typed maps (if supported), pointers/custom types (supported or rejected), repeated references, and cycles; docs/changelog match behavior.

## Stage 2 — repair verification and consumer truth

### 4. Fix documentation contract verification (`CUR-005`, `AUD-039`, `AUD-048`, `AUD-054`, `AUD-065`)

- update the remaining stale `TopologicalSort` architecture section and keep the corrected API reference;
- clarify whether SQLite is intentionally outside the “three built-in stores” category, then reconcile every store-count/interchangeability claim against the four-implementation matrix;
- generate exported API reference from source or validate declaration blocks with `go/types`;
- make signature and public-inventory drift fail `internal/doctest` or a dedicated docs gate;
- sample every public code fence for semantic compilation, not parser acceptance alone.

**Exit:** deliberately changing an exported signature or concrete store inventory without docs updates makes the docs gate fail.

### 5. Refresh the mediation-writer golden (`CUR-006`)

- inspect the six removed `examples/new_simple` writer entries and record that each disappeared because the action-side journal writes were deleted;
- regenerate or deliberately update the checked-in VB-09 writer set without weakening the AST deriver;
- run `TestVB09_TerminalWriterSetMatchesTheGolden` directly;
- rerun the complete `go test -race -timeout 30m ./...` gate on the current isolated remediation SHA; the full ledger runs it again after the final candidate is frozen.

**Exit:** the targeted and full-race gates pass, while deliberately adding an unmediated terminal writer makes VB-09 fail.

### 6. Bind release tag to source identity (`CUR-007`, `AUD-009`)

Add a preflight script/test that fails unless:

- tag `vX.Y.Z-suffix` equals `workflow.Version` and `VersionInfo`;
- `CHANGELOG.md` contains the exact release section/date;
- the candidate descends from the previous published tag; an intentional replacement lineage instead requires a tracked ADR and explicit accepted/superseded disposition for `AUD-009`;
- tests, lint, race, formal capstones, generated-code diff, docs, and examples all pass on that SHA;
- the SBOM/provenance subject is produced from that exact tag checkout.

**Exit:** a deliberately mismatched tag/version fails before GitHub Release creation. For the default closure path, the next candidate descends from the prior published tag and becomes an ancestor of subsequent development. An intentional replacement lineage is GO-eligible only after a tracked policy decision explicitly reclassifies `AUD-009`.

## Stage 3 — reduce recurrent evidence debt

1. Generate API/test/source counts; stop hand-copying census numbers.
2. Reduce `.planning` to current decisions plus compact indexed history; archive bulky scratch artifacts elsewhere.
3. Track release-bearing state; ignored files may inform work but may not be required evidence.
4. Open public issues for accepted technical debt and link each release acceptance to an issue/owner.
5. Replace the wall-clock O(N) test with a structural allocation/goroutine/algorithm guard.
6. Publish a generated per-type concurrency matrix rather than broad “thread-safe” language.
7. Continue deleting comments that narrate historical review rounds without specifying a current invariant.
8. Decide whether a future stability tier needs explicit caller-supplied action versions; the current documented structural digest instead requires host deployment/migration discipline.

**Exit:** a fresh reviewer can reproduce release status from tracked files and standard commands without `.planning`, private launchers, or oral history.

## Suggested order

1. Track the minimal strict release ledger and isolate one remediation baseline (`CUR-008`, first half).
2. `CUR-001` FlatBuffers timestamp.
3. `CUR-002` nil/typed-nil policy.
4. `CUR-003` Clone contract.
5. `CUR-005` docs gate.
6. `CUR-006` writer golden and full-race gate.
7. `CUR-007` release preflight.
8. Freeze the resulting final candidate SHA and rerun the full evidence ledger, including the complete race suite (`CUR-008`, second half).
9. Only then tag the next alpha.