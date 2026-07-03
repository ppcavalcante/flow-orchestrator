package workflow

// Phase 22 (M7) — Layer 1 of DEC-M7-verify: the engine-invariant property suite.
//
// These properties run over RANDOM acyclic DAGs and assert the executor's core
// safety contract holds for ALL of them, not just hand-picked cases. The suite is
// part of the real test package and runs under `go test ./... -race` in CI.
//
// Random-DAG generator (the foundation every property shares): a dagSpec is built
// from a random topological permutation — node i may depend only on nodes at
// positions < i (lower-triangular adjacency), so every generated graph is ACYCLIC
// BY CONSTRUCTION (no rejection sampling). Edge inclusion is gated by a swept
// probability p: p→0 yields all-roots / a single level, p→1 yields a long chain.
// gopter shrinks the primitive inputs (n, seed, edge-permille); smaller inputs map
// deterministically to smaller DAGs (fewer nodes, fewer edges), so a failing case
// shrinks toward a minimal reproducer.
//
// Each property documents the discriminating MUTATION that turns it RED — the
// reason it actually bites rather than passing vacuously.

import (
	"context"
	"fmt"
	"math/rand"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
)

// dagSpec is a generated random DAG description. deps[i] holds the indices (all
// < i) that node i depends on. continueOnError[i] / fails[i] are per-node flags
// used by the failure-safety property; the structural properties ignore them.
type dagSpec struct {
	n               int
	deps            [][]int
	continueOnError []bool
	fails           []bool
}

// invNodeName is the canonical node name for index i. (Distinct from any existing
// helper in the package — itoa/contains/min already exist in other _test.go files.)
func invNodeName(i int) string { return fmt.Sprintf("n%d", i) }

// buildDAGSpec deterministically constructs an acyclic dagSpec from primitive
// random inputs. edgePermille is the per-eligible-pair edge probability in
// 0..1000; failSeed/coeSeed drive the per-node failure / continue-on-error flags.
// Determinism makes a shrunk (n, seed, edgePermille) reproduce an identical, and
// strictly smaller, structure.
func buildDAGSpec(n int, seed int64, edgePermille int, coeSeed, failSeed int64) dagSpec {
	if n < 1 {
		n = 1
	}
	if edgePermille < 0 {
		edgePermille = 0
	}
	if edgePermille > 1000 {
		edgePermille = 1000
	}
	rng := rand.New(rand.NewSource(seed))
	coeRng := rand.New(rand.NewSource(coeSeed))
	failRng := rand.New(rand.NewSource(failSeed))

	spec := dagSpec{
		n:               n,
		deps:            make([][]int, n),
		continueOnError: make([]bool, n),
		fails:           make([]bool, n),
	}
	for i := 0; i < n; i++ {
		spec.deps[i] = make([]int, 0, i)
		// Lower-triangular: i may depend only on j < i -> acyclic by construction.
		for j := 0; j < i; j++ {
			if rng.Intn(1000) < edgePermille {
				spec.deps[i] = append(spec.deps[i], j)
			}
		}
		spec.continueOnError[i] = coeRng.Intn(2) == 0
		spec.fails[i] = failRng.Intn(2) == 0
	}
	return spec
}

// buildDAG materializes a dagSpec into a real DAG. When withActions is true each
// node gets the supplied action factory (index -> action); otherwise nodes get a
// trivial success action. applyFlags wires WithContinueOnError per the spec.
func (s dagSpec) buildDAG(
	t *testing.T,
	applyFlags bool,
	actionFor func(i int) func(context.Context, *WorkflowData) error,
) (*DAG, bool) {
	t.Helper()
	b := NewWorkflowBuilder().WithWorkflowID("inv")
	for i := 0; i < s.n; i++ {
		var nb *NodeBuilder
		if len(s.deps[i]) == 0 {
			nb = b.AddStartNode(invNodeName(i))
		} else {
			nb = b.AddNode(invNodeName(i))
		}
		if actionFor != nil {
			nb.WithAction(actionFor(i))
		} else {
			nb.WithAction(func(_ context.Context, _ *WorkflowData) error { return nil })
		}
		if len(s.deps[i]) > 0 {
			depNames := make([]string, 0, len(s.deps[i]))
			for _, j := range s.deps[i] {
				depNames = append(depNames, invNodeName(j))
			}
			nb.DependsOn(depNames...)
		}
		if applyFlags && s.continueOnError[i] {
			nb.WithContinueOnError()
		}
	}
	dag, err := b.Build()
	if err != nil {
		// A correctly-generated acyclic spec must always Build; a Build error is a
		// generator bug, surfaced by the generator self-check property, not a SUT
		// failure to be silently skipped here.
		return nil, false
	}
	return dag, true
}

// dagSpecGens are the shared primitive generators every property draws from.
// n in [1,12] keeps shrink fast; edgePermille swept 0..1000 hits both extremes.
func dagSpecGens() []gopter.Gen {
	return []gopter.Gen{
		gen.IntRange(1, 12),   // n
		gen.Int64(),           // structural seed (edges)
		gen.IntRange(0, 1000), // edgePermille (p, swept wide)
		gen.Int64(),           // continue-on-error seed
		gen.Int64(),           // failure seed
	}
}

// reachableFrom returns the set of node indices reachable downstream from `start`
// following deps in the dependent direction (who depends on start, transitively).
func (s dagSpec) downstreamOf(start int) map[int]bool {
	// Build forward adjacency: dependents[j] = nodes that depend on j.
	dependents := make([][]int, s.n)
	for i := 0; i < s.n; i++ {
		for _, j := range s.deps[i] {
			dependents[j] = append(dependents[j], i)
		}
	}
	seen := make(map[int]bool)
	stack := []int{start}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, d := range dependents[cur] {
			if !seen[d] {
				seen[d] = true
				stack = append(stack, d)
			}
		}
	}
	return seen
}

// TestEngineInvariants is the Phase 22 Layer-1 property suite. Each property holds
// over every generated random acyclic DAG; the comment on each names the mutation
// it discriminates.
func TestEngineInvariants(t *testing.T) {
	params := gopter.DefaultTestParameters()
	params.MinSuccessfulTests = 300
	params.MaxShrinkCount = 100
	properties := gopter.NewProperties(params)

	// --- Generator self-check -------------------------------------------------
	// Every graph the generator calls "acyclic" must pass Validate(). If this ever
	// fails, the generator is buggy (emitting a cycle), not the SUT — this keeps
	// the other properties' acyclic precondition honest.
	// RED if: the lower-triangular construction is broken (e.g. an edge i->j with
	// j >= i sneaks in).
	properties.Property("generator emits only valid acyclic DAGs", prop.ForAll(
		func(n int, seed int64, edgePermille int, coeSeed, failSeed int64) bool {
			s := buildDAGSpec(n, seed, edgePermille, coeSeed, failSeed)
			dag, ok := s.buildDAG(t, false, nil)
			if !ok {
				return false // Build failed on an acyclic spec => generator bug.
			}
			return dag.Validate() == nil
		},
		dagSpecGens()...,
	))

	// --- 1. Topological correctness of GetLevels ------------------------------
	// For every edge dep->node, level(dep) < level(node).
	// RED if: GetLevels ever emits a node at the same or an earlier level than one
	// of its dependencies (e.g. the level = max(parent)+1 math regresses).
	properties.Property("GetLevels: every node strictly after all its deps", prop.ForAll(
		func(n int, seed int64, edgePermille int, coeSeed, failSeed int64) bool {
			s := buildDAGSpec(n, seed, edgePermille, coeSeed, failSeed)
			dag, ok := s.buildDAG(t, false, nil)
			if !ok {
				return false
			}
			levels := dag.GetLevels()
			levelOf := make(map[string]int)
			total := 0
			for li, lvl := range levels {
				for _, node := range lvl {
					levelOf[node.Name] = li
					total++
				}
			}
			// Completeness: every node must appear in exactly one level (a cycle node
			// would be silently dropped by Kahn — but the spec is acyclic, so all n
			// must be present; a missing node is itself a failure).
			if total != s.n {
				return false
			}
			for i := 0; i < s.n; i++ {
				for _, j := range s.deps[i] {
					if levelOf[invNodeName(j)] >= levelOf[invNodeName(i)] {
						return false
					}
				}
			}
			return true
		},
		dagSpecGens()...,
	))

	// --- 2. Peak concurrency <= MaxConcurrency --------------------------------
	// A shared atomic counts in-flight actions (incremented at action entry, which
	// is AFTER the executor's semaphore acquire); the running max must never exceed
	// the configured MaxConcurrency.
	// RED if: the per-level semaphore is replaced by an unbounded per-node launch,
	// or MaxConcurrency stops being honored.
	properties.Property("peak in-flight <= MaxConcurrency", prop.ForAll(
		func(n int, seed int64, edgePermille int, maxConc int, coeSeed int64) bool {
			s := buildDAGSpec(n, seed, edgePermille, coeSeed, 0)
			var inFlight int32
			var peak int32
			actionFor := func(_ int) func(context.Context, *WorkflowData) error {
				return func(_ context.Context, _ *WorkflowData) error {
					cur := atomic.AddInt32(&inFlight, 1)
					// Track running max with a CAS loop.
					for {
						old := atomic.LoadInt32(&peak)
						if cur <= old || atomic.CompareAndSwapInt32(&peak, old, cur) {
							break
						}
					}
					// Hold the slot briefly so every sibling the executor is willing to
					// run concurrently is genuinely in-flight at the same instant — this
					// is what makes the property able to OBSERVE a breach (without it the
					// action returns before siblings start and peak never climbs, a
					// vacuous pass). Verified to go RED under an unbounded-semaphore mutation.
					time.Sleep(2 * time.Millisecond)
					atomic.AddInt32(&inFlight, -1)
					return nil
				}
			}
			dag, ok := s.buildDAG(t, false, actionFor)
			if !ok {
				return false
			}
			dag.config.MaxConcurrency = maxConc // exercise the configured limit directly
			limit := maxConc
			if limit <= 0 {
				limit = DefaultMaxConcurrency // executor coerces non-positive to default
			}
			if err := dag.Execute(context.Background(), NewWorkflowData("inv")); err != nil {
				return false
			}
			return int(atomic.LoadInt32(&peak)) <= limit
		},
		gen.IntRange(1, 12), gen.Int64(), gen.IntRange(0, 1000), gen.IntRange(1, 8), gen.Int64(),
	))

	// --- 3. Run-once completeness (all-success) -------------------------------
	// On an all-success run, every node's action runs exactly once.
	// RED if: a node is launched twice (e.g. duplicate level membership) or skipped.
	properties.Property("all-success: every action runs exactly once", prop.ForAll(
		func(n int, seed int64, edgePermille int, coeSeed, failSeed int64) bool {
			s := buildDAGSpec(n, seed, edgePermille, coeSeed, failSeed)
			counts := make([]int32, s.n)
			actionFor := func(i int) func(context.Context, *WorkflowData) error {
				return func(_ context.Context, _ *WorkflowData) error {
					atomic.AddInt32(&counts[i], 1)
					return nil
				}
			}
			dag, ok := s.buildDAG(t, false, actionFor)
			if !ok {
				return false
			}
			if err := dag.Execute(context.Background(), NewWorkflowData("inv")); err != nil {
				return false
			}
			for i := 0; i < s.n; i++ {
				if atomic.LoadInt32(&counts[i]) != 1 {
					return false
				}
			}
			return true
		},
		dagSpecGens()...,
	))

	// --- 4. Deps-before-execution ---------------------------------------------
	// When a node's action runs, ALL its deps are already Completed (observed from
	// inside the action via GetNodeStatus).
	// RED if: the executor launches a node before a dependency has reached Completed
	// (e.g. the dep-completeness guard is dropped).
	properties.Property("a node sees all deps Completed when it runs", prop.ForAll(
		func(n int, seed int64, edgePermille int, coeSeed, failSeed int64) bool {
			s := buildDAGSpec(n, seed, edgePermille, coeSeed, failSeed)
			var violated atomic.Bool
			actionFor := func(i int) func(context.Context, *WorkflowData) error {
				deps := s.deps[i]
				return func(_ context.Context, d *WorkflowData) error {
					for _, j := range deps {
						st, ok := d.GetNodeStatus(invNodeName(j))
						if !ok || st != Completed {
							violated.Store(true)
						}
					}
					return nil
				}
			}
			dag, ok := s.buildDAG(t, false, actionFor)
			if !ok {
				return false
			}
			if err := dag.Execute(context.Background(), NewWorkflowData("inv")); err != nil {
				return false
			}
			return !violated.Load()
		},
		dagSpecGens()...,
	))

	// --- 5. Failure-safety, two-armed (DEC-P21-depguard) ----------------------
	// Annotate a random subset continue-on-error and fail a random subset. Then:
	//   * a continue-on-error node that fails UNBLOCKS its dependents (they run);
	//   * Execute returns nil IFF every node that is NOT continue-on-error succeeded;
	//   * when a HARD (non-coe) node fails, no node strictly downstream of it runs
	//     (fail-fast halts; verified by checking that downstream actions did not run).
	// RED if: the guard broadens to Completed||Failed (a hard-failed dep would
	// wrongly unblock its dependent) or narrows to Completed-only (a coe-failed dep
	// would wrongly block its dependent) — exactly the G21 discriminating pair, now
	// quantified over random DAGs.
	properties.Property("two-armed failure-safety partition", prop.ForAll(
		func(n int, seed int64, edgePermille int, coeSeed, failSeed int64) bool {
			s := buildDAGSpec(n, seed, edgePermille, coeSeed, failSeed)
			ran := make([]int32, s.n)
			actionFor := func(i int) func(context.Context, *WorkflowData) error {
				shouldFail := s.fails[i]
				return func(_ context.Context, _ *WorkflowData) error {
					atomic.AddInt32(&ran[i], 1)
					if shouldFail {
						return fmt.Errorf("node %d intentional failure", i)
					}
					return nil
				}
			}
			dag, ok := s.buildDAG(t, true, actionFor) // applyFlags = true: wire coe
			if !ok {
				return false
			}
			execErr := dag.Execute(context.Background(), NewWorkflowData("inv"))

			// Identify the FIRST hard failure in topological order (lowest index that
			// fails and is not continue-on-error). Everything strictly downstream of
			// it must NOT have run (fail-fast). Nodes before/independent of it are
			// unconstrained here (a later level may simply never start).
			firstHardFail := -1
			for i := 0; i < s.n; i++ {
				if s.fails[i] && !s.continueOnError[i] {
					firstHardFail = i
					break
				}
			}

			// Arm B / Execute contract: nil IFF no hard node FAILED. We can only
			// assert the forward direction soundly per-run: if there is NO hard
			// failure anywhere, Execute must be nil; if there IS at least one hard
			// failure whose level is actually reached, Execute must be non-nil.
			anyHardFail := firstHardFail >= 0
			if !anyHardFail {
				// No hard failure possible => every non-coe node succeeds => nil.
				if execErr != nil {
					return false
				}
			} else {
				// At least one hard failure exists AND (being lowest topo index) its
				// level is reachable from roots without an earlier hard stop, so it
				// WILL run and trip fail-fast => Execute must be non-nil.
				if execErr != nil {
					// expected; now assert no strict-downstream of it ran.
					down := s.downstreamOf(firstHardFail)
					for d := range down {
						if atomic.LoadInt32(&ran[d]) != 0 {
							return false
						}
					}
				} else {
					return false // a reached hard failure must fail the workflow
				}
			}

			// Arm A (coe unblocks): for every coe node that FAILED and ran, each of
			// its direct dependents that is NOT downstream of an earlier hard stop
			// must have run (it was unblocked by the coe-Failed arm). We check the
			// clean case: when there is NO hard failure at all, every dependent of a
			// failed-coe node must have run.
			if !anyHardFail {
				for i := 0; i < s.n; i++ {
					if !s.fails[i] || !s.continueOnError[i] {
						continue
					}
					// direct dependents of i:
					for k := 0; k < s.n; k++ {
						for _, j := range s.deps[k] {
							if j == i {
								if atomic.LoadInt32(&ran[k]) == 0 {
									return false // coe-failed dep did not unblock dependent
								}
							}
						}
					}
				}
			}
			return true
		},
		dagSpecGens()...,
	))

	// --- 5b. Normal-Failed dep BLOCKS (the broaden direction of DEC-P21-depguard).
	// The Execute-level property above cannot observe a broadened guard: a hard
	// failure halts the workflow before the wrongly-unblocked (later-level) dependent
	// is ever reached, so Completed||Failed vs the correct guard are indistinguishable
	// at Execute granularity. This property drives executeNodesInLevel DIRECTLY with a
	// hand-built level whose single node depends on a node pre-marked Failed (NOT
	// continue-on-error). The dependent must NOT run and the level must report the
	// unmet-dependency error.
	// RED if: the guard broadens to Completed||Failed (a normal Failed dep would wrongly
	// resolve and the dependent would run). Confirmed to falsify under that mutation.
	properties.Property("normal Failed dep blocks its dependent (direct executor)", prop.ForAll(
		func(depFailed bool) bool {
			data := NewWorkflowData("inv5b")
			dep := NewNode("dep", ActionFunc(func(_ context.Context, _ *WorkflowData) error { return nil }))
			var ran int32
			child := NewNode("child", ActionFunc(func(_ context.Context, _ *WorkflowData) error {
				atomic.AddInt32(&ran, 1)
				return nil
			}))
			child.AddDependency(dep)
			// Pre-set the dep's status as if a prior level had run it. depFailed toggles
			// Failed vs Completed; dep is a NORMAL node (ContinueOnError stays false).
			if depFailed {
				data.SetNodeStatus("dep", Failed)
			} else {
				data.SetNodeStatus("dep", Completed)
			}
			failures, _ := executeNodesInLevel(context.Background(), []*Node{child}, data, DefaultMaxConcurrency, resolveTracer(nil))
			if depFailed {
				// normal Failed dep -> child must be blocked: it did not run, it is
				// marked Skipped (DEC-CHUNK3-status, S1), and Skipped is NOT a
				// failure so the level reports no NodeError for it.
				childStatus, _ := data.GetNodeStatus("child")
				return atomic.LoadInt32(&ran) == 0 && childStatus == Skipped && len(failures) == 0
			}
			// Completed dep -> child runs, no failures.
			return atomic.LoadInt32(&ran) == 1 && len(failures) == 0
		},
		gen.Bool(),
	))

	// --- 5c. Skipped-status soundness (DEC-CHUNK3-status, S1) ------------------
	// Over a random DAG with random coe/fail flags, after Execute every node's
	// final status is well-formed:
	//   (a) totality: every node is present in the status map (no absent node);
	//   (b) a node that RAN ends Completed or Failed (never Pending/Skipped);
	//   (c) Skipped soundness: a Skipped node did NOT run AND has >=1 dep that is
	//       terminal non-resolving (a non-coe Failed dep, or a Skipped dep);
	//   (d) Skipped completeness: a node that did NOT run and HAS a terminal
	//       non-resolving dep is NOT left Pending — it must be Skipped;
	//   (e) a node whose deps all RESOLVED is never Skipped.
	// RED if: the skip sweep is dropped (blocked nodes wrongly stay Pending — (d)),
	// or skip is broadened to independent unreached nodes (a node with no
	// non-resolving dep wrongly becomes Skipped — (e)), or coe resolution drifts
	// (a coe-Failed dep wrongly causes a skip — (c)/(e)).
	properties.Property("Skipped status is sound and complete (S1)", prop.ForAll(
		func(n int, seed int64, edgePermille int, coeSeed, failSeed int64) bool {
			s := buildDAGSpec(n, seed, edgePermille, coeSeed, failSeed)
			ran := make([]int32, s.n)
			actionFor := func(i int) func(context.Context, *WorkflowData) error {
				shouldFail := s.fails[i]
				return func(_ context.Context, _ *WorkflowData) error {
					atomic.AddInt32(&ran[i], 1)
					if shouldFail {
						return fmt.Errorf("node %d intentional failure", i)
					}
					return nil
				}
			}
			dag, ok := s.buildDAG(t, true, actionFor) // applyFlags: wire coe
			if !ok {
				return false
			}
			data := NewWorkflowData("inv")
			execErr := dag.Execute(context.Background(), data)
			// Cross-check with the chunk-2 contract: Execute errors iff a hard
			// (non-coe) node actually failed. This both consumes execErr and ties
			// the status accounting to the error-return contract.
			anyHardFail := false
			for i := 0; i < s.n; i++ {
				if s.fails[i] && !s.continueOnError[i] {
					anyHardFail = true
					break
				}
			}
			if anyHardFail && execErr == nil {
				return false
			}
			if !anyHardFail && execErr != nil {
				return false
			}

			status := make([]NodeStatus, s.n)
			for i := 0; i < s.n; i++ {
				st, present := data.GetNodeStatus(invNodeName(i))
				if !present {
					return false // (a) totality
				}
				status[i] = st
			}

			// depIsSkipCause mirrors the production rule: a dep is a terminal
			// non-resolving skip cause iff it is Skipped, or Failed AND not
			// continue-on-error (a coe-Failed dep RESOLVES, never a skip cause).
			depIsSkipCause := func(j int) bool {
				if status[j] == Skipped {
					return true
				}
				return status[j] == Failed && !s.continueOnError[j]
			}

			for i := 0; i < s.n; i++ {
				didRun := atomic.LoadInt32(&ran[i]) > 0
				switch status[i] {
				case Completed, Failed:
					if !didRun {
						return false // (b) a terminal run-state implies it ran
					}
				case Skipped:
					if didRun {
						return false // (c) skipped nodes did not run
					}
					hasSkipCause := false
					for _, j := range s.deps[i] {
						if depIsSkipCause(j) {
							hasSkipCause = true
							break
						}
					}
					if !hasSkipCause {
						return false // (c) skipped requires a non-resolving terminal dep
					}
				case Pending:
					if didRun {
						return false // a node that ran is never Pending
					}
					// (d) completeness: a not-run node with a skip-cause dep must
					// NOT be left Pending.
					for _, j := range s.deps[i] {
						if depIsSkipCause(j) {
							return false
						}
					}
				default:
					return false // Running must be transient; no other state at rest
				}
			}
			return true
		},
		dagSpecGens()...,
	))

	// --- 6. Cycle-rejection THROUGH Execute -----------------------------------
	// Take a generated acyclic DAG with >=2 nodes, inject ONE back-edge (high topo
	// index -> low), and assert Execute returns a "cycle detected" error AND zero
	// nodes ran.
	// RED if: Validate ever runs AFTER GetLevels — the cyclic nodes would be
	// silently Kahn-dropped and the acyclic remainder would execute (some action
	// would run), instead of a clean cycle rejection.
	properties.Property("a cyclic DAG is rejected by Execute and runs nothing", prop.ForAll(
		func(n int, seed int64, edgePermille int) bool {
			if n < 2 {
				return true // need at least 2 nodes to form a back-edge
			}
			s := buildDAGSpec(n, seed, edgePermille, 0, 0)
			var ranCount int32
			actionFor := func(_ int) func(context.Context, *WorkflowData) error {
				return func(_ context.Context, _ *WorkflowData) error {
					atomic.AddInt32(&ranCount, 1)
					return nil
				}
			}
			// Build a CLEAN acyclic DAG first (the builder's own Build-time Validate
			// would reject a cycle, so we must not inject before Build). Then inject a
			// 2-cycle DIRECTLY onto the built nodes' DependsOn so that Execute's OWN
			// Validate() call (dag.go) is the first and only guard that sees the cycle.
			// This is what makes the property test cycle-rejection THROUGH Execute, not
			// merely through Build.
			dag, built := s.buildDAG(t, false, actionFor)
			if !built {
				return false // a clean acyclic spec must Build
			}
			n0 := dag.Nodes[invNodeName(0)]
			nLast := dag.Nodes[invNodeName(s.n-1)]
			if n0 == nil || nLast == nil {
				return false
			}
			n0.DependsOn = append(n0.DependsOn, nLast)
			nLast.DependsOn = append(nLast.DependsOn, n0)

			err := dag.Execute(context.Background(), NewWorkflowData("inv"))
			if err == nil {
				return false // a cycle MUST be rejected by Execute
			}
			if !contains(err.Error(), "cycle") {
				return false // must be the cycle-detection error specifically
			}
			return atomic.LoadInt32(&ranCount) == 0 // and zero nodes ran
		},
		gen.IntRange(2, 12), gen.Int64(), gen.IntRange(0, 1000),
	))

	// --- 7. Persistence round-trip (incl. typed keys) -------------------------
	// A WorkflowData populated via the NEW typed-key API (Phase 20) round-trips
	// through the FlatBuffers store: Save then Load yields the same typed values.
	// This EXTENDS the existing fidelity tests (which cover string-keyed int64
	// edges) into the typed-key dimension.
	// RED if: the typed bridge or the store drops/garbles a value on Save/Load.
	properties.Property("typed-key WorkflowData survives FB Save->Load", prop.ForAll(
		func(vals []int64) bool {
			store, err := NewFlatBuffersStore(t.TempDir())
			if err != nil {
				return false
			}
			d := NewWorkflowData("rt")
			keys := make([]Key[int64], len(vals))
			for i, v := range vals {
				keys[i] = NewKey[int64](fmt.Sprintf("k%d", i))
				Set(d, keys[i], v)
			}
			if err := store.Save(d); err != nil {
				return false
			}
			got, err := store.Load("rt")
			if err != nil {
				return false
			}
			for i, v := range vals {
				out, ok := Get(got, keys[i])
				if !ok || out != v {
					return false
				}
			}
			return true
		},
		gen.SliceOf(gen.Int64()),
	))

	properties.TestingRun(t)
}
