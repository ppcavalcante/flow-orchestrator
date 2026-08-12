package workflow

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// A2 -- deciding J2's POPULATION mechanically, before any clause is written.
// ---------------------------------------------------------------------------
//
// THE CONCERN J2 NAMES IS REAL AND MECHANICAL. depResolved returns true for a Bypassed
// dependency when the dependent's role is mergeDependent, and the launch gate counts taken
// tails over mergeAction.tails ONLY -- so a merge's NON-tail dependencies (an explicit
// MergeBuilder.DependsOn, and the structural DEC-M11-DEPMODEL choice edge) are SATISFIED by
// Bypassed while contributing nothing to the fire decision. Resolution is not occurrence.
//
// WHAT IS NOT ESTABLISHED is that such a graph exists WHILE DOMINANCE HOLDS. Reading the
// static rules against each other suggests the two may be in tension: a merge depends on
// its source ChoiceNode via the DEPMODEL edge, so V dominating the merge forces V to
// dominate the choice, and a Bypassed V cascades to the choice and hence to every tail,
// leaving zero taken tails and a merge that is itself Bypassed.
//
// 🔴 THAT IS REASONING, AND THIS MILESTONE HAS BEEN REPEATEDLY WRONG EXACTLY THERE. So the
// paragraph above decides nothing. The search below decides it, and it is CALIBRATED
// AGAINST A SEEDED POSITIVE FIRST: without that, "no witness" and "no instrument" produce
// the same silence, and the silence is in the flattering direction.

// vb07J2Bound is the search's bound, stated as data so the report cannot drift from what
// ran. A BOUNDED SEARCH CANNOT PROVE GLOBAL EMPTINESS and no artifact may read as if it
// did; what it can do is distinguish "looked and found nothing" from "never looked".
type vb07J2Bound struct {
	MaxNodes            int
	MergeNodes          int
	ChoiceNodes         int
	BranchesPerChoice   int
	OptionalBranchBody  bool
	TailsModes          int
	NonTailMergeDeps    bool
	SeparateSink        bool
	BranchOutcomes      int
	ContinueOnErrorUsed bool
}

func (b vb07J2Bound) String() string {
	return fmt.Sprintf(
		"nodes<=%d, merges<=%d, choices=%d, branches/choice=%d, optional branch body=%t, "+
			"merge tail-set modes=%d (both/only-branch-1/only-branch-2), "+
			"non-tail merge DependsOn generated=%t, separate sink=%t, branch outcomes enumerated=%d, "+
			"ContinueOnError used=%t",
		b.MaxNodes, b.MergeNodes, b.ChoiceNodes, b.BranchesPerChoice, b.OptionalBranchBody, b.TailsModes,
		b.NonTailMergeDeps, b.SeparateSink, b.BranchOutcomes, b.ContinueOnErrorUsed)
}

// vb07Recorder records which node ACTIONS actually ran. Invocation, not status: a status
// can be written by anything holding the shared *WorkflowData, and the whole point of this
// search is to tell "the verifier ran" apart from "the verifier looks done".
type vb07Recorder struct {
	mu      sync.Mutex
	invoked map[string]int
}

func newVB07Recorder() *vb07Recorder { return &vb07Recorder{invoked: map[string]int{}} }

func (r *vb07Recorder) action(name string) ActionFunc {
	return ActionFunc(func(_ context.Context, _ *WorkflowData) error {
		r.mu.Lock()
		r.invoked[name]++
		r.mu.Unlock()
		return nil
	})
}

func (r *vb07Recorder) count(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.invoked[name]
}

// vb07Shape is one generated graph shape. The builder is rebuilt from it for every triple,
// because Build() MUTATES (validateReconvergence appends DEPMODEL edges and the action
// clause snapshots actions), so a shape may never be built twice.
type vb07Shape struct {
	body1, body2 bool
	// tailsMode selects WHICH branch tails the merge records as From tails: both, or
	// only one. 🔴 THIS DIMENSION WAS MISSING FROM THE FIRST GENERATOR AND ITS ABSENCE
	// MADE THE SWEEP VACUOUS FOR ITS OWN QUESTION. With both branches always recorded as
	// tails, every extra DependsOn named a node that was ALREADY a tail, so the generator
	// could not express "a dependency whose Bypassed resolves the merge without
	// contributing a taken tail" -- which is the entire mechanism J2 is about. The
	// calibration positive is what exposed it: no generated shape could reproduce the
	// graph the calibration had to be hand-built as.
	tailsMode string // "both" | "only1" | "only2"
	extraDep  string // "" for none: a NON-TAIL MergeBuilder.DependsOn

	// 🔴 THE SECOND MERGE IS WHERE THE STRUCTURAL ARGUMENT IS WEAKEST, WHICH IS THE ONLY
	// REASON IT IS GENERATED. The subsumption claim rests on one step -- V dom M forces
	// V dom C, so a Bypassed V cascades to the tails and the merge is itself Bypassed.
	// With ONE merge that step is forced and the sweep cannot test it. With TWO there are
	// two M's, and V could dominate M1 without dominating M2 -- where M2 is the one
	// carrying the Bypassed-reachable non-tail dependency. A one-merge bound cannot see
	// that shape, and it is the shape a reader will ask about first.
	//
	// 🔴 BOTH TOPOLOGIES ARE GENERATED, AND THE FIRST DRAFT GENERATED ONLY ONE ON A FALSE
	// PREMISE (119-F3, found by review). That draft chained the pair SERIALLY and justified
	// it by claiming the engine rejects a parallel pair -- that a non-merge sink depending on
	// both would reconverge two branch entries of one ChoiceNode. THAT CLAIM IS FALSE: the
	// parallel topology BUILDS -- 82 of the 162 generated parallel shapes do.
	//
	// Worse, the serial chain DESTROYS the very configuration the widening was commissioned
	// to reach. With m2 depending on m, every root->m2 path passes m, so dominators(m2)
	// includes dominators(m) and V dom m implies V dom m2 for EVERY V. "V dominates m1 and
	// not m2" cannot occur in a serial pair at all -- so the serial family, on its own, was a
	// wider bound that could not see the shape it was widened for.
	//
	// THE REAL REASON THE ASYMMETRIC CASE COLLAPSES is different and is worth more than the
	// false one it replaces: validateReconvergence APPENDS the merge<-choice DEPMODEL edge,
	// so the ChoiceNode dominates every merge and no branch-side node dominates any merge.
	// That is why no non-merge verifier can dominate one merge and not the other, and it was
	// written down nowhere until review derived it.
	chain string // "serial" (m2 depends on m1) | "parallel" (both feed the sink)

	secondMerge bool
	tailsMode2  string // as tailsMode, for m2
	extraDep2   string // a NON-TAIL DependsOn on m2, beyond any chain edge

	separateSink bool
	takeBranch1  bool
}

// mergeName is the last merge in the chain -- what a separate sink hangs off.
func (s vb07Shape) lastMerge() string {
	if s.secondMerge {
		return "m2"
	}
	return "m"
}

// tails2 is m2's recorded From tail set.
func (s vb07Shape) tails2() []string {
	switch s.tailsMode2 {
	case "only1":
		return []string{s.tail1()}
	case "only2":
		return []string{s.tail2()}
	default:
		return []string{s.tail1(), s.tail2()}
	}
}

// tails returns the From tail set the merge records, per tailsMode.
func (s vb07Shape) tails() []string {
	switch s.tailsMode {
	case "only1":
		return []string{s.tail1()}
	case "only2":
		return []string{s.tail2()}
	default:
		return []string{s.tail1(), s.tail2()}
	}
}

func (s vb07Shape) tail1() string {
	if s.body1 {
		return "b1t"
	}
	return "b1"
}

func (s vb07Shape) tail2() string {
	if s.body2 {
		return "b2t"
	}
	return "b2"
}

func (s vb07Shape) nodes() []string {
	out := []string{"r", "c", "b1", "b2", "m"}
	if s.body1 {
		out = append(out, "b1t")
	}
	if s.body2 {
		out = append(out, "b2t")
	}
	if s.secondMerge {
		out = append(out, "m2")
	}
	if s.separateSink {
		out = append(out, "s")
	}
	sort.Strings(out)
	return out
}

func (s vb07Shape) String() string {
	if s.secondMerge {
		return fmt.Sprintf("body1=%t body2=%t tails=%s extraDep=%q secondMerge(%s) tails2=%s "+
			"extraDep2=%q separateSink=%t takeBranch1=%t",
			s.body1, s.body2, s.tailsMode, s.extraDep, s.chain, s.tailsMode2, s.extraDep2,
			s.separateSink, s.takeBranch1)
	}
	return fmt.Sprintf("body1=%t body2=%t tails=%s extraDep=%q separateSink=%t takeBranch1=%t",
		s.body1, s.body2, s.tailsMode, s.extraDep, s.separateSink, s.takeBranch1)
}

// vb07Build constructs the shape through the PUBLIC builder, optionally carrying one
// boundary declaration. decl==nil builds the bare graph.
func vb07Build(s vb07Shape, rec *vb07Recorder, decl *boundaryDecl) (*DAG, error) {
	b := NewWorkflowBuilder().WithWorkflowID("vb07-j2")
	b.AddStartNode("r").WithAction(rec.action("r"))
	b.AddChoice("c").DependsOn("r").
		When(func(d *WorkflowData) bool { _, ok := d.Get("takeBranch1"); return ok }, "b1").
		Otherwise("b2")
	b.AddNode("b1").WithAction(rec.action("b1"))
	b.AddNode("b2").WithAction(rec.action("b2"))
	if s.body1 {
		b.AddNode("b1t").DependsOn("b1").WithAction(rec.action("b1t"))
	}
	if s.body2 {
		b.AddNode("b2t").DependsOn("b2").WithAction(rec.action("b2t"))
	}
	m := b.AddMerge("m").From(s.tails()...).WithAction(rec.action("m"))
	if s.extraDep != "" {
		m.DependsOn(s.extraDep)
	}
	if s.secondMerge {
		m2 := b.AddMerge("m2").From(s.tails2()...).WithAction(rec.action("m2"))
		if s.chain == "serial" {
			m2.DependsOn("m")
		}
		if s.extraDep2 != "" {
			m2.DependsOn(s.extraDep2)
		}
	}
	if s.separateSink {
		sink := b.AddNode("s").WithAction(rec.action("s"))
		if s.secondMerge && s.chain == "parallel" {
			// The sink joins BOTH merges. This is the topology the first draft asserted the
			// engine rejects; it builds.
			sink.DependsOn("m", "m2")
		} else {
			sink.DependsOn(s.lastMerge())
		}
	}
	if decl != nil {
		b.WithBoundary(decl.doer, decl.verifier, decl.sink)
	}
	return b.Build()
}

// vb07Flag is the search's verdict predicate, applied identically to the generated corpus
// and to the seeded calibration positive: the SINK's action ran, the VERIFIER's did not,
// and the verifier is Bypassed.
func vb07Flag(rec *vb07Recorder, data *WorkflowData, verifier, sink string) bool {
	if rec.count(sink) == 0 || rec.count(verifier) != 0 {
		return false
	}
	st, ok := data.GetNodeStatus(verifier)
	return ok && st == Bypassed
}

// TestVB07_J2_PopulationSearch decides J2's population by bounded exhaustive search.
//
// BOTH OUTCOMES ARE VALID RESULTS. A witness becomes J2's fixture; no witness at the bound
// means J2 has no demonstrated population and must land as a DIAGNOSTIC branch labelled as
// one -- there is precedent in this very file's subject, clause (a), which fires constantly
// and changes zero verdicts. What is NOT a valid result is an unreported one.
func TestVB07_J2_PopulationSearch(t *testing.T) {
	bound := vb07J2Bound{
		MaxNodes: 9, MergeNodes: 2, ChoiceNodes: 1, BranchesPerChoice: 2,
		OptionalBranchBody: true, TailsModes: 3, NonTailMergeDeps: true, SeparateSink: true,
		BranchOutcomes: 2, ContinueOnErrorUsed: false,
	}
	t.Logf("A2 search BOUND: %s", bound)

	// ---------------------------------------------------------------------
	// CALIBRATION FIRST. Before a no-witness result may be believed, the search's own
	// verdict predicate must be shown to fire on a graph it MUST flag. Without this,
	// a broken instrument and an empty population are indistinguishable, and the
	// indistinguishable reading is the flattering one.
	//
	// The seeded positive is F-M23-CONTROL-FLOW-01's shape: an action forges the
	// verifier's status so the executor skips it. It forges BYPASSED specifically,
	// because that is the conjunct this predicate tests -- a forged Completed would
	// leave the verifier's action equally un-run but the predicate would correctly
	// decline to flag it, and that is a DIFFERENT channel, already recorded and out of
	// A2's scope.
	// ---------------------------------------------------------------------
	t.Run("calibration: the predicate FIRES on a seeded positive", func(t *testing.T) {
		rec := newVB07Recorder()
		b := NewWorkflowBuilder().WithWorkflowID("vb07-calib")
		b.AddStartNode("r").WithAction(rec.action("r"))
		b.AddChoice("c").DependsOn("r").
			When(func(*WorkflowData) bool { return true }, "b1").
			Otherwise("V")
		b.AddNode("b1").WithAction(rec.action("b1"))
		b.AddNode("V").WithAction(rec.action("V"))
		// S is a MERGE. Its taken tail is b1; V is a NON-TAIL dependency, which is the
		// whole mechanism: depResolved reports a Bypassed dependency as resolved for a
		// mergeDependent, and the launch gate counts taken tails over the recorded tail
		// set only -- so V satisfies S while contributing nothing to the fire decision.
		b.AddMerge("S").From("b1").WithAction(rec.action("S")).DependsOn("V")
		dag, err := b.Build()
		require.NoError(t, err)
		data := NewWorkflowData("vb07-calib")
		require.NoError(t, dag.Execute(context.Background(), data))

		require.Equal(t, 1, rec.count("S"), "the seeded positive must RUN the sink")
		require.Equal(t, 0, rec.count("V"), "the seeded positive must SKIP the verifier's action")
		require.True(t, vb07Flag(rec, data, "V", "S"),
			"the search's verdict predicate did NOT fire on a graph it must flag. The INSTRUMENT is "+
				"broken, and until this passes a no-witness result from the sweep below means nothing")
	})

	// 🔴 WHY THE SEEDED POSITIVE IS THIS SHAPE AND NOT A STATUS FORGERY, recorded because
	// the first attempt was the forgery and it FAILED, and the failure is informative
	// rather than a detail of the harness.
	//
	// The obvious calibration is F-M23-CONTROL-FLOW-01's shape: an action writes the
	// verifier's status so the executor skips it. Seeded as r -> V -> S with r forging
	// V = Bypassed, IT DOES NOT MAKE S RUN. depResolved treats Bypassed as resolving ONLY
	// for a mergeDependent, so an ordinary AND dependent blocked solely by a Bypassed
	// dependency is itself Bypassed -- the forgery CASCADES and the sink never runs.
	// Measured, not reasoned: rec.count("S") was 0.
	//
	// A forgery of COMPLETED does make the sink run with the verifier un-run, but it
	// leaves the verifier Completed, and this predicate's third conjunct is Bypassed. That
	// is a different channel, already recorded as F-M23-CONTROL-FLOW-01, and it is out of
	// A2's scope rather than a hole in it.
	//
	// So the only shape that reaches the predicate is the OR-join one above -- which is
	// J2's own hypothesised mechanism, seeded by hand WITHOUT any regard to dominance.
	// That makes this calibration do double duty: it proves the instrument fires, AND it
	// establishes that the runtime mechanism J2 names is real. What the sweep then decides
	// is the separate question of whether that mechanism survives a boundary HEAD accepts.

	// The CONTROL for the calibration: the same graph without the forgery must NOT be
	// flagged. A predicate that fires on everything calibrates just as convincingly.
	t.Run("calibration control: the predicate DECLINES without the forgery", func(t *testing.T) {
		rec := newVB07Recorder()
		b := NewWorkflowBuilder().WithWorkflowID("vb07-calib-ctl")
		b.AddStartNode("r").WithAction(rec.action("r"))
		b.AddNode("V").DependsOn("r").WithAction(rec.action("V"))
		b.AddNode("S").DependsOn("V").WithAction(rec.action("S"))
		dag, err := b.Build()
		require.NoError(t, err)
		data := NewWorkflowData("vb07-calib-ctl")
		require.NoError(t, dag.Execute(context.Background(), data))
		require.Equal(t, 1, rec.count("V"), "the control must RUN the verifier")
		require.False(t, vb07Flag(rec, data, "V", "S"),
			"the predicate fired without a forgery: it flags everything and proves nothing")
	})

	// ---------------------------------------------------------------------
	// THE SWEEP.
	// ---------------------------------------------------------------------
	shapes := vb07Shapes()
	familyShapes, familyBuilt := map[string]int{}, map[string]int{}
	for _, sh := range shapes {
		familyShapes[sh.family()]++
	}

	graphsBuilt, triplesTried, triplesAccepted, runs := 0, 0, 0, 0
	twoMergeBuilt, twoMergeAccepted, asymmetricDominance, twoMergeVerifiers := 0, 0, 0, 0
	// Keyed BY FAMILY, never by chain name. The hand-written serial/parallel pair folded any
	// new family into "serial" -- measured: adding a third chain reported serial=9220 and the
	// new family 0. That is the reviewer's family()-folding minor reappearing one level over,
	// inside the counters added to fix 119-F8 and 119-F10.
	builtByFamily, acceptedByFamily := map[string]int{}, map[string]int{}
	// 119-F9 / the cross-product: the site feeding asymmetricDominance and bypassableVerifiers
	// is a THIRD population. Keyed BY FAMILY rather than by two named chains, so a family the
	// generator gains is measured here without this line being edited.
	observedByFamily := map[string]int{}
	// flagEvaluated counts, per generated FAMILY, how many times the verdict predicate was
	// actually EVALUATED. This is the terminal population -- see the floor for why it is the
	// one that ends the ladder rather than a fourth rung on it.
	flagEvaluated := map[string]int{}
	bypassableSeen := map[string]bool{}
	// 119-F4: what vb07BypassReachable actually FLAGGED, accumulated across the sweep.
	// Without this the instrument itself is unpinned -- see the floor below.
	bypassReachableSeen := map[string]bool{}
	var witnesses []string
	// AUD-008 / C-12: the sweep must not count an ACCEPTED triple whose Execute
	// REGRESSED to an error as a clean no-witness run — the emptiness claim ranges
	// only over triples that actually executed. Collected unconditionally-guarded
	// inside the loop (no control-flow escape, so the straight-line warrant checked
	// by TestVB07_SweepLoopHasNoSkipBetweenAcceptAndVerdict is preserved) and
	// asserted after the loop.
	var execErrs []string

	for _, s := range shapes {
		// The bare shape must build at all; a shape the reconvergence validator rejects
		// contributes no triples and is counted so the report can show it.
		if _, err := vb07Build(s, newVB07Recorder(), nil); err != nil {
			continue
		}
		graphsBuilt++
		familyBuilt[s.family()]++
		builtByFamily[s.family()]++
		if s.secondMerge {
			twoMergeBuilt++
		}

		names := s.nodes()
		for _, doer := range names {
			for _, verifier := range names {
				for _, sink := range names {
					triplesTried++
					rec := newVB07Recorder()
					decl := boundaryDecl{doer: doer, verifier: verifier, sink: sink}
					dag, err := vb07Build(s, rec, &decl)
					if err != nil {
						continue // HEAD refuses this declaration; not J2's population
					}
					triplesAccepted++
					acceptedByFamily[s.family()]++
					if s.secondMerge {
						twoMergeAccepted++
						// 🔴 COUNT THE CONFIGURATION, NOT THE PRESENCE (119-F3). The
						// two-merge counters above can only say a two-merge graph was
						// built and accepted; they stay green while the shape the
						// widening was commissioned for -- V dominating one merge and
						// not the other -- never occurs. This counts that shape.
						if vb07DominatesOne(dag, decl.verifier) {
							asymmetricDominance++
						}
						twoMergeVerifiers++
						observedByFamily[s.family()]++
						br := vb07BypassReachable(dag)
						for name, reachable := range br {
							if reachable {
								bypassReachableSeen[name] = true
							}
						}
						if br[decl.verifier] {
							bypassableSeen[decl.verifier] = true
						}
					}

					data := NewWorkflowData("vb07-j2")
					if s.takeBranch1 {
						data.Set("takeBranch1", true)
					}
					execErr := dag.Execute(context.Background(), data)
					runs++
					if execErr != nil {
						execErrs = append(execErrs, fmt.Sprintf("shape[%s] boundary(%s, %s, %s): %v", s, doer, verifier, sink, execErr))
					}

					flagEvaluated[s.family()]++
					if vb07Flag(rec, data, verifier, sink) {
						witnesses = append(witnesses, fmt.Sprintf(
							"shape[%s] boundary(%s, %s, %s)", s, doer, verifier, sink))
					}
				}
			}
		}
	}

	// Anti-vacuity on the SEARCH's own verdict. A sweep that built nothing, accepted no
	// triple or executed no run reports "no witness" in exactly the same words as one that
	// looked properly, and a machine consumer cannot tell them apart from a log line.
	// 🔴 THE EQUALITY. A floor asks "did ANY get through"; this asks "did they ALL".
	//
	// The per-family floors above catch a family going to ZERO. Starve every OTHER parallel
	// triple and every one of them still passes, because a halved count is not a zero count.
	// qa named the tell without naming the fix -- runs diverging from triplesAccepted,
	// "visible in the log, asserted by nothing". This is that assertion. It is not a further
	// rung on the ladder: it is the SAME population, asserted as an IDENTITY rather than a
	// floor. Second time this phase a count stood where an identity was needed; the first was
	// require.Len blind to an in-place edit (119-F7).
	//
	// 🔴 AND IT IS AN INVARIANT, NOT A FACT ABOUT TODAY -- which is the condition this had to
	// meet before landing, because a guard that reds on CORRECT code gets deleted by the next
	// person and takes the real coverage with it. The warrant: between triplesAccepted++ and
	// flagEvaluated[...]++ the loop body is STRAIGHT-LINE. dag.Execute's error is CAPTURED (AUD-008,
	// into execErrs) but the capture is a non-escaping guarded append -- it does not continue/return/
	// break out of the loop -- so even a failing execution still counts toward runs and flagEvaluated.
	// No legitimate path accepts a triple without running and evaluating it, so the equality cannot red
	// on correct code -- it can only red
	// if someone inserts control flow between those two points, which is exactly the defect it
	// exists to catch. That straight-line property is itself asserted, structurally, by
	// TestVB07_SweepLoopHasNoSkipBetweenAcceptAndVerdict -- so the warrant re-runs instead of
	// resting on this paragraph.
	require.Equal(t, triplesAccepted, runs,
		"the sweep ACCEPTED %d triples and RAN %d. A floor catches a family starved to zero; only "+
			"this catches a family starved PARTIALLY, and a partially starved population reports a "+
			"clean verdict over a set nobody counted", triplesAccepted, runs)
	totalEvaluated := 0
	for _, n := range flagEvaluated {
		totalEvaluated += n
	}
	require.Equal(t, runs, totalEvaluated,
		"the sweep RAN %d triples and evaluated the witness predicate %d times; every run must "+
			"reach the verdict or the emptiness claim is over a smaller set than the one reported",
		runs, totalEvaluated)

	// AUD-008 / C-12: every ACCEPTED triple must have EXECUTED without error. The prior code
	// discarded dag.Execute's error, so a triple whose execution REGRESSED to an error still
	// counted as a run and contributed a clean no-witness verdict — the emptiness claim ranged
	// over triples that may never have exercised their control flow. Assert none errored.
	require.Empty(t, execErrs,
		"%d of the %d accepted triples ERRORED during Execute; the no-witness verdict must range only over "+
			"triples that actually executed, not merely those that built: %v", len(execErrs), runs, execErrs)

	// Computed from the counters above, never transcribed. Every population floor below
	// interpolates it, so a failure carries the whole picture rather than one number.
	breakdown := vb07Breakdown(familyShapes, familyBuilt, acceptedByFamily, flagEvaluated)

	bypassableVerifiers := make([]string, 0, len(bypassableSeen))
	for v := range bypassableSeen {
		bypassableVerifiers = append(bypassableVerifiers, v)
	}
	sort.Strings(bypassableVerifiers)

	require.NotZero(t, graphsBuilt, "no generated shape built: the GENERATOR is broken, not the engine")
	require.NotZero(t, triplesAccepted,
		"HEAD accepted NO boundary declaration over any generated graph: the sweep examined an "+
			"empty population and its silence means nothing")
	require.NotZero(t, runs, "no accepted triple was executed: the sweep is BROKEN, not the engine")

	// The two-merge family must actually have been BUILT, not silently rejected by the
	// reconvergence validator. Without this the widened bound could contribute nothing and
	// the sweep would report the same clean zero as before at the same cost.
	require.NotZero(t, twoMergeBuilt,
		"no TWO-MERGE shape built: the widened bound contributed nothing and the sweep cannot "+
			"speak to the V-dominates-m1-but-not-m2 configuration at all")
	require.NotZero(t, twoMergeAccepted,
		"HEAD accepted no declaration over any two-merge graph: the widened bound is present in "+
			"the generator and ABSENT from the population the sweep actually examined")
	// 🔴 THE ASYMMETRIC CONFIGURATION IS MEASURED UNREACHABLE, AND THAT IS THE RESULT --
	// NOT A GAP IN THE SWEEP. The two-merge bound was added to reach "V dominates m1 and not
	// m2". It never occurs, and the reason is structural: validateReconvergence APPENDS the
	// merge<-choice DEPMODEL edge, so the ChoiceNode dominates every merge and no branch-side
	// node dominates any merge. Measured over the two-merge family, the ONLY verifiers HEAD
	// ever accepts are the root and the ChoiceNode, and each dominates BOTH merges in both
	// the serial and the parallel topology.
	//
	// A NON-ZERO HERE IS THE ALARM. It means the reconvergence edge or the dominance
	// predicate changed, the asymmetric case became reachable, and the two-merge reasoning
	// behind the J2 subsumption has to be re-derived rather than this assertion relaxed.
	vb07AssertVerdict(t, "asymmetricDominance", asymmetricDominance == 0,
		"an accepted two-merge declaration now has its verifier dominating ONE merge and not "+
			"the other. That configuration was measured unreachable, because the merge<-choice "+
			"DEPMODEL edge makes the ChoiceNode dominate every merge; if it is reachable now, "+
			"the J2 subsumption argument must be re-derived, not this assertion relaxed")

	// AND THE LOAD-BEARING ONE: every verifier HEAD accepts over a two-merge graph must be a
	// node that CANNOT reach Bypassed. This is the subsumption's actual mechanism rather than
	// a proxy for it -- the J2 hazard's first conjunct is a Bypassed verifier, so if no
	// accepted verifier can ever be Bypassed the hazard has no way in. It fails the moment a
	// branch-side node becomes an acceptable verifier over a merge.
	require.NotZero(t, twoMergeVerifiers,
		"no accepted two-merge declaration was examined for its verifier's bypass-reachability: "+
			"the check below is over an EMPTY set and returns true for that reason")

	// 🔴 119-F5: THE PARALLEL FAMILY NEEDS ITS OWN FLOOR, because two_merge_built is a SUM.
	// Bite-proven by review: delete the parallel family and every assertion here still
	// passes, reporting shapes=564 built=564 two_merge_built=324 -- EXACTLY the pre-repair
	// numbers 119-F3 was raised about. A silent regression to the state the finding flagged
	// would be indistinguishable from the fix being in place. This is not hypothetical: 80
	// of 162 generated parallel shapes ALREADY fail a reconvergence rule, so any future
	// tightening could take the surviving ones without a word.
	// 🔴 THE FLOORS ARE DRIVEN OFF THE (FAMILY x VERDICT) CROSS-PRODUCT, BOTH AXES DERIVED.
	//
	// F5 -> F8 -> F9 were four rounds, four fixes, each aimed at ONE CELL, and each round
	// found the cell nobody had written. That is not a converging process -- it is a
	// hand-written enumeration being audited one entry at a time. So the enumeration is gone:
	// families come from the generated shapes, verdicts come from this file's own AST, and a
	// new family or a new verdict gets its cell BY CONSTRUCTION rather than by the next
	// review round. Fix the class, not the instances.
	//
	// 🔴 AND THE CELLS ARE NOT UNIFORMLY REQUIRED, WHICH IS WHY THIS IS NOT A 9-WAY NotZero.
	// MEASURED, not assumed: the observation site is inside `if s.secondMerge`, so the
	// one-merge family CANNOT reach asymmetricDominance or bypassableVerifiers at all. A
	// "floor every cell non-zero" would red on CORRECT code -- the precise failure that gets
	// a guard deleted by the next person who meets it. So an empty cell must be DECLARED
	// structurally empty with its reason, and the check runs in both directions: an
	// undeclared empty cell reds, and a declared-empty cell that becomes populated reds too,
	// because a stale exemption is how coverage is lost quietly.
	// The FAMILY axis, DERIVED from the generated shapes. A hand-written list silently
	// under-floors a family somebody adds to the generator, which is the inherit-the-answer
	// defect the whole sweep exists to avoid.
	families := map[string]bool{}
	for _, sh := range shapes {
		families[sh.family()] = true
	}
	require.NotEmpty(t, families, "the generator produced no families: it is BROKEN, not the engine")

	// 🔴 DERIVATION CANNOT DETECT A REMOVAL, AND 047bb68 DELETED THE ONLY THING THAT COULD.
	// A derived family list grows by construction when the generator gains a topology -- but
	// if someone DELETES "parallel" from the chain list, familyNames silently becomes
	// {one-merge, serial} and every cross-product cell is satisfied over the smaller set. The
	// hand-written per-chain build floors used to catch exactly that, and replacing them with
	// the derived product removed the detection along with the enumeration.
	//
	// So this list is the DECLARED MINIMUM and nothing else. It does not enumerate the family
	// axis -- the cross-product does that, and a family added here-unlisted is still floored.
	// Its only job is that a family DISAPPEARING reds. Same shape as vb09NonTerminalStatuses:
	// the derived side cannot drift, and the declared side exists solely so a removal cannot
	// pass silently.
	for _, required := range []string{"one-merge", "serial", "parallel"} {
		require.Truef(t, families[required],
			"family %q is GONE from the generated corpus. A derived family list grows silently and "+
				"shrinks silently; every cross-product cell below would be satisfied over the "+
				"remaining families and report a clean verdict over a smaller population than the "+
				"one this phase measured", required)
	}
	familyNames := make([]string, 0, len(families))
	for f := range families {
		familyNames = append(familyNames, f)
	}
	sort.Strings(familyNames)

	// The POPULATION axis, keyed by family throughout. built -> accepted -> observed ->
	// evaluated are the four populations a family passes through; every verdict consumes one.
	// 🔴 KEYED BY THE COUNTER'S OWN VARIABLE NAME, so the DERIVED edge and the DECLARED
	// binding live in ONE namespace and can be compared directly. Keyed by prose labels
	// ("evaluated") they could not be: closing 119-F17 then needed a hardcoded third string to
	// bridge the two namespaces, and a hardcoded expectation is precisely the defect 119-F17
	// is about. Caught by reading which assertion fired in the rebinding bite -- it was the
	// hardcoded one, not the derived one.
	populations := map[string]map[string]int{
		"builtByFamily":    builtByFamily,
		"acceptedByFamily": acceptedByFamily,
		"observedByFamily": observedByFamily,
		"flagEvaluated":    flagEvaluated,
	}
	verdictPopulation := map[string]string{
		"witnesses":           "flagEvaluated",
		"asymmetricDominance": "observedByFamily",
		"bypassableVerifiers": "observedByFamily",
	}
	// Every family x population cell, both axes derived.
	popNames := make([]string, 0, len(populations))
	for name := range populations {
		popNames = append(popNames, name)
	}
	sort.Strings(popNames)
	for _, pop := range popNames {
		for _, family := range familyNames {
			cell := family + " x " + pop
			reason, declaredEmpty := vb07StructurallyEmptyCells[cell]
			switch {
			case populations[pop][family] == 0 && !declaredEmpty:
				require.Failf(t, "uncovered cross-product cell",
					"cell %q is EMPTY and not declared structurally empty. Either the family cannot "+
						"reach that population -- declare it in vb07StructurallyEmptyCells with the "+
						"reason -- or it is being starved and this is the finding.\n%s", cell, breakdown)
			case populations[pop][family] > 0 && declaredEmpty:
				require.Failf(t, "stale structural-emptiness exemption",
					"cell %q is declared structurally empty (%q) but has %d observations; a stale "+
						"exemption silently excuses a cell from coverage", cell, reason,
					populations[pop][family])
			}
		}
	}
	for _, verdict := range vb07DeclaredVerdicts(t, "TestVB07_J2_PopulationSearch") {
		popName, registered := verdictPopulation[verdict]
		require.Truef(t, registered,
			"verdict %q is asserted by this test and is bound to NO population, so nothing floors "+
				"it per family. Bind it in verdictPopulation -- a verdict whose population nobody "+
				"measures is a verdict whose emptiness means nothing", verdict)
		_, known := populations[popName]
		require.Truef(t, known,
			"verdict %q is bound to population %q, which is not measured", verdict, popName)
	}

	// 🔴 119-F17: THE EDGE JOINING THE TWO AXES WAS HAND-WRITTEN AND UNAUDITED, which is the
	// F5 -> F8 -> F9 shape one level up: the cell nobody wrote became the edge nobody checked.
	// Both axes derive; the map joining them did not, and nothing asserted that a verdict names
	// the population that ACTUALLY FEEDS IT. Rebinding "witnesses" from "evaluated" to "built"
	// PASSED -- the check only asked that the binding name SOME measured population.
	//
	// That matters beyond this file: the terminal-floor argument (witnesses append from one
	// site, so counting verdict evaluations subsumes every rung above it) is the basis of the
	// J2 subsumption that phases 120-123 INHERIT. A silently rebound edge stops that argument
	// backing the verdict with every gate green.
	//
	// Two directions, mirroring the exemption map which already had both:
	//   (1) REVERSE -- every binding must name a LIVE verdict, so a binding whose verdict was
	//       renamed or deleted reds instead of lingering as a dead entry that reads as coverage;
	//   (2) FORWARD, for the load-bearing verdict -- "witnesses" must be bound to the counter
	//       incremented at the witness append site, DERIVED from this file's AST rather than
	//       trusted from the map. The other two verdicts share the observation block and their
	//       binding is stated, not derived; that residual is declared rather than implied.
	declared := map[string]bool{}
	for _, v := range vb07DeclaredVerdicts(t, "TestVB07_J2_PopulationSearch") {
		declared[v] = true
	}
	for verdict := range verdictPopulation {
		require.Truef(t, declared[verdict],
			"verdictPopulation binds %q, which is NOT a verdict this test asserts. A binding whose "+
				"verdict was renamed or removed is a dead edge that still reads as coverage -- the "+
				"same stale-exemption failure the cell map is checked in both directions for",
			verdict)
	}
	// DERIVED against DECLARED, with no third string in between. The counter at the witness
	// append site is read from this file's AST; the binding is read from the map above; they
	// must be the same name. Rebinding the verdict to any other measured population now reds
	// against the code rather than against an expectation somebody typed.
	require.Equalf(t, vb07CounterAtWitnessAppend(t), verdictPopulation["witnesses"],
		"the witnesses verdict is bound to population %q, but the counter incremented at the "+
			"witness append site is %q. The binding must name the population that ACTUALLY feeds "+
			"the verdict; naming a different measured population passes every other check while "+
			"the terminal-floor argument -- the basis of the J2 subsumption that 120-123 inherit "+
			"-- silently stops backing it",
		verdictPopulation["witnesses"], vb07CounterAtWitnessAppend(t))

	require.NotEmpty(t, bypassReachableSeen,
		"vb07BypassReachable flagged NOTHING across the entire sweep. The bypass-reachability "+
			"INSTRUMENT is dead, and the un-bypassable-verifier floor below then passes "+
			"vacuously -- it would report the subsumption as holding over an empty set")
	for _, must := range []string{"b1", "b2"} {
		require.True(t, bypassReachableSeen[must],
			"vb07BypassReachable did not flag %q, a direct ChoiceNode branch target. It reads "+
				"choiceAction's branch list; if that shape changed, the instrument is reading a "+
				"structure that no longer exists and its silence means nothing", must)
	}
	vb07AssertVerdict(t, "bypassableVerifiers", len(bypassableVerifiers) == 0,
		"a verifier HEAD accepts over a two-merge graph CAN reach Bypassed: %v. The J2 hazard "+
			"needs exactly that, and its absence is what the subsumption rests on", bypassableVerifiers)

	t.Logf("A2 search RESULT: shapes=%d graphs_built=%d triples_tried=%d triples_accepted=%d runs=%d "+
		"two_merge_shapes_built=%d (serial=%d parallel=%d) "+
		"two_merge_triples_accepted=%d (serial=%d parallel=%d) "+
		"two_merge_verifiers=%d (serial=%d parallel=%d) "+
		"verdict_evaluated=%v bypass_reachable_flagged=%d asymmetric_dominance=%d witnesses=%d",
		len(shapes), graphsBuilt, triplesTried, triplesAccepted, runs,
		twoMergeBuilt, builtByFamily["serial"], builtByFamily["parallel"],
		twoMergeAccepted, acceptedByFamily["serial"], acceptedByFamily["parallel"],
		twoMergeVerifiers, observedByFamily["serial"], observedByFamily["parallel"],
		flagEvaluated, len(bypassReachableSeen), asymmetricDominance, len(witnesses))
	t.Logf("A2 per-family breakdown, derived from the run's own counters:\n%s", breakdown)
	for _, w := range witnesses {
		t.Logf("A2 WITNESS: %s", w)
	}

	// 🔴 THIS ASSERTION IS THE SUBSUMPTION'S ONLY MECHANICAL GUARD, AND IT WAS MISSING.
	// Until it was added the search LOGGED its witness count and asserted nothing about it,
	// so the claim "the dominance predicate already excludes this hazard" lived only in
	// prose -- and prose does not red. If the root-anchored dominance predicate is ever
	// weakened so that a Bypassed verifier can dominate a merge that still fires, a witness
	// appears here and this reds naming it. A recorded claim that nothing re-runs is the
	// same shape as an analyzer spec no gate invokes.
	// 🔴 THE SAMPLE IS CAPPED, AND THAT IS A DEFECT FIX RATHER THAN TIDINESS. This was
	// require.Empty over the whole slice, and when its own bite made the sweep produce
	// witnesses in the thousands the rendered failure came back with an EMPTY Error and an
	// EMPTY Messages line -- the assertion fired at the right place and told the reader
	// nothing at all. A guard whose failure names no member is no more use than one that
	// does not fire. The count here is CHECKED against the measurement in the same
	// assertion, which is the permitted shape; what must never appear is a count instead of
	// the identities.
	sample := witnesses
	if len(sample) > 10 {
		sample = sample[:10]
	}
	vb07AssertVerdict(t, "witnesses", len(witnesses) == 0,
		"a boundary HEAD ACCEPTS admits a run in which the sink's action ran while the verifier's "+
			"did not, with the verifier Bypassed. VB-07's J2 hazard was measured EMPTY at this bound "+
			"and recorded as SUBSUMED by the dominance predicate; a witness here means that "+
			"subsumption no longer holds and the reasoning behind it must be re-derived, not this "+
			"assertion relaxed.\n%d witness(es); first %d:\n  %s",
		len(witnesses), len(sample), strings.Join(sample, "\n  "))
}

// ---------------------------------------------------------------------------
// A3's input: what would J2's STATIC predicate actually reject?
// ---------------------------------------------------------------------------
//
// A2 answers "does the runtime hazard occur over a boundary HEAD accepts" -- and at its
// bound the answer is no. That is NOT the same question as "would J2's clause reject
// anything", and conflating them is how a clause with no population gets written anyway.
// So the static predicate is evaluated over the SAME accepted triples, separately, and its
// count is reported. A clause that rejects nothing is dead code; a clause that rejects
// declarations with no demonstrated unsoundness is a false refusal. The two call for
// different decisions and only a measurement separates them.

// vb07BypassReachable computes, structurally over the built graph, the nodes that can
// legitimately reach Bypassed. Derived from the engine's OWN rules rather than hand-listed:
//
//   - a ChoiceNode's branch targets are marked Bypassed by choiceAction.bypassExcept;
//   - a non-merge node blocked SOLELY by Bypassed dependencies is Bypassed
//     (classifyBlockedStatus's bypass rule), so it inherits when ALL its deps do;
//   - a merge with zero taken tails is Bypassed, so it inherits when all its TAILS do.
func vb07BypassReachable(dag *DAG) map[string]bool {
	br := map[string]bool{}
	for _, n := range dag.nodes {
		ca, ok := n.action.(*choiceAction)
		if !ok {
			continue
		}
		for _, b := range ca.branches {
			br[b.target] = true
		}
		if ca.hasDefault {
			br[ca.defaultTarget] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for name, n := range dag.nodes {
			if br[name] || len(n.dependsOn) == 0 {
				continue
			}
			var deps []string
			if ma, isMerge := n.action.(*mergeAction); isMerge {
				deps = ma.tails
			} else {
				for _, d := range n.dependsOn {
					deps = append(deps, d.name)
				}
			}
			if len(deps) == 0 {
				continue
			}
			all := true
			for _, d := range deps {
				if !br[d] {
					all = false
					break
				}
			}
			if all {
				br[name] = true
				changed = true
			}
		}
	}
	return br
}

// vb07J2Static is J2's clause as a PREDICATE, evaluated outside production code so its
// population can be measured before anything is shipped. It reports the offending
// (merge, dependency) pair, or ("", "").
func vb07J2Static(dag *DAG, succ map[string][]string, d boundaryDecl) (string, string) {
	br := vb07BypassReachable(dag)
	for name, n := range dag.nodes {
		ma, isMerge := n.action.(*mergeAction)
		if !isMerge {
			continue
		}
		// M must lie on a V->S path, or BE S. "Over merge nodes generally, not sinks
		// only" is the half earlier wordings dropped.
		onPath := name == d.sink ||
			(reachAvoiding(succ, d.verifier, name, nil) != nil &&
				reachAvoiding(succ, name, d.sink, nil) != nil)
		if !onPath {
			continue
		}
		tailSet := map[string]bool{}
		for _, t := range ma.tails {
			tailSet[t] = true
		}
		for _, dep := range n.dependsOn {
			if !tailSet[dep.name] && br[dep.name] {
				return name, dep.name
			}
		}
	}
	return "", ""
}

// TestVB07_J2_StaticPredicatePopulation measures how many declarations HEAD accepts that
// J2's static clause would refuse. This is the number A3 is decided on.
func TestVB07_J2_StaticPredicatePopulation(t *testing.T) {
	// The SAME generated corpus the search uses, so the two measurements cannot describe
	// different populations. The branch outcome is irrelevant to a STATIC predicate, so one
	// of the two outcome variants of each shape is dropped rather than counted twice.
	var shapes []vb07Shape
	for _, sh := range vb07Shapes() {
		if !sh.takeBranch1 {
			shapes = append(shapes, sh)
		}
	}

	accepted, wouldRefuse, wouldRefuseGuarded := 0, 0, 0
	var samples []string
	for _, s := range shapes {
		names := s.nodes()
		for _, doer := range names {
			for _, verifier := range names {
				for _, sink := range names {
					decl := boundaryDecl{doer: doer, verifier: verifier, sink: sink}
					dag, err := vb07Build(s, newVB07Recorder(), &decl)
					if err != nil {
						continue
					}
					accepted++
					if m, dep := vb07J2Static(dag, successors(dag), decl); m != "" {
						wouldRefuse++
						// The candidate repair: J2 also requires that the VERIFIER
						// can itself reach Bypassed. If V cannot be Bypassed there is
						// no hazard for J2 to name, and the predicate as specified
						// never checks it -- which is why it refuses declarations
						// whose verifier is the root.
						if vb07BypassReachable(dag)[decl.verifier] {
							wouldRefuseGuarded++
						}
						if len(samples) < 5 {
							samples = append(samples, fmt.Sprintf(
								"shape[%s] boundary(%s, %s, %s) -> merge %q dep %q",
								s, doer, verifier, sink, m, dep))
						}
					}
				}
			}
		}
	}

	require.NotZero(t, accepted,
		"HEAD accepted no declaration over any generated graph: the measurement is over an "+
			"EMPTY population and its number means nothing")

	// 🔴 119-F13: THESE TWO NUMBERS WERE LOGGED AND ASSERTED BY NOTHING, and the doc comment
	// above calls the second "the number A3 is decided on". That is this file's own rule,
	// broken in the sibling of the test that states it: the sweep's own comment says the
	// search "LOGGED its witness count and asserted nothing about it, so the claim lived only
	// in prose -- and prose does not red". Fixed for the sweep, still true here.
	//
	// The load-bearing one is wouldRefuseGuarded. Any artifact citing "the guarded clause
	// refuses ZERO declarations" as the subsumption's basis is citing this number, and
	// without an assertion it could drift to non-zero with the gate fully green.
	require.Zero(t, wouldRefuseGuarded,
		"J2 WITH the verifier-is-Bypassed-reachable conjunct now refuses %d of the %d declarations "+
			"HEAD accepts. It refused ZERO when the subsumption was decided, and that zero is the "+
			"basis every artifact cites for shipping no J2 clause. A non-zero here means the "+
			"repaired predicate has acquired a population and the disposition must be re-derived",
		wouldRefuseGuarded, accepted)
	require.NotZero(t, wouldRefuse,
		"J2 AS SPECIFIED now refuses ZERO declarations. The false-refusal measurement is what "+
			"established that shipping the clause as written would break previously-legal graphs; "+
			"a zero here means that measurement no longer holds and the disposition rests on nothing")

	t.Logf("A3 INPUT: declarations HEAD accepts = %d; J2 as specified would refuse = %d; "+
		"J2 with the verifier-is-Bypassed-reachable conjunct would refuse = %d",
		accepted, wouldRefuse, wouldRefuseGuarded)
	for _, s := range samples {
		t.Logf("A3 SAMPLE would-refuse: %s", s)
	}
}

// vb07Shapes enumerates the generated corpus: every one-merge shape, plus the two-merge
// shapes that probe the step the subsumption claim rests on.
//
// 🔴 THE TWO-MERGE FAMILY IS DELIBERATELY NARROWER THAN THE ONE-MERGE FAMILY, and the
// asymmetry is a COST decision taken with a measurement rather than a guess. Branch bodies
// are dropped there (the tail-set mode already varies which branch each merge joins, which
// is the axis that matters for a non-tail Bypassed-reachable dependency) and the extra-dep
// alphabet is the branch entries only. What it MUST contain, and does, is the shape where
// m1 and m2 join DIFFERENT branches and m2 carries a non-tail dependency on the other one:
// that is the "V dominates m1 but not m2" configuration the one-merge bound cannot express.
// The full unrestricted two-merge cross-product is roughly an order of magnitude more work
// for shapes that differ only in branch-body padding; the timing is reported in the SUMMARY.
// 🔴 THE PER-FAMILY BREAKDOWN IS DERIVED AND PRINTED BY THE SWEEP -- IT IS NOT WRITTEN HERE.
//
// An earlier draft transcribed a shapes/build/rejected table into this comment. It was
// accurate when written and it was a FIFTH RESTATED NUMBER in a phase that had already
// produced four: nothing re-ran it, so it could only rot. Deleting it outright would have
// been worse -- the 726-vs-646 gap would then be left for the next reader to re-derive.
//
// So it is BOUND ON THE QUERY instead, which is DEC-M23-ORACLE-SCOPE-CHANNELS-R2 one level
// down: TestVB07_J2_PopulationSearch computes the breakdown from its own counters, logs it,
// and interpolates it into every population floor's failure message. To see it, run the
// sweep; the numbers come out of the run and cannot disagree with it.
//
// What is worth stating here is the SHAPE, which is not a number and does not drift: the
// parallel two-merge family does NOT fully build, and every rejection in it is the same
// reconvergence error. That is why the earlier claim -- "validateReconvergence rejects a
// parallel pair" -- was false as a CATEGORICAL and right about the MECHANISM.
func vb07Shapes() []vb07Shape {
	var shapes []vb07Shape
	for _, body1 := range []bool{false, true} {
		for _, body2 := range []bool{false, true} {
			for _, tailsMode := range []string{"both", "only1", "only2"} {
				for _, sep := range []bool{false, true} {
					for _, take1 := range []bool{false, true} {
						base := vb07Shape{
							body1: body1, body2: body2, tailsMode: tailsMode,
							separateSink: sep, takeBranch1: take1,
						}
						extras := []string{"", "r", "b1", "b2"}
						if body1 {
							extras = append(extras, "b1t")
						}
						if body2 {
							extras = append(extras, "b2t")
						}
						for _, e := range extras {
							s := base
							s.extraDep = e
							shapes = append(shapes, s)
						}
					}
				}
			}
		}
	}
	for _, chain := range []string{"serial", "parallel"} {
		for _, tailsMode := range []string{"both", "only1", "only2"} {
			for _, tailsMode2 := range []string{"both", "only1", "only2"} {
				for _, extra := range []string{"", "b1", "b2"} {
					for _, extra2 := range []string{"", "b1", "b2"} {
						for _, sep := range []bool{false, true} {
							for _, take1 := range []bool{false, true} {
								if chain == "parallel" && !sep {
									continue // a parallel pair needs the shared sink to be a pair
								}
								shapes = append(shapes, vb07Shape{
									chain: chain, tailsMode: tailsMode, extraDep: extra,
									secondMerge: true, tailsMode2: tailsMode2, extraDep2: extra2,
									separateSink: sep, takeBranch1: take1,
								})
							}
						}
					}
				}
			}
		}
	}
	return shapes
}

// vb07DominatesOne reports whether v dominates EXACTLY ONE of the two merges -- the
// asymmetric configuration the two-merge bound exists to reach.
//
// Dominance is computed the way validateBoundary computes it, over the SAME built graph
// and after validateReconvergence has appended its merge<-choice edges: v dominates x iff
// no root reaches x along a path avoiding v. Reusing the shipped helpers rather than
// reimplementing them is deliberate -- a private copy of the dominance walk could agree
// with nothing.
func vb07DominatesOne(dag *DAG, v string) bool {
	if _, ok := dag.nodes["m2"]; !ok {
		return false
	}
	succ := successors(dag)
	dominates := func(x string) bool {
		if v == x {
			return false
		}
		for _, root := range rootNames(dag) {
			if root == v {
				continue
			}
			if reachAvoiding(succ, root, x, &v) != nil {
				return false
			}
		}
		return true
	}
	return dominates("m") != dominates("m2")
}

// TestBoundary_J1_RefusalLeavesTheBuilderReusable pins the correction behind J1's
// placement note (the review's second minor).
//
// 🔴 WHY IT EXISTS. J1's comment used to justify its position by saying the action clause
// MUTATES and a refused graph must not be mutated on the way out. The mutation is real --
// validateBoundary assigns snapshotBoundaryAction's result over the node's action -- but it
// is UNOBSERVABLE: build() allocates a fresh *Node from the NodeBuilder's own action value,
// and snapshotBoundaryAction allocates its result rather than editing in place. The wording
// now rests on cost alone, and this is what stops the false version drifting back, because a
// comment cannot fail and this can.
//
// 🔴 SCOPE, STATED SO A GREEN IS NOT OVER-READ. The ACCEPTED arm is the load-bearing one:
// there the action clause demonstrably RUNS and snapshots both V and S, so a re-build that
// still runs the consumer's own action is real evidence the builder was untouched. On the
// REFUSED arm J1 answers first and the action clause never executes at all -- so that arm
// witnesses builder reusability and says NOTHING about the snapshot. An assertion about the
// consumer's action value on the refused path would be VACUOUS, and is deliberately absent.
func TestBoundary_J1_RefusalLeavesTheBuilderReusable(t *testing.T) {
	newBuilder := func(composite *CompositeAction, verifierCOE, withBoundary bool) *WorkflowBuilder {
		b := NewWorkflowBuilder().WithWorkflowID("j1-reuse")
		b.AddNode("D").WithAction(plainAct())
		v := b.AddNode("V").WithAction(composite).DependsOn("D")
		if verifierCOE {
			v.WithContinueOnError()
		}
		b.AddNode("S").WithAction(plainAct()).DependsOn("V")
		if withBoundary {
			b.WithBoundary("D", "V", "S")
		}
		return b
	}

	t.Run("ACCEPTED: the snapshot RUNS and the builder's action survives it", func(t *testing.T) {
		ran := 0
		// A CompositeAction is the one kind snapshotBoundaryAction clones, so it is the
		// kind that would expose an in-place edit if there were one.
		composite := NewCompositeAction(ActionFunc(func(context.Context, *WorkflowData) error {
			ran++
			return nil
		}))

		dag, err := newBuilder(composite, false, true).Build()
		require.NoError(t, err, "this declaration is satisfied; the action clause runs and snapshots")
		require.NotNil(t, dag)

		// 🔴 ASSERT IDENTITY, NOT LENGTH. The property is "snapshotBoundaryAction ALLOCATES
		// rather than editing in place", and that is a statement about WHICH OBJECT the
		// built node holds. A length check cannot see it: an in-place edit that assigns a
		// same-length clone back onto the consumer's own composite leaves the length at 1
		// and sails through. The first draft of this test asserted only the length, and its
		// bite red only because the mutation happened to APPEND -- the assertion was fitted
		// to the mutation instead of to the property. Ask what an assertion can SEE.
		built, ok := dag.nodes["V"]
		require.True(t, ok, "V must be a node of the built graph")
		require.NotSame(t, composite, built.action,
			"the built node must hold a SNAPSHOT, not the consumer's own action object. If these "+
				"are the same pointer, snapshotBoundaryAction edited in place, and every claim "+
				"that a refused or accepted build leaves the consumer's value untouched is false")
		require.Len(t, composite.actions, 1,
			"and the consumer's own action must be unchanged in content as well as identity")

		// Re-build from the SAME builder and run it. If the snapshot had written through to
		// the builder, this is where a wrong or missing action would surface.
		dag, err = newBuilder(composite, false, true).Build()
		require.NoError(t, err)
		require.NoError(t, dag.Execute(context.Background(), NewWorkflowData("j1-reuse")))
		require.Equal(t, 1, ran, "the re-built DAG must run the consumer's own action")
	})

	t.Run("REFUSED: J1 answers and the builder stays reusable", func(t *testing.T) {
		ran := 0
		composite := NewCompositeAction(ActionFunc(func(context.Context, *WorkflowData) error {
			ran++
			return nil
		}))

		dag, err := newBuilder(composite, true, true).Build()
		require.Nil(t, dag, "J1 must refuse a verifier carrying ContinueOnError")
		require.ErrorIs(t, err, ErrValidation)
		require.Contains(t, err.Error(), "ContinueOnError")

		// The SAME builder shape, without the declaration, must still build and run.
		dag, err = newBuilder(composite, true, false).Build()
		require.NoError(t, err, "a refused build must leave the consumer's action reusable")
		require.NoError(t, dag.Execute(context.Background(), NewWorkflowData("j1-reuse")))
		require.Equal(t, 1, ran, "the re-built DAG must run the consumer's own action")
	})
}

// family names the generated family a shape belongs to, for the terminal verdict-evaluation
// floor. Three families, because "two merges" is not one population: the serial and parallel
// topologies are reached by different code and are starved independently.
func (s vb07Shape) family() string {
	if !s.secondMerge {
		return "one-merge"
	}
	// 🔴 RETURN THE CHAIN VERBATIM. This used to fold every non-"parallel" chain into
	// "serial" (reviewer minor), so adding a third topology to the generator would have had
	// its triples counted under serial's floor and never floored on its own -- a family
	// silently under-covered by the very mechanism built to stop that. The caller derives its
	// family list from the shapes, so a new chain value grows a floor instead of hiding.
	return s.chain
}

// TestVB07_WitnessAppendSiteIsSingular is the guard for the ONE structural fact the
// termination argument rests on.
//
// 🔴 WHY THIS EXISTS. The terminal floor claims that counting evaluations of the witness
// predicate subsumes every population above it. That is only true because `witnesses` is
// appended from EXACTLY ONE place: starve a family anywhere upstream and its evaluation
// count is zero. Add a SECOND append site and the argument fails SILENTLY -- the terminal
// floor stays green while witnesses arrive through a path it never counted.
//
// The premise was true when written and checked by nobody, which is the shape this whole
// phase is about. It is now checked on every run, in the same idiom as the exactly-one
// executeNodesInLevel control and VB-09's same-name-method ambiguity floor: not "we believe
// it is singular" but "it reds the day it stops being".
func TestVB07_WitnessAppendSiteIsSingular(t *testing.T) {
	const self = "boundary_vb07_test.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, self, nil, 0)
	require.NoError(t, err, "parsing this test's own source")

	var sites []string
	ast.Inspect(f, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, lhs := range assign.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok || id.Name != "witnesses" {
				continue
			}
			sites = append(sites, fmt.Sprintf("%s:%d", self, fset.Position(assign.Pos()).Line))
		}
		return true
	})

	// 🔴 119-F14, TAKEN AS A DECLARED LIMITATION RATHER THAN A MATCHER CHANGE, and the reason
	// is on the record rather than implied. This guard matches the NAME `witnesses`, not the
	// PROPERTY "something appended to the witness set". A second contribution path that took
	// a *[]string and appended through the pointer would leave this green while witnesses
	// arrived by an uncounted route -- the reviewer demonstrated it.
	//
	// Matching the property instead would mean following aliasing, which a syntactic AST pass
	// cannot do; the honest options are a name match with the residual DECLARED, or a
	// type-resolution dependency this module does not carry. The residual is real and it is
	// narrow: it needs somebody to route witness appends through a helper, which is a
	// deliberate act rather than churn. Declared here so a future reader does not read this
	// guard as proving more than it does -- the same treatment as vb09Limitations, and the
	// same reason: an undeclared gap tells a reader the gaps are enumerated when they are not.
	require.Lenf(t, sites, 1,
		"the witness set is assigned from %d place(s) (%s), and the terminal-floor argument "+
			"requires EXACTLY ONE. That argument is what licenses counting verdict-predicate "+
			"evaluations instead of flooring every population above it -- with a second append "+
			"site, a family can contribute witnesses through a path the terminal floor never "+
			"counts, and it stays green while the claim is false. Re-derive the termination "+
			"argument; do not relax this. DECLARED RESIDUAL: this matches the NAME, so a helper "+
			"appending through a *[]string is invisible to it.", len(sites), strings.Join(sites, ", "))
}

// vb07Breakdown renders the per-family shapes/built/rejected/accepted/evaluated table from
// the sweep's own counters.
//
// 🔴 IT EXISTS SO THE TABLE IS NEVER WRITTEN DOWN. A transcribed table is a number nothing
// re-runs; this one cannot disagree with the run that produced it, and it rides in every
// population floor's failure message so a red carries the whole picture rather than the one
// counter that tripped. Bind on the query, never on the number.
func vb07Breakdown(shapes, built, accepted, evaluated map[string]int) string {
	names := make([]string, 0, len(shapes))
	for f := range shapes {
		names = append(names, f)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("    per-family, derived from this run:\n")
	fmt.Fprintf(&b, "      %-12s %8s %8s %10s %10s %10s\n",
		"family", "shapes", "built", "rejected", "accepted", "evaluated")
	totShapes, totBuilt, totAccepted, totEval := 0, 0, 0, 0
	for _, f := range names {
		fmt.Fprintf(&b, "      %-12s %8d %8d %10d %10d %10d\n",
			f, shapes[f], built[f], shapes[f]-built[f], accepted[f], evaluated[f])
		totShapes += shapes[f]
		totBuilt += built[f]
		totAccepted += accepted[f]
		totEval += evaluated[f]
	}
	fmt.Fprintf(&b, "      %-12s %8d %8d %10d %10d %10d\n",
		"TOTAL", totShapes, totBuilt, totShapes-totBuilt, totAccepted, totEval)
	return b.String()
}

// TestVB07_SweepLoopHasNoSkipBetweenAcceptAndVerdict is the WARRANT for the equality
// assertions in the sweep, and it is the reason they are invariants rather than facts about
// today's code.
//
// 🔴 THE CONDITION THIS MEETS. `require.Equal(triplesAccepted, runs)` is only safe if no
// LEGITIMATE path can accept a triple without running and evaluating it. If one could, the
// equality would red on correct code -- and a guard that reds spuriously gets deleted by the
// next person who meets it, taking the real coverage with it. So the straight-line property
// is asserted structurally rather than read once and trusted: between the accept counter and
// the verdict-evaluation counter the loop body must contain no branch out.
//
// This is the same instrument as the singular-append-site guard: a claim about the SHAPE of
// this file, checked against this file's own AST on every run.
func TestVB07_SweepLoopHasNoSkipBetweenAcceptAndVerdict(t *testing.T) {
	const self = "boundary_vb07_test.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, self, nil, 0)
	require.NoError(t, err)

	// Find the innermost block holding both counters, then walk the statements between them.
	var region []ast.Stmt
	ast.Inspect(f, func(n ast.Node) bool {
		block, ok := n.(*ast.BlockStmt)
		if !ok {
			return true
		}
		start, end := -1, -1
		for i, st := range block.List {
			switch text(fset, st) {
			case "triplesAccepted++":
				start = i
			case "flagEvaluated[s.family()]++":
				end = i
			}
		}
		if start >= 0 && end > start {
			region = block.List[start+1 : end]
		}
		return true
	})
	require.NotNil(t, region,
		"could not locate the statements between triplesAccepted++ and flagEvaluated[...]++ in "+
			"the sweep loop. This guard is the WARRANT for the equality assertions; if it cannot "+
			"find its subject it proves nothing and must be repaired, not deleted")

	var branches []string
	for _, st := range region {
		branches = append(branches, vb07LoopEscapes(fset, self, st, 0, 0)...)
	}

	require.Emptyf(t, branches,
		"the sweep loop can now BRANCH between accepting a triple and evaluating the verdict: %s. "+
			"That breaks the warrant for require.Equal(triplesAccepted, runs) -- with a skip in "+
			"there the equality reds on code that is doing the right thing, and whoever meets that "+
			"red will delete the assertion rather than the skip. Either remove the branch or "+
			"re-derive the equality's warrant; do not relax this.", strings.Join(branches, ", "))
}

// text renders a statement's source form for exact matching.
func text(fset *token.FileSet, n ast.Node) string {
	var b strings.Builder
	if err := printer.Fprint(&b, fset, n); err != nil {
		return ""
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// The (family x verdict) cross-product: both axes derived, neither enumerated.
// ---------------------------------------------------------------------------

// vb07StructurallyEmptyCells declares the cells a family CANNOT reach, with the reason.
//
// 🔴 THIS IS AN EXEMPTION LIST AND IT IS CHECKED IN BOTH DIRECTIONS. An undeclared empty
// cell reds (a family is being starved, or a new one arrived); a declared-empty cell that
// becomes populated ALSO reds, because a stale exemption quietly excuses a cell from
// coverage and that is how this phase lost coverage four times running.
//
// Both entries are the same measured fact: the site feeding these two verdicts sits inside
// `if s.secondMerge`, so a one-merge shape cannot reach it. That is why the cross-product is
// not a uniform NotZero -- a guard that reds on correct code gets deleted by whoever meets it.
var vb07StructurallyEmptyCells = map[string]string{
	"one-merge x observedByFamily": "the observation population is fed inside `if s.secondMerge`; a one-merge shape never reaches it, so its asymmetricDominance and bypassableVerifiers cells are empty BY CONSTRUCTION",
}

// vb07AssertVerdict is the single door every verdict of the sweep goes through, and the
// reason the VERDICT axis of the cross-product is derivable rather than hand-listed:
// vb07DeclaredVerdicts recovers the set by reading this file's calls to it.
//
// DECLARED LIMITATION, stated rather than left for a reviewer to find: a verdict asserted
// WITHOUT this helper is invisible to the derivation. That is the residual, it is the same
// shape as vb09Limitations, and it is why the helper is the only sanctioned way to add one.
func vb07AssertVerdict(t *testing.T, name string, holds bool, msg string, args ...interface{}) {
	t.Helper()
	// 🔴 119-F19: THE FORMAT STRING IS A CONSTANT AND THE CALLER'S MESSAGE IS AN ARGUMENT.
	// Building "verdict %q: " + msg made the format NON-CONSTANT, which defeats go vet's
	// printf-wrapper check entirely -- and a literal % in a caller's message then renders as
	// 100%!o(MISSING)f EXACTLY WHEN THE VERDICT FIRES. This file already records fixing an
	// assertion that produced an unreadable failure at the moment it mattered; this is the
	// same defect in the door every verdict now goes through.
	require.Truef(t, holds, "verdict %q: %s", name, fmt.Sprintf(msg, args...))
}

// vb07DeclaredVerdicts derives the verdict axis from this file's own AST -- every call to
// vb07AssertVerdict, by its literal name argument.
//
// A verdict added through the helper therefore acquires its per-family cells BY
// CONSTRUCTION: it appears here, finds no registered population map, and reds. No floor code
// is edited to cover it, which is the entire point -- four consecutive review rounds each
// found the cell nobody had written by hand.
func vb07DeclaredVerdicts(t *testing.T, enclosing string) []string {
	t.Helper()
	const self = "boundary_vb07_test.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, self, nil, 0)
	require.NoError(t, err)

	// 🔴 119-F16: SCOPED TO ONE FUNCTION, BECAUSE THE THING IT FEEDS IS. The first version
	// quantified over the FILE while verdictPopulation is scoped to ONE TEST, so the two
	// quantifiers disagreed -- and the disagreement was reachable by FOLLOWING THIS FILE'S OWN
	// INSTRUCTION. vb07AssertVerdict's doc calls itself "the only sanctioned way to add" a
	// verdict; route a sibling test's verdict through that door and the sweep reds with
	// "verdict X is asserted by this test and is bound to NO population" -- and X is asserted
	// 220 lines away, in a different test. An affirmatively false message, produced by
	// complying with the file. Third time in this phase, and this one is inside the instrument
	// built to close the first two.
	var body *ast.BlockStmt
	for _, decl := range f.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok && fd.Name.Name == enclosing {
			body = fd.Body
		}
	}
	require.NotNilf(t, body,
		"could not find %s in %s to scope the verdict derivation to. A derivation that cannot "+
			"find its own subject proves nothing", enclosing, self)

	var names []string
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || id.Name != "vb07AssertVerdict" {
			return true
		}
		// 🔴 119-F18: RED ON A FORM IT CANNOT READ, NEVER SKIP IT. The first version did
		// `return true` when the name argument was not a string literal, so a verdict named
		// by a const or a variable dropped silently out of the derived axis and its cells
		// went unfloored with the gate green. That is the class 23b1061 fixed once already --
		// "AST filters silently drop the forms they do not match; fix the CLASS" -- reintroduced
		// in a new filter three commits later. A call to the sanctioned door whose name cannot
		// be read is a verdict that is not derivable, and that must be LOUD.
		require.GreaterOrEqualf(t, len(call.Args), 2,
			"a call to the sanctioned verdict door at %s:%d has too few arguments to name a "+
				"verdict; the axis cannot be derived from it",
			self, fset.Position(call.Pos()).Line)
		lit, ok := call.Args[1].(*ast.BasicLit)
		require.Truef(t, ok && lit.Kind == token.STRING,
			"the verdict name at %s:%d is not a string literal, so this call cannot contribute to "+
				"the derived axis -- and a silently-skipped call is a verdict whose cells go "+
				"unfloored with the gate green. Name it with a literal",
			self, fset.Position(call.Pos()).Line)
		// strconv.Unquote rather than trimming quote characters: a backtick-quoted name
		// survives a Trim of `"` with its backticks attached and would never match its binding.
		name, uerr := strconv.Unquote(lit.Value)
		require.NoErrorf(t, uerr, "verdict name %s at %s:%d is not an unquotable literal",
			lit.Value, self, fset.Position(call.Pos()).Line)
		names = append(names, name)
		return true
	})
	sort.Strings(names)

	require.NotEmptyf(t, names,
		"no verdicts derived from %s in %s. The verdict axis of the cross-product is EMPTY, so "+
			"the per-cell check iterates over nothing and passes vacuously -- which is the exact "+
			"defect the cross-product replaced", enclosing, self)
	return names
}

// vb07LoopEscapes reports the statements in n that actually LEAVE the enclosing sweep loop.
//
// 🔴 IT COUNTS NESTING DEPTH, AND THE FIRST VERSION DID NOT -- WHICH MADE IT CRY WOLF
// (119-F11). That version counted every BranchStmt in the region, so an unlabelled
// `continue` TRAPPED BY A NESTED for/range/switch read as a loop escape. The trapping
// construct is already in the region: the `for name, reachable := range br` loop. Rewriting
// `if reachable { … }` as `if !reachable { continue }` -- semantics-preserving, and the
// canonical Go idiom -- left the sweep byte-identical and made this guard FAIL, with a
// message asserting the equality would red on correct code. Both clauses false.
//
// That is the worst failure mode available to this guard and it is the one the equality was
// conditioned on: a guard that reds on correct code gets deleted by the next person who
// meets it, and here that would take the equalities' only re-running warrant with it. Same
// class as 119-F10 -- a replacement message that is affirmatively false.
//
// The rule, per Go's own semantics:
//
//   - an UNLABELLED `continue` is trapped by any enclosing for/range: counts only at loop
//     depth 0;
//   - an UNLABELLED `break` is trapped by for/range AND switch/select: counts only at
//     breakable depth 0;
//   - a LABELLED branch can target an outer loop: counts at ANY depth;
//   - `goto` and `return` always leave: they always count.
//
// The FuncLit exclusion was right in the first version and is kept -- a branch inside a
// closure leaves the closure, not the loop. Nested loops and switches are the other two
// trapping constructs; the concept was there and was applied to one of three cases.
func vb07LoopEscapes(fset *token.FileSet, self string, n ast.Node, loopDepth, breakableDepth int) []string {
	var out []string
	at := func(p token.Pos) string { return fmt.Sprintf("%s:%d", self, fset.Position(p).Line) }

	switch v := n.(type) {
	case nil:
		return nil
	case *ast.FuncLit:
		return nil // a branch inside a closure leaves the closure, not the loop
	case *ast.ForStmt:
		for _, c := range []ast.Node{v.Init, v.Cond, v.Post} {
			out = append(out, vb07LoopEscapes(fset, self, c, loopDepth, breakableDepth)...)
		}
		return append(out, vb07LoopEscapes(fset, self, v.Body, loopDepth+1, breakableDepth+1)...)
	case *ast.RangeStmt:
		out = append(out, vb07LoopEscapes(fset, self, v.X, loopDepth, breakableDepth)...)
		return append(out, vb07LoopEscapes(fset, self, v.Body, loopDepth+1, breakableDepth+1)...)
	case *ast.SwitchStmt:
		out = append(out, vb07LoopEscapes(fset, self, v.Init, loopDepth, breakableDepth)...)
		out = append(out, vb07LoopEscapes(fset, self, v.Tag, loopDepth, breakableDepth)...)
		return append(out, vb07LoopEscapes(fset, self, v.Body, loopDepth, breakableDepth+1)...)
	case *ast.TypeSwitchStmt:
		return append(out, vb07LoopEscapes(fset, self, v.Body, loopDepth, breakableDepth+1)...)
	case *ast.SelectStmt:
		return append(out, vb07LoopEscapes(fset, self, v.Body, loopDepth, breakableDepth+1)...)
	case *ast.BranchStmt:
		switch {
		case v.Label != nil:
			// A labelled branch can name an outer loop, so it escapes at any depth.
			out = append(out, fmt.Sprintf("%s %s at %s", v.Tok, v.Label.Name, at(v.Pos())))
		case v.Tok == token.CONTINUE && loopDepth == 0:
			out = append(out, fmt.Sprintf("continue at %s", at(v.Pos())))
		case v.Tok == token.BREAK && breakableDepth == 0:
			out = append(out, fmt.Sprintf("break at %s", at(v.Pos())))
		case v.Tok == token.GOTO:
			out = append(out, fmt.Sprintf("goto at %s", at(v.Pos())))
		}
		return out
	case *ast.ReturnStmt:
		return append(out, fmt.Sprintf("return at %s", at(v.Pos())))
	}

	// Everything else: recurse into children at the same depths.
	ast.Inspect(n, func(c ast.Node) bool {
		if c == nil || c == n {
			return c == n
		}
		switch c.(type) {
		case *ast.FuncLit, *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt,
			*ast.TypeSwitchStmt, *ast.SelectStmt, *ast.BranchStmt, *ast.ReturnStmt:
			out = append(out, vb07LoopEscapes(fset, self, c, loopDepth, breakableDepth)...)
			return false
		}
		return true
	})
	return out
}

// vb07CounterAtWitnessAppend derives WHICH per-family counter is incremented at the witness
// append site, by reading this file's own AST rather than trusting the binding map.
//
// 🔴 THIS IS THE FORWARD HALF OF 119-F17. The cross-product's two axes are derived; the edge
// joining a verdict to its population was a hand-written map entry that nothing checked, so
// rebinding "witnesses" to a different measured population passed silently. The binding is
// now confronted with the code: whatever counter is incremented immediately before the sole
// witness append IS the population that feeds the witness verdict, and the map must say so.
//
// DECLARED RESIDUAL: this derives the edge for the WITNESS verdict only -- the one the J2
// subsumption's terminal-floor argument rests on. asymmetricDominance and bypassableVerifiers
// share the two-merge observation block and their binding is stated rather than derived.
// Declared here rather than left for a reviewer, on the standing rule that an undeclared gap
// tells a reader the gaps are enumerated when they are not.
func vb07CounterAtWitnessAppend(t *testing.T) string {
	t.Helper()
	const self = "boundary_vb07_test.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, self, nil, 0)
	require.NoError(t, err)

	// Positions, not block structure. A first draft matched on the rendered text of statements
	// in every enclosing BlockStmt and found the append FIVE times -- once per nesting level,
	// because an outer statement's text contains its descendants'. Source order answers the
	// question directly: which per-family counter is incremented closest above the append.
	appendPos := token.NoPos
	appends := 0
	counters := map[token.Pos]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			for _, lhs := range v.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && id.Name == "witnesses" {
					appends++
					appendPos = v.Pos()
				}
			}
		case *ast.IncDecStmt:
			if idx, ok := v.X.(*ast.IndexExpr); ok && v.Tok == token.INC {
				if m, ok := idx.X.(*ast.Ident); ok && text(fset, idx.Index) == "s.family()" {
					counters[v.Pos()] = m.Name
				}
			}
		}
		return true
	})

	require.Equalf(t, 1, appends,
		"expected exactly ONE assignment to the witness set in %s, found %d; the edge derivation "+
			"cannot identify which counter feeds the verdict", self, appends)
	require.NotEqualf(t, token.NoPos, appendPos, "no witness append found in %s", self)

	best, name := token.NoPos, ""
	for pos, m := range counters {
		if pos < appendPos && pos > best {
			best, name = pos, m
		}
	}
	require.NotEmptyf(t, name,
		"no per-family counter increment precedes the witness append in %s. The edge between the "+
			"witnesses verdict and its population cannot be derived, so the binding is unchecked -- "+
			"which is exactly the state 119-F17 found", self)
	return name
}
