package workflow

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Phase 118b (M23) — the crash-window harness and the BAR-M23 oracle that VER-02
// specified for 117's T10, which never landed (F118-VER02-01). Phases 119-121 cite
// VER-02 as their exit gate, so this file is what that citation refers to.
//
// BAR-M23 clause 1: for every declared boundary (D, V, S) and every DAG the library
// accepts, in every execution in which S's action is invoked, V's action ran to
// Completed in the SAME LOGICAL RUN — the WorkflowID drive-set across resumes, not one
// Execute invocation. That distinction is forced by shipped semantics: a Completed node
// does not re-run on resume, so run 1 completes V, crashes before S, and run 2 invokes S
// in a process where V never executed. Stated over one Execute the bar would be violated
// by correct durable execution; stated over the logical run it is the real claim.
//
// claimscope:prohibition — the next sentence quotes the banned effect-scope phrasing in
// order to RULE IT OUT for this file, which is the opt-out's intended use. Recorded
// rather than quietly reworded: the claim-scope guard caught this file on the full-suite
// run while its own scoped run was 3/3 PASS, which is the 118 lesson exactly.
//
// The property is PRECEDENCE (DEC-M23-NAMING). It is NOT "V verifies D's effect" and
// nothing here should be read as proving that: V may legitimately run before D.

// ---------------------------------------------------------------------------
// T2 — the oracle
// ---------------------------------------------------------------------------

// barM23OracleCallSites is how many places in this package's suite invoke the oracle.
// It is the POPULATION BOUND printed in every report, and it is a const rather than a
// computed value because the report must state it even when nothing is parsing.
//
// It is pinned by TestBARM23_PopulationBoundIsAccurate, which counts the call sites
// mechanically. That guard is the whole point: a hand-maintained number quietly drifting
// out of agreement with the thing it describes is this milestone's defining defect, and
// stating a population bound in prose while the population moved would be an instance of
// it inside the remedy for it.
const barM23OracleCallSites = 11

// barM23Arm is one clause of BAR-M23 together with whether this milestone can
// evaluate it at all. Availability is part of the oracle's OUTPUT and not merely of
// its doc comment, because 119-121 cite this oracle as their exit gate and a green
// from an instrument must never be readable as bar-green for a clause the instrument
// cannot see. 117's T10 was specified with exactly this requirement and it is the
// half that was dropped.
type barM23Arm struct {
	Clause    string
	Available bool
	Basis     string
}

// barM23Report is what the oracle returns. It carries the arm availability
// declaration unconditionally, and its ONLY verdict accessor returns the scope of the
// verdict alongside it, so a caller cannot obtain a verdict without also obtaining the
// scope of that verdict.
//
// That sentence used to sit here as an assertion while the type shipped a `Violated()
// bool` that handed back a bare scopeless boolean (118B-12). It is now enforced by the
// compiler — see Passed — and by TestBARM23_NoScopelessVerdictAccessor.
type barM23Report struct {
	Boundaries int
	Arms       []barM23Arm
	Violations []string
	// Unresolved holds boundaries the oracle CANNOT DECIDE. A report with unresolved
	// entries is NOT A PASS — Passed requires this slice empty, which is the mechanism
	// behind that sentence rather than a restatement of it (118B-12). See barM23Invoked
	// for why a suspendable sink lands here.
	Unresolved []string
}

// Passed reports whether this report is a PASS, together with the SCOPE of that verdict.
//
// # The two-value shape is the mechanism, not a convenience
//
// Go forbids a two-value call in a boolean context, so `if !rep.Passed()` and
// `require.False(t, rep.Passed())` DO NOT COMPILE. A caller must bind the scope to obtain
// the verdict. That is what makes barM23Report's doc comment true by construction instead
// of by assertion, and it is why this replaced `Violated() bool` outright rather than
// gaining a sibling: leaving the scopeless accessor in place would have left the sentence
// false and the compiler silent.
//
// # A pass requires Unresolved empty, and that is the whole of 118B-12
//
// Violated() was `len(r.Violations) > 0` and never consulted Unresolved, so
// `!rep.Violated()` was TRUE on an all-UNDECIDED report — the state the report's own text
// calls "not a pass". The 118B-1 remedy therefore changed the string an undecided boundary
// produces and not the verdict any caller computed, and was inert at three of its five
// call sites. TestBARM23_SuspendableSinkIsUndecidedNotGreen now asserts this predicate on
// a real all-undecided report.
//
// # What it still is NOT
//
// It is deliberately not named Green: a pass here is a pass on the EVALUATED arms over the
// call sites this suite hands the oracle, and the returned scope is what says so. The
// scope string is the sole channel by which the bounds reach a caller — barM23Bounds
// records why there is no other.
// # A report over ZERO boundaries is NOT a pass
//
// The Boundaries conjunct is an ANTI-VACUITY FLOOR on the verdict itself, and qa found
// this file already arguing for it: Unresolved blocks a pass because an oracle handed a
// journal it cannot read has established nothing, and a DAG that declares no boundary
// establishes nothing by the identical argument. Without the floor, quantifying over the
// empty set returns true — vacuously, and indistinguishably from a real green.
//
// It is not academic. Phase 119's success criterion is "oracle green over N generated
// cases": a generator calling THIS accessor in a loop. Every generated DAG that happens to
// declare no boundary would have counted as a passing case, in the phase this oracle exists
// to gate.
func (r barM23Report) Passed() (bool, string) {
	return r.Boundaries > 0 && len(r.Violations) == 0 && len(r.Unresolved) == 0, r.String()
}

// String renders the verdict WITH its scope. The arm table is emitted whether or not
// there are violations — that is the point of it.
func (r barM23Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "BAR-M23 oracle: %d declared boundary(ies) quantified over\n", r.Boundaries)
	for _, a := range r.Arms {
		state := "NOT EVALUATED (no referent in M23)"
		if a.Available {
			state = "EVALUATED"
		}
		fmt.Fprintf(&b, "  arm [%s] %s — %s\n", state, a.Clause, a.Basis)
	}
	if r.Boundaries == 0 {
		// Say WHY, on the render, for the same reason the unreadable-status message names
		// its case: a caller who gets a non-pass needs to know it quantified over nothing
		// rather than that something failed.
		b.WriteString("  verdict: NO DECLARED BOUNDARY — this report quantifies over the EMPTY SET " +
			"and is NOT a pass. A green here would be vacuous, and a generator looping on this " +
			"accessor cannot tell a vacuous green from a real one.\n")
	} else if len(r.Violations) == 0 {
		if len(r.Unresolved) == 0 {
			b.WriteString("  verdict: no counterexample on the EVALUATED arms above. " +
				"This is NOT bar-green for the not-evaluated arms.\n")
		} else {
			fmt.Fprintf(&b, "  verdict: no counterexample, but %d boundary(ies) UNDECIDED — "+
				"this is NOT a pass.\n", len(r.Unresolved))
		}
	} else {
		fmt.Fprintf(&b, "  verdict: %d VIOLATION(S)\n", len(r.Violations))
		for _, v := range r.Violations {
			fmt.Fprintf(&b, "    - %s\n", v)
		}
	}
	for _, u := range r.Unresolved {
		fmt.Fprintf(&b, "    ? UNDECIDED: %s\n", u)
	}
	b.WriteString(barM23Bounds())
	return b.String()
}

// barM23Bounds is appended to every RENDER of the report, green or red, so a caller who
// holds the verdict string holds the limits on it. Passed returns that string alongside
// the verdict, which is what makes the two inseparable.
//
// 🔴 THIS COMMENT USED TO SAY "printed on EVERY report", AND THAT WAS FALSE (118B-9,
// found by independent review). Nothing this file prints is visible on the invocation the
// Makefile and CI actually run, which is the invocation 119-121 will cite.
//
// # Measured, with both controls, because the obvious remedy is also false
//
// On go1.25.1, `go test` DISCARDS a passing package's entire binary output. Not just
// t.Logf: fmt.Println, os.Stdout, os.Stderr and a TestMain writing after m.Run all print
// ZERO times on an all-passing non-verbose run. Under -v all of them appear; with a
// FAILING sibling test in the same package the stdout writes appear while a passing test's
// t.Logf still does not. So "move the render out of t.Logf so it survives" would have
// looked exactly like a fix and been none — this is a property of the toolchain, not of
// where the render was placed.
//
// # What actually follows, which is the remedy
//
//   - The STRING channel carries these bounds wherever the string goes, and Passed makes
//     obtaining a verdict WITHOUT that string impossible — see Passed and
//     TestBARM23_NoScopelessVerdictAccessor. That is the channel a caller in code uses.
//   - The OPERATOR channel is `make bar-oracle`, which runs this selection with -v so the
//     bounds print by construction. TestBARM23_BoundsHaveAnInvocationThatPrintsThem reds
//     if that target loses its -v or its selection.
//   - 🔴 A bare `make test` or CI green prints these bounds ZERO times, and no mechanism
//     available here changes that. A phase citing "oracle green" from a package `ok` line
//     HAS NOT SEEN THEM. That residual is recorded here rather than left in a mailbox —
//     which is the rule this comment was written to apply, and then did not.
func barM23Bounds() string {
	return fmt.Sprintf(`  🔴 BOUNDS ON THE ABOVE, so it cannot be over-read:
    - POPULATION IS NOT ENUMERATED. This oracle checks the DAGs a caller hands it —
      %d call sites in this package's suite — and NOT "every DAG the library accepts",
      which is what BAR-M23 clause 1 actually quantifies over. The DETECTOR is bitten:
      a seeded violation reds it. The POPULATION is not enumerated at all. Those are
      different claims and only the first is proven. A phase citing "oracle green" is
      citing a green over those call sites, not over the package.
    - INVOCATION IS INFERRED FROM STATUS, and a false is not sound on its own. A
      SUSPENDABLE sink parks BY entering its action, so its non-terminal status cannot be
      told from never-having-run (118B-1, found by review). Those boundaries are reported
      as UNDECIDED above rather than skipped, and an undecided boundary is NOT a pass.
      This matters because DEC-M23-VB08-R3 rules waitForCondition and waitForSignal
      ELIGIBLE as V/S, so the blind spot sits where the predicate is most permissive.
      Sink-side only: V is tested != Completed, so a parked V still reports.
    - AN UNPERSISTED S INVOCATION IS INVISIBLE. An S whose action ran but whose status
      never persisted reads as uninvoked. That is sound for the logical-run form — the
      invocation died with the process and S re-runs on resume — but the oracle can
      UNDER-REPORT an S that genuinely ran and then vanished with the process.
    - NO RECURSION INTO CHILD DAGs. *DAG satisfies Action, so a built child DAG nests
      under WithAction, unbounded (F118-ENG-01, open). Boundaries declared inside a
      nested child are NOT quantified over here.
    - A COMPENSATED BOUNDARY IS REPORTED HONOURED. If V ran to Completed and a saga
      rollback afterwards undid what V did, this report says honoured, not violated.
      Clause 1 is PRECEDENCE — that V RAN before S was invoked — so a rollback rewriting
      V's status does not un-run it. Nothing here speaks to what V's run means for D's
      work, and a reader must not take an honoured boundary as saying the undo was
      harmless (DEC-M23-NAMING; the compensation-edge residual is F118-COMP-01).
    - THE JOURNAL IS WRITABLE BY ANY ACTION, AND THIS ORACLE READS OCCURRENCE FROM IT.
      SetNodeStatus is exported and every Action is handed the same *WorkflowData
      (action.go:19), so an action can write any node's status. MEASURED at d19e6f1 through
      the fully public API on a legal WithBoundary chain: D's action set V's status to
      Completed, V's own action then ran ZERO times because a Completed node does not
      re-execute, S was invoked, and this oracle reported the boundary HONOURED with zero
      violations. A SILENT FALSE GREEN over a boundary that was never honoured, and the
      same family as 118B-6 — a journal treated as a record of what happened. M23 cannot
      detect it; the oracle's only input is that journal. The engine/consumer data split is
      M24 (P1-1, whole-project deep-dive).
`, barM23OracleCallSites)
}

// barM23StatusEvidence is what a node's CURRENT status proves about its PAST.
//
// The oracle asks two HAS-THIS-EVER-HAPPENED questions — was S's action invoked, did V
// run to Completed — and a saga rollback REWRITES the status it reads them from after the
// fact (118B-6, reproduced twice by independent review, once through the fully public
// API). Adding the one missing status to a boolean switch would fix one instance, leave
// the other, and leave the next status that overwrites Completed to bite identically, so
// the mapping is TOTAL and its default is FAIL-SAFE instead:
//
//   - everCompleted is the durable has-ever-run record, and it exists: node.go:49-67
//     authors Compensated and CompensationFailed as a declared pair, each reached ONLY
//     from Completed. For those two the current status IS proof of the past event. No
//     new journal field is needed and none was added.
//   - classified=false is what an UNRECOGNISED status returns, and the oracle reports
//     such a boundary UNDECIDED rather than vacuously passing it. That is the property:
//     a status the mapping has never seen must not read as never-having-run.
//   - TestBARM23_StatusMappingIsTotal enumerates node.go's NodeStatus constants
//     MECHANICALLY and reds when the taxonomy grows one this mapping does not classify,
//     so the fail-safe default is a backstop rather than the whole defence.
//
// A compensated V still SATISFIES clause 1. The property is PRECEDENCE (DEC-M23-NAMING;
// this file's header states what it must never be read as, and states it once): V ran to
// Completed before S was invoked, and a rollback that later undoes what V did does not
// un-run it. Reporting that boundary as a violation — which 047e55a did — was a false
// positive on an honoured boundary.
type barM23StatusEvidence struct {
	// classified is false for a status this mapping does not recognise. The other two
	// fields are then meaningless and must not be read as negatives.
	classified bool
	// invoked means the node's action was ENTERED at some point in this logical run.
	invoked bool
	// everCompleted means the node's action ran to Completed at some point in this
	// logical run, EVEN IF a rollback has since rewritten the status.
	everCompleted bool
}

func barM23Evidence(st NodeStatus) barM23StatusEvidence {
	switch st {
	case Completed:
		return barM23StatusEvidence{classified: true, invoked: true, everCompleted: true}
	case Compensated, CompensationFailed:
		// Both are reached ONLY from Completed (node.go:51, :58) and are authored as a
		// declared pair. The rollback rewrote the status; it did not un-run the action.
		return barM23StatusEvidence{classified: true, invoked: true, everCompleted: true}
	case Running, Failed:
		// Entered its action; never reached Completed.
		return barM23StatusEvidence{classified: true, invoked: true}
	case Pending, Skipped, Bypassed, Waiting:
		// Never entered its action — SUBJECT to the suspendable caveat below, which the
		// oracle applies and this mapping deliberately does not: a suspendable node parks
		// BY entering its action, and that fact is a property of the NODE, not of the
		// status. Encoding it here would make the status mapping lie about kinds it
		// cannot see.
		return barM23StatusEvidence{classified: true}
	default:
		return barM23StatusEvidence{}
	}
}

// barM23Invoked reports whether a node's action was invoked, INFERRED FROM STATUS. It is
// the sink-side projection of barM23Evidence, kept as a named predicate because the
// crash-window harness's skip gate and 118B-1's regression both key on it.
//
// A true is sound. A FALSE IS NOT SOUND ON ITS OWN and callers must consult the node's
// suspendable flag before trusting one — see barM23Oracle, which does.
//
// 🔴 THIS COMMENT USED TO ASSERT THE OPPOSITE, CONFIDENTLY AND WRONGLY (118B-1, found
// by independent review). It read "Pending/Skipped/Bypassed/Waiting never entered the
// action." A suspendable node reaches its parked state BY ENTERING ITS ACTION: it runs,
// evaluates, returns ErrSuspended, and its status is left non-terminal precisely so a
// resume re-runs it. The action was invoked and the status says otherwise.
//
// Measured, not argued: a sink built with AddWaitForCondition parked at pending with its
// predicate having been invoked once, and the oracle reported ZERO violations. The
// reason this rates MAJOR rather than a note is that the wrong reason was stated
// confidently enough that a reader would not re-derive it -- and the blindness lands
// exactly where DEC-M23-VB08-R3 is MOST permissive, since waitForCondition and
// waitForSignal are ruled ELIGIBLE as V/S. A clause-1 green that silently drops
// suspendable sinks is a gate that can pass a real violation, and 119-121 cite it.
//
// The blindness is SINK-SIDE ONLY: V is tested != Completed, so a parked verifier still
// counts as not-Completed and its violation is still reported.
func barM23Invoked(st NodeStatus) bool { return barM23Evidence(st).invoked }

// barM23Unreadable renders WHY an operand's status could not be classified, and it
// distinguishes the two cases rather than collapsing them, because a reader who gets
// UNDECIDED needs to know which one they are looking at: the journal holds NO ENTRY for
// this node, or it holds one this mapping does not recognise. The first is the reachable
// case today — GetNodeStatus returns the empty NodeStatus and false for a node the journal
// has never seen — and a bare "sits at " with nothing after it would be the least useful
// sentence this report could print.
func barM23Unreadable(role, node string, st NodeStatus, recorded bool) string {
	if !recorded {
		return fmt.Sprintf("%s (%s) has NO ENTRY IN THE JOURNAL AT ALL — this logical run never "+
			"recorded a status for it, so the oracle cannot tell whether its action was invoked",
			role, node)
	}
	return fmt.Sprintf("%s (%s) sits at %v, which barM23Evidence does not classify", role, node, st)
}

// barM23Oracle is the BAR-M23 oracle. It quantifies over every DECLARED boundary on
// the graph and reports every execution in which S's action was invoked without V
// having reached Completed in the same logical run.
//
// It takes its declarations from the *DAG it is handed and NEVER from the
// __boundaries__ projection (see the requirement recorded at encodeBoundaryEnvelope).
// dag.boundaries is in the RUN-CONSTANCY class — set only by build(), so a resume
// re-derives it from the rebuilt graph. The projection is a possibly-stale,
// operator-editable snapshot; DEC-M23-VB01-SLOT accepted that staleness precisely
// because M23 has no out-of-process reader, and reading it back here would both
// invalidate that acceptance and make the snapshot authoritative over the graph.
//
// data is the logical run's journal. Because a resumed run loads the persisted
// statuses, a V that completed in run 1 still reads as HAVING COMPLETED to the oracle in
// run 2 — which is what makes the across-resume form of the bar checkable at all.
//
// That sentence used to say "is still Completed", and that was false under saga rollback
// (118B-6): a compensated V sits at Compensated, not Completed. It is stated over
// barM23Evidence's everCompleted rather than over the literal status precisely because
// the status is REWRITABLE and the question is not.
func barM23Oracle(dag *DAG, data *WorkflowData) barM23Report {
	rep := barM23Report{
		Boundaries: len(dag.boundaries),
		Arms: []barM23Arm{
			{
				Clause:    "1: whenever S's action is invoked, V ran to Completed in the same logical run",
				Available: true,
				Basis: "declarations read from the *DAG (RUN-CONSTANCY, never the projection); " +
					"statuses read from the WorkflowData journal, which spans resumes",
			},
			{
				Clause:    "2 (BAR-M24): the authorization input S consumes was written by V",
				Available: false,
				Basis: "NO REFERENT UNTIL M24 — no durable endorsement exists for the oracle to read, " +
					"so this arm is not evaluated and a green above says nothing about it",
			},
		},
	}
	for _, d := range dag.boundaries {
		sinkSt, sinkRecorded := data.GetNodeStatus(d.sink)
		sink := barM23Evidence(sinkSt)
		if !sink.classified {
			// A status this mapping cannot read. It must NOT read as never-having-run —
			// that is the exact shape of 118B-1 and 118B-6, one layer up.
			rep.Unresolved = append(rep.Unresolved, fmt.Sprintf(
				"boundary (D=%s, V=%s, S=%s): %s. CLAUSE 1 IS NOT DECIDED here — this is not a "+
					"pass. If the status exists but is unclassified, classify it in barM23Evidence "+
					"(TestBARM23_StatusMappingIsTotal should have caught that first)",
				d.doer, d.verifier, d.sink, barM23Unreadable("S", d.sink, sinkSt, sinkRecorded)))
			continue
		}
		if !sink.invoked {
			// A non-terminal status does NOT mean the action was not entered when the node
			// is suspendable: it parks BY running. The journal cannot tell "S ran and
			// parked" from "S never ran", so this boundary is UNDECIDED rather than
			// vacuous, and it is reported instead of silently skipped (118B-1).
			if n, ok := dag.GetNode(d.sink); ok && n.suspendable {
				rep.Unresolved = append(rep.Unresolved, fmt.Sprintf(
					"boundary (D=%s, V=%s, S=%s): S is a SUSPENDABLE kind sitting at %v. It reaches a "+
						"non-terminal status BY entering its action and returning ErrSuspended, so the "+
						"journal cannot distinguish S-ran-and-parked from S-never-ran. CLAUSE 1 IS NOT "+
						"DECIDED here — this is not a pass",
					d.doer, d.verifier, d.sink, sinkSt))
				continue
			}
			continue // S never entered its action; clause 1 is vacuous for this boundary
		}
		verifierSt, vRecorded := data.GetNodeStatus(d.verifier)
		verifier := barM23Evidence(verifierSt)
		if !verifier.classified {
			rep.Unresolved = append(rep.Unresolved, fmt.Sprintf(
				"boundary (D=%s, V=%s, S=%s): S was invoked (status %v) but %s. CLAUSE 1 IS NOT "+
					"DECIDED here — this is not a pass",
				d.doer, d.verifier, d.sink, sinkSt,
				barM23Unreadable("V", d.verifier, verifierSt, vRecorded)))
			continue
		}
		if !verifier.everCompleted {
			rep.Violations = append(rep.Violations, fmt.Sprintf(
				"boundary (D=%s, V=%s, S=%s): S was invoked (status %v) but V is %v and never reached "+
					"Completed in this logical run",
				d.doer, d.verifier, d.sink, sinkSt, verifierSt))
		}
	}
	return rep
}

// ---------------------------------------------------------------------------
// T1 — the crash-window harness
// ---------------------------------------------------------------------------

// buildBoundaryResumeDAG generates a boundary-carrying workflow:
//
//	seed -> doer -> verifier -> sink -> after
//
// with WithBoundary(doer, verifier, sink) actually declared, so build() validates the
// root-anchored dominance predicate and the oracle downstream has a real referent
// rather than a test-only stand-in. Every action is a shipped ActionFunc: per 118-D7
// the action clause speaks only for in-package kinds, so a harness that defined its
// own action type would be unreachable-by-instrument rather than correct-by-criterion.
//
// verifier is in a strictly earlier level than sink whenever the boundary holds, which
// is what makes "crash after V's level, then resume" the shape of the crash window.
func buildBoundaryResumeDAG(t *testing.T, id string, c *execCounter) *DAG {
	t.Helper()
	wb := NewWorkflowBuilder().WithWorkflowID(id)
	wb.AddStartNode("seed").WithAction(resumeCountAction(c, "seed"))
	wb.AddNode("doer").DependsOn("seed").WithAction(resumeCountAction(c, "doer"))
	wb.AddNode("verifier").DependsOn("doer").WithAction(resumeCountAction(c, "verifier"))
	wb.AddNode("sink").DependsOn("verifier").WithAction(resumeCountAction(c, "sink"))
	wb.AddNode("after").DependsOn("sink").WithAction(resumeCountAction(c, "after"))
	wb.WithBoundary("doer", "verifier", "sink")
	dag, err := wb.Build()
	require.NoError(t, err, "the declared boundary must satisfy the root-anchored predicate")
	return dag
}

// TestBARM23_CrashWindow_VCompletedCrashResumeSInvoked is the harness: V completed ->
// crash -> resume -> S invoked, level-granular, driven over a range of crash levels so
// it is a generator rather than a single fixture.
//
// The vacuity this guards against is the one this project has hit before: a resume test
// whose node is already Completed is SKIPPED and proves nothing. So the arms assert the
// crash window was ENTERED — sink's invocation count is 0 after the crash and 1 after
// the resume, and verifier's count stays at 1 ACROSS the resume. That last assertion is
// the whole reason BAR-M23 is stated over the logical run: on resume, S is invoked in a
// process in which V demonstrably never ran.
func TestBARM23_CrashWindow_VCompletedCrashResumeSInvoked(t *testing.T) {
	// Levels: seed=0, doer=1, verifier=2, sink=3, after=4. Crashing after 3 checkpoints
	// leaves verifier persisted and sink unrun; the loop brackets that so an off-by-one
	// in checkpoint accounting surfaces as a named skip, never as a silent pass.
	entered := 0
	for _, crashLevel := range []int{2, 3, 4} {
		t.Run(fmt.Sprintf("crashAfter%d", crashLevel), func(t *testing.T) {
			store := NewInMemoryStore()
			id := fmt.Sprintf("barm23-window-%d", crashLevel)

			// Phase 1 gets its OWN counter, and phase 2 a fresh one, because a process
			// death loses in-process state. crashAfterLevels errors at the checkpoint
			// AFTER a level's actions have already run, so an invocation that was never
			// persisted did not survive the crash — counting it across the resume would
			// model a crash that does not happen. The window is therefore defined on the
			// PERSISTED JOURNAL, which is the only thing that actually crosses the gap.
			c1 := newExecCounter()
			data1 := NewWorkflowData(id)
			err := buildBoundaryResumeDAG(t, id, c1).Execute(
				withCheckpoint(context.Background(), crashAfterLevels(store, crashLevel)), data1)
			require.Error(t, err, "the simulated crash must surface as an error")

			persisted, lerr := store.Load(id)
			require.NoError(t, lerr)
			vSt, _ := persisted.GetNodeStatus("verifier")
			sSt, _ := persisted.GetNodeStatus("sink")
			if vSt != Completed || barM23Invoked(sSt) {
				t.Skipf("not the crash window: persisted verifier=%v sink=%v "+
					"(this crash level brackets the window rather than entering it)", vSt, sSt)
			}

			// This IS the crash window: V durably Completed, S not yet durably invoked.
			entered++

			// Pre-resume the bar is vacuous for this boundary — S was never invoked.
			pre := barM23Oracle(buildBoundaryResumeDAG(t, id, c1), persisted)
			prePassed, preScope := pre.Passed()
			require.True(t, prePassed, "pre-resume, S uninvoked, clause 1 is vacuous:\n%s", preScope)

			// Phase 2: resume clean from the persisted journal, with a FRESH counter — it
			// counts only what run 2's process actually did.
			c2 := newExecCounter()
			data2, lerr2 := store.Load(id)
			require.NoError(t, lerr2)
			resumed := buildBoundaryResumeDAG(t, id, c2)
			require.NoError(t, resumed.Execute(
				withCheckpoint(context.Background(), func(s *WorkflowData) error {
					return store.SaveCheckpoint(s)
				}), data2))

			// THE POINT OF THE WHOLE HARNESS: in run 2's process, S was invoked and V was
			// not — a Completed node does not re-execute. Stated over one Execute the bar
			// is violated here by CORRECT durable execution; stated over the logical run it
			// holds. This is the assertion that makes that distinction real rather than
			// asserted, and it is keyed on an invocation COUNT because an error-based probe
			// reads identically on both sides.
			assert.Equal(t, 1, c2.get("sink"), "S must be invoked on the resume")
			assert.Equal(t, 0, c2.get("verifier"), "V must NOT re-run on resume (Completed does not re-execute)")
			assertNodeStatus(t, data2, "sink", Completed)
			assertNodeStatus(t, data2, "after", Completed)

			// And the oracle, over the logical run, sees V Completed despite it never having
			// run in this process.
			rep := barM23Oracle(resumed, data2)
			require.Equal(t, 1, rep.Boundaries, "the oracle must quantify over the declared boundary")
			passed, scope := rep.Passed()
			assert.True(t, passed, "clause 1 holds across the crash window:\n%s", scope)
			t.Logf("oracle over the crash window:\n%s", scope)
		})
	}
	require.NotZero(t, entered, "NO crash level entered the window — the harness proved nothing")
}

// TestBARM23_CrashWindow_ViolatingResumeIsCaught is the crash-window harness's NEGATIVE
// CONTROL (118B-8), and without it the harness above proves less than it appears to.
//
// # What was missing, measured rather than argued
//
// The arm above asserts the oracle reports NO violation across a correct crash and resume.
// It never established that a violation ARISING THROUGH the crash/resume path would be
// caught. Two mutations showed what that costs: with the oracle made structurally unable
// to append any violation, the entire crash-window test passed IDENTICALLY TO GREEN — two
// skips and one pass — and only the direct-seed bite failed. The whole falsification power
// of the phase rested on one test that seeds dag.boundaries and never crashes or resumes.
//
// That matters because every oracle blind spot found in this phase lives in the
// status→invocation mapping (118B-1 suspendable, 118B-6 compensation statuses), and the
// harness driving the real path never fed the oracle an input of that class. Worse, the
// skip gate above consults barM23Invoked to decide what the window IS, so a defect there
// MOVES THE WINDOW instead of reddening an arm.
//
// # Why this arm asserts rather than skips
//
// The positive arm sweeps crash levels and skips the ones that bracket the window, which
// is correct there: the skips are the evidence that level 3 is a measured window rather
// than a chosen constant. Here a skip would silently delete the only falsifying input in
// the crash path, which is the vacuity this whole file exists to refuse. So the window is
// ASSERTED, and a crash-accounting change surfaces as a red naming the statuses it found.
//
// # The declaration is seeded and the VIOLATION is not
//
// build()'s root-anchored predicate refuses a graph in which S is reachable without
// passing V, so a genuinely violating boundary cannot be declared through WithBoundary —
// same constraint and same remedy as TestBARM23_Oracle_Bites. What is NOT seeded is
// everything the arm is about: the crash, the resume, S's invocation in run 2, and V's
// status. Those are produced by driving the same helpers the positive arm uses.
func TestBARM23_CrashWindow_ViolatingResumeIsCaught(t *testing.T) {
	const id = "barm23-window-violating"
	store := NewInMemoryStore()

	// seed=0, doer=1, {verifier, sink}=2. verifier and sink are SIBLINGS, so nothing
	// orders them and sink runs whatever verifier does — the graph itself is the genuine
	// clause-1 violation. Crashing after 2 levels leaves seed and doer persisted and level
	// 2 unrun, which is the same window shape the positive arm enters.
	build := func(c *execCounter) *DAG {
		wb := NewWorkflowBuilder().WithWorkflowID(id)
		wb.AddStartNode("seed").WithAction(resumeCountAction(c, "seed"))
		wb.AddNode("doer").DependsOn("seed").WithAction(resumeCountAction(c, "doer"))
		wb.AddNode("verifier").DependsOn("doer").WithAction(ActionFunc(func(_ context.Context, _ *WorkflowData) error {
			c.inc("verifier")
			return errors.New("verifier failed")
		}))
		wb.AddNode("sink").DependsOn("doer").WithAction(resumeCountAction(c, "sink"))
		dag, err := wb.Build()
		require.NoError(t, err)
		dag.boundaries = []boundaryDecl{{doer: "doer", verifier: "verifier", sink: "sink"}}
		return dag
	}

	// Phase 1: crash after level 1's checkpoint, before level 2 is persisted. Its own
	// counter, because a process death loses in-process state.
	c1 := newExecCounter()
	data1 := NewWorkflowData(id)
	require.Error(t, build(c1).Execute(
		withCheckpoint(context.Background(), crashAfterLevels(store, 2)), data1),
		"the run must not complete: it is crashed and its verifier fails")

	persisted, lerr := store.Load(id)
	require.NoError(t, lerr)
	vSt, _ := persisted.GetNodeStatus("verifier")
	sSt, _ := persisted.GetNodeStatus("sink")
	require.False(t, barM23Invoked(sSt),
		"the crash must land BEFORE S is durably invoked, or the resume is not what invokes it "+
			"(persisted verifier=%v sink=%v)", vSt, sSt)
	require.NotEqual(t, Completed, vSt, "V must not be persisted Completed; got %v", vSt)

	// Phase 2: resume clean from the persisted journal with a FRESH counter, which counts
	// only what run 2's process actually did.
	c2 := newExecCounter()
	data2, lerr2 := store.Load(id)
	require.NoError(t, lerr2)
	resumed := build(c2)
	require.Error(t, resumed.Execute(
		withCheckpoint(context.Background(), func(s *WorkflowData) error {
			return store.SaveCheckpoint(s)
		}), data2), "the resumed run fails: V fails")

	// THE FALSIFYING INPUT, and it arose in run 2 rather than being seeded: S's action was
	// invoked on the RESUME, in a logical run in which V never reached Completed.
	require.Equal(t, 1, c2.get("sink"), "S must be invoked on the resume, or this proves nothing")
	rvSt, _ := data2.GetNodeStatus("verifier")
	require.NotEqual(t, Completed, rvSt, "V must never have completed in this logical run; got %v", rvSt)

	rep := barM23Oracle(resumed, data2)
	require.Equal(t, 1, rep.Boundaries, "the oracle must quantify over the declared boundary")
	passed, scope := rep.Passed()
	require.False(t, passed,
		"a violation arising THROUGH the crash/resume path must be caught. A green here means the "+
			"harness cannot detect any defect of the class every finding in this phase belongs to:\n%s", scope)
	require.Len(t, rep.Violations, 1, "it must red as a VIOLATION, not as undecided:\n%s", scope)
	assert.Contains(t, scope, "S was invoked", "the message must name what went wrong")
	t.Logf("violating resume, oracle red (persisted at crash: verifier=%v sink=%v):\n%s", vSt, sSt, scope)
}

// ---------------------------------------------------------------------------
// T2 — the oracle must be shown to FAIL
// ---------------------------------------------------------------------------

// TestBARM23_Oracle_Bites seeds a REAL clause-1 violation and asserts the oracle reds
// on it. "The oracle runs" is not the criterion; "the oracle can fail" is.
//
// The violation cannot be seeded through WithBoundary, because build()'s root-anchored
// predicate REFUSES a graph in which S is reachable without passing V — the instrument
// would be unreachable-by-construction. This test is in package workflow, so it declares
// the triple directly on dag.boundaries after build(), which reaches the oracle's arm
// without weakening build(). The graph itself is the genuine violation: verifier and
// sink are siblings, so sink runs whatever verifier does.
func TestBARM23_Oracle_Bites(t *testing.T) {
	c := newExecCounter()
	wb := NewWorkflowBuilder().WithWorkflowID("barm23-bite")
	wb.AddStartNode("seed").WithAction(resumeCountAction(c, "seed"))
	// verifier FAILS, so it never reaches Completed.
	wb.AddNode("verifier").DependsOn("seed").WithAction(ActionFunc(func(_ context.Context, _ *WorkflowData) error {
		c.inc("verifier")
		return errors.New("verifier failed")
	}))
	// sink is a SIBLING of verifier, not a descendant — nothing orders them.
	wb.AddNode("sink").DependsOn("seed").WithAction(resumeCountAction(c, "sink"))
	dag, err := wb.Build()
	require.NoError(t, err)

	// build() would refuse this declaration; the oracle's subject is the declaration, so
	// the seed is applied here rather than through WithBoundary.
	dag.boundaries = []boundaryDecl{{doer: "seed", verifier: "verifier", sink: "sink"}}

	data := NewWorkflowData("barm23-bite")
	// The run errors because V fails; that error is incidental to the oracle's subject,
	// which is the STATUSES it leaves behind. It is asserted rather than discarded so the
	// seed cannot silently stop failing.
	require.Error(t, dag.Execute(context.Background(), data), "the seeded run errors: V fails")

	// CONFIRM THE MUTATION WAS REACHED. Phase 118's blocker returned err == nil on both
	// sides of the defect, so an error-based probe was structurally blind to it. The
	// discriminating signal here is the invocation count: if S never ran, a red from the
	// oracle would mean nothing.
	require.Equal(t, 1, c.get("sink"), "the seed is only a violation if S was ACTUALLY invoked")
	vSt, _ := data.GetNodeStatus("verifier")
	require.NotEqual(t, Completed, vSt, "the seed requires V NOT Completed; got %v", vSt)

	rep := barM23Oracle(dag, data)
	passed, msg := rep.Passed()
	require.False(t, passed, "the oracle MUST fail on a seeded violation:\n%s", msg)
	// A failed Passed() is not yet the right failure: an all-UNDECIDED report also fails
	// it. The seeded violation must show up as a VIOLATION.
	require.Len(t, rep.Violations, 1, "the seed must red as a violation, not as undecided:\n%s", msg)

	// Read the failure MESSAGE, not just the fact of failure.
	assert.Contains(t, msg, "S was invoked", "the message must name what went wrong")
	assert.Contains(t, msg, "V=verifier", "the message must name the offending boundary")
	assert.Contains(t, msg, "1 VIOLATION(S)")

	// 118B-14 — THE BOUNDS ON A REAL RENDER, RED PATH. The sentence being enforced is
	// "printed on every report, GREEN OR RED", so asserting only the green path would
	// re-create the defect one branch over: String() takes a different arm when Violations
	// is non-empty, and a refactor could drop the bounds from this path alone.
	assert.Contains(t, msg, "BOUNDS ON THE ABOVE",
		"a RED render carries the bounds too — a violation report is the one a reader is most "+
			"likely to quote (118B-14)")
	assert.Contains(t, msg, "POPULATION IS NOT ENUMERATED",
		"a red verdict must not be readable as a bar-wide result either")
	t.Logf("SEEDED VIOLATION, oracle red:\n%s", msg)
}

// TestBARM23_Oracle_DeclaresArmAvailability pins the requirement that the oracle's OWN
// OUTPUT states what it did and did not evaluate. 119-121 cite this oracle as their exit
// gate; without this, "oracle green" reads as bar-green for the V-written clause, which
// has no referent until M24 and which this oracle cannot see.
func TestBARM23_Oracle_DeclaresArmAvailability(t *testing.T) {
	c := newExecCounter()
	dag := buildBoundaryResumeDAG(t, "barm23-arms", c)
	data := NewWorkflowData("barm23-arms")
	require.NoError(t, dag.Execute(context.Background(), data))

	rep := barM23Oracle(dag, data)
	passed, out := rep.Passed()
	require.True(t, passed, "clean run holds clause 1:\n%s", out)
	require.Equal(t, 1, rep.Boundaries,
		"pin the fixture's population: a green from Passed() now REQUIRES a non-zero boundary count, "+
			"so this arm would otherwise not distinguish a real green from a vacuous one")

	assert.Contains(t, out, "NOT EVALUATED (no referent in M23)",
		"the unavailable arm must be declared in the OUTPUT, not only in a doc comment")
	assert.Contains(t, out, "BAR-M24", "the not-evaluated arm must be named")
	assert.Contains(t, out, "NOT bar-green",
		"a green verdict must carry its own scope limit")

	// 118B-14 — THE BOUNDS ON A REAL RENDER, GREEN PATH. Every other assertion on the
	// bounds in this file calls barM23Bounds() DIRECTLY, so it tests the helper and not the
	// report: removing the b.WriteString(barM23Bounds()) line from String() left all 15 arms
	// green. The evidence's noun was the helper; the claim's noun is the render.
	//
	// `out` here is what Passed() handed back, which is the only channel a caller in code
	// has, so asserting on it is asserting on the thing a caller actually receives. The two
	// bounds added last — the writable-journal one and compensated-boundary-honoured — have
	// NO other delivery channel at all, so without this a refactor drops them silently with
	// the API shape intact and every arm green.
	assert.Contains(t, out, "BOUNDS ON THE ABOVE",
		"the RENDERED verdict must carry the bounds, not merely be adjacent to a helper that "+
			"returns them (118B-14)")
	assert.Contains(t, out, "POPULATION IS NOT ENUMERATED",
		"the population bound must reach the caller through the render")
	assert.Contains(t, out, "THE JOURNAL IS WRITABLE BY ANY ACTION",
		"the forgeable-journal bound has no delivery channel except this render (P1-1)")

	// The green form must not be quotable as unqualified bar-green.
	require.Len(t, rep.Arms, 2)
	assert.True(t, rep.Arms[0].Available, "clause 1 is the arm M23 can evaluate")
	assert.False(t, rep.Arms[1].Available, "the V-written clause has no referent until M24")
	t.Logf("arm availability as the oracle reports it:\n%s", out)
}

// TestBARM23_PopulationBoundIsAccurate counts the oracle's call sites mechanically and
// asserts the number the report PRINTS agrees with them. Without this the population
// bound is prose: someone adds a fourth fixture, the report keeps claiming the old
// count, and the bound becomes false in exactly the direction that flatters it.
//
// go/parser rather than grep, for the reason the claim-scope guard uses it: a grep
// counts substrings, including ones in comments and strings, and this file's own
// comments mention barM23Oracle repeatedly. The compiler's view is the only one that
// counts CALLS.
func TestBARM23_PopulationBoundIsAccurate(t *testing.T) {
	// os.ReadDir + ParseFile rather than parser.ParseDir, which is deprecated as of Go
	// 1.25 — and its stated reason is a real bound, not a style note: it does not consider
	// build tags. Neither does this walk, so a file excluded by a build tag would still be
	// counted here, and this project has been bitten twice by a filename-shaped exclusion
	// (_arm is a GOARCH suffix).
	//
	// 118B-11: that bound used to be answered by "the oracle's own file is confirmed
	// present in TestGoFiles and absent from IgnoredGoFiles", which answers a DIFFERENT
	// question — only the absence half was mechanically enforced (by
	// TestSealed_NoGoFileIsSilentlyExcludedFromTheBuild) and the presence half by nothing,
	// while the real exposure is a call site in some OTHER build-tagged file inflating the
	// count and making this guard's own message instruct a human to RAISE the const,
	// printing a population larger than what executes. It is now enforced instead of
	// justified: the toolchain is asked which test files compile, below.
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	sites, files := 0, 0
	var siteFiles []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, e.Name(), nil, 0)
		require.NoError(t, perr, "parsing %s", e.Name())
		files++
		// The call FORMS this counts, and the one it declares it cannot, are documented on
		// barM23CountOracleCalls and witnessed against a synthetic source by
		// TestBARM23_ASTFiltersMatchTheFormsTheyClaim. Before that, a parenthesised call
		// walked through and the pin agreed with a number that was too small — and an
		// agreeing pin is more persuasive than a missing one.
		if n := barM23CountOracleCalls(f); n > 0 {
			sites += n
			siteFiles = append(siteFiles, e.Name())
		}
	}

	// Confirm the instrument REACHED something. A parser pointed at the wrong directory
	// returns zero files and zero calls, which would pass a bare equality against a
	// const that happened to be zero and would look exactly like a correct sweep.
	//
	// 118B-10: the floor is a COUNT, not NotZero, and it matches the sibling sweep
	// TestSealed_NoTestFileIsPlatformGatedByItsFilename. NotZero was satisfied by a sweep
	// that had narrowed to one file — and since every counted call site lives in the
	// oracle's own file, that narrowing would have been invisible: the count would still
	// have agreed with the const. An arm that cannot detect its own vacuity is 118-D1's
	// shape and it has recurred repeatedly in this milestone.
	require.Greater(t, files, 50,
		"the sweep walked only %d test file(s); it is BROKEN, not the tree — this package has many "+
			"times that, and a sweep narrowed to the oracle's own file would still find every call "+
			"site and still agree with the const", files)
	require.NotZero(t, sites, "the parser found no barM23Oracle calls — it is not seeing this file")

	// 118B-11, the presence half, ENFORCED. The walk above cannot see build tags, so ask
	// the toolchain which test files it actually compiles and require every file the count
	// came from to be one of them. A call site in a build-excluded file would otherwise
	// inflate the printed population above what runs — an error in the flattering
	// direction, and the direction a bound must never err in.
	out, lerr := exec.Command("go", "list", "-f",
		"{{range .TestGoFiles}}{{.}}\n{{end}}{{range .XTestGoFiles}}{{.}}\n{{end}}", ".").Output()
	require.NoError(t, lerr, "go list must run; without it this guard reports nothing and looks green")
	compiled := map[string]bool{}
	for _, f := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if f = strings.TrimSpace(f); f != "" {
			compiled[f] = true
		}
	}
	require.Greater(t, len(compiled), 50,
		"go list reported only %d compiled test files — the query is broken, not the tree", len(compiled))
	var excluded []string
	for _, f := range siteFiles {
		if !compiled[f] {
			excluded = append(excluded, f)
		}
	}
	sort.Strings(excluded)
	require.Empty(t, excluded,
		"barM23Oracle call site(s) counted in file(s) the build EXCLUDES: %v\n"+
			"The walk does not consider build tags, so those calls inflate the population this "+
			"oracle PRINTS above the population that actually executes. Either remove the build "+
			"constraint from those files or stop counting them — do NOT raise the const, which is "+
			"what this guard's equality message would otherwise tell you to do.", excluded)

	require.Equal(t, barM23OracleCallSites, sites,
		"the population bound printed in every report says %d call sites but the package has %d. "+
			"Update barM23OracleCallSites — the bound is quoted by phases citing this oracle.",
		barM23OracleCallSites, sites)

	// And the bound must actually reach the output, not merely exist as a const.
	assert.Contains(t, barM23Bounds(), "POPULATION IS NOT ENUMERATED")
	assert.Contains(t, barM23Bounds(), fmt.Sprintf("%d call sites", sites))
}

// ---------------------------------------------------------------------------
// The AST filters, extracted — and the rule they are all instances of
// ---------------------------------------------------------------------------

// THE STANDING RULE, and it is written once here because five guards in this file broke
// it the same way: AN AST FILTER'S COMMENT MUST STATE WHICH SYNTACTIC FORMS IT DOES NOT
// MATCH.
//
// Every census in this file defines its population with a type assertion, and a type
// assertion that fails is a SILENT SKIP. A census that silently drops a form reports a
// wrong denominator with total confidence — and unlike a missing guard, an agreeing one is
// persuasive. Independent review found all five filters evadable; two of them had a
// SECOND escape beyond the first.
//
// So the filters live here as functions rather than inline, for one reason that is not
// tidiness: it lets TestBARM23_ASTFiltersMatchTheFormsTheyClaim run them over a SYNTHETIC
// source containing every evasive form, which is the only way an anti-vacuity check can
// witness a form the real tree does not contain. Planting a pointer-receiver method or an
// untyped status constant in production source to prove a guard sees them would be
// absurd; parsing a string that contains them is not.
//
// Where a form genuinely cannot be caught, the comment DECLARES it instead of implying
// coverage. That is the difference between a stated limit and a false statement of
// coverage, and 118B-16 was the second kind.

// barM23CountOracleCalls counts CALLS to barM23Oracle in one parsed file.
//
// Forms it MATCHES: a bare identifier call, a parenthesised one, and a selector whose
// final name is barM23Oracle.
//
// 🔴 FORM IT CANNOT MATCH, DECLARED: a call through a FUNCTION VALUE — `f := barM23Oracle;
// f(dag, data)` — is a call to `f`, and no call-site count can see it. That is a real bound
// on the population bound, not a bug to be fixed here: catching it needs type information
// this sweep does not have. A wrapper function IS caught, because the wrapper's own body
// contains a countable call.
//
// Direction of the residual error: UNDER-count, so the printed population can understate
// what is checked. That is the direction that matters, since 118B-2's population sweep is
// filed as 119 debt ON THIS PIN standing in for it: a refactor collapsing call sites behind
// one helper drives the count down, the const is updated DOWN to keep the pin green, and
// the report then claims a smaller population than the oracle actually sees.
func barM23CountOracleCalls(f *ast.File) int {
	n := 0
	ast.Inspect(f, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		fun := call.Fun
		for {
			p, ok := fun.(*ast.ParenExpr)
			if !ok {
				break
			}
			fun = p.X
		}
		switch fn := fun.(type) {
		case *ast.Ident:
			if fn.Name == "barM23Oracle" {
				n++
			}
		case *ast.SelectorExpr:
			if fn.Sel != nil && fn.Sel.Name == "barM23Oracle" {
				n++
			}
		}
		return true
	})
	return n
}

// barM23ConstStringValue folds a constant expression to its string value.
//
// Forms it MATCHES: a string literal, a parenthesised one, and a concatenation of those.
//
// 🔴 It REPORTS FAILURE for anything else rather than skipping it, and that inversion is
// the whole point (118B-16). The previous filter took only *ast.BasicLit and skipped the
// rest, so `Quarantined NodeStatus = "quar" + "antined"` — a correctly typed status, a real
// member of the taxonomy — left the census silently. A value this cannot resolve makes the
// census RED and names the constant, because a census that cannot read a member of its own
// population cannot certify totality over it.
func barM23ConstStringValue(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		if err != nil {
			return "", false
		}
		return s, true
	case *ast.ParenExpr:
		return barM23ConstStringValue(v.X)
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		l, lok := barM23ConstStringValue(v.X)
		r, rok := barM23ConstStringValue(v.Y)
		if !lok || !rok {
			return "", false
		}
		return l + r, true
	default:
		return "", false
	}
}

// barM23StatusConstants extracts the NodeStatus taxonomy declared in one parsed file, and
// returns separately the constants whose value it COULD NOT resolve.
//
// 🔴 THE UNIT IS THE CONST BLOCK, NOT THE SPEC, and that is 118B-16's fix. The previous
// filter required each spec to carry an explicit `NodeStatus` type and justified the rest
// with: "a constant declared without it is an untyped string and is not a member of the
// taxonomy, so skipping it is correct rather than a blind spot." THAT SENTENCE WAS FALSE,
// and it is this milestone's own defect class — locally true, restated wider. An UNTYPED
// string constant is implicitly assignable to a named string type, so
// `Quarantined = "quarantined"` sitting in node.go's own NodeStatus block is storable
// through SetNodeStatus with NO CONVERSION and comes back from the journal as a status the
// oracle meets. It is not a member of the DECLARED-TYPE set; it is a member of the
// REACHABLE set, and the reachable set is the one that matters.
//
// So: any const block containing at least one explicitly NodeStatus-typed spec puts ALL of
// its untyped specs in the population too.
//
// 🔴 FORMS IT DOES NOT MATCH, DECLARED: a status constant declared in a block containing no
// explicitly typed spec at all, and one whose type is written as a qualified name from
// another package. Both are outside how this package declares its taxonomy, and both are
// stated rather than implied.
func barM23StatusConstants(f *ast.File) (statuses []NodeStatus, unresolved []string) {
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		blockIsTaxonomy := false
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if id, ok := vs.Type.(*ast.Ident); ok && id.Name == "NodeStatus" {
				blockIsTaxonomy = true
				break
			}
		}
		if !blockIsTaxonomy {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			// An explicitly typed spec counts only if that type is NodeStatus; an UNTYPED
			// spec in this block counts, because it is implicitly assignable to it.
			if vs.Type != nil {
				id, ok := vs.Type.(*ast.Ident)
				if !ok || id.Name != "NodeStatus" {
					continue
				}
			}
			for i, v := range vs.Values {
				name := "?"
				if i < len(vs.Names) {
					name = vs.Names[i].Name
				}
				s, ok := barM23ConstStringValue(v)
				if !ok {
					unresolved = append(unresolved, name)
					continue
				}
				statuses = append(statuses, NodeStatus(s))
			}
		}
	}
	return statuses, unresolved
}

// barM23ReportMethods returns every method declared on barM23Report in one parsed file,
// and those that hand back a verdict WITHOUT its scope.
//
// 🔴 THE OFFENDER RULE IS "EXACTLY ONE RETURN VALUE, AND NOT String", not "returns bool",
// and that is 118B-15's fix. The previous filter asked for a bare `bool` identifier, which
// two forms walked straight through:
//
//   - a POINTER receiver (*ast.StarExpr, not *ast.Ident) — the filter even named this form
//     in a comment and then did not handle it. `rep` is addressable, so Go auto-addresses
//     and `if !rep.Violated()` compiles exactly as before;
//   - a NAMED BOOLEAN type (`type v bool`), since `!` and `if` accept any boolean-KIND type.
//     No AST-only filter can know a named type's underlying kind.
//
// The second form is why the rule keys on ARITY instead of on the return type's spelling. A
// single return value cannot carry both a verdict and its scope, whatever it is named, so
// arity is the property that actually matters and it is decidable from syntax alone.
// String is allow-listed because it returns the scope ALONE, which is the opposite defect
// from a scopeless verdict.
//
// 🔴 FORM IT DOES NOT MATCH, DECLARED: a method returning two or more values NEITHER of
// which is the scope — `(bool, int)` would pass. Arity is a proxy, and this is the gap the
// proxy leaves.
func barM23ReportMethods(f *ast.File) (methods, offenders []string) {
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}
		recv := fn.Recv.List[0].Type
		if star, ok := recv.(*ast.StarExpr); ok {
			recv = star.X // a POINTER receiver, which is what walked through before
		}
		id, ok := recv.(*ast.Ident)
		if !ok || id.Name != "barM23Report" {
			continue
		}
		methods = append(methods, fn.Name.Name)
		if fn.Name.Name == "String" {
			continue
		}
		if res := fn.Type.Results; res != nil && len(res.List) == 1 {
			// One RESULT FIELD can still declare several names — `(a, b bool)` — so count
			// the values, not the fields.
			n := len(res.List[0].Names)
			if n <= 1 {
				offenders = append(offenders, fn.Name.Name)
			}
		}
	}
	sort.Strings(methods)
	sort.Strings(offenders)
	return methods, offenders
}

// barM23BitingTests returns the tests in one parsed file that assert the oracle REPORTS A
// VIOLATION — the arms that make "the DETECTOR is bitten" true.
//
// # Why a conjunction, and why it is the whole design
//
// A test qualifies only if the SAME function both (a) asserts a value bound from Passed()
// is false, and (b) asserts Violations is non-empty. Either half alone is satisfied by an
// arm that proves something else entirely: TestBARM23_SuspendableSinkIsUndecidedNotGreen
// and TestBARM23_UnclassifiedStatusIsUndecidedNotVacuous both assert a false verdict, and
// both assert Violations is ZERO, because their subject is UNDECIDED. Counting them as
// bites would let the detector claim be held up by arms that never make the oracle fire —
// which is the vacuity this census exists to refuse.
//
// # FORMS IT DOES NOT MATCH, DECLARED
//
//   - an assertion made inside a HELPER the test calls, rather than in the test's own body
//     (closures inside the body ARE walked, so subtests count);
//   - a hand-written `if got { t.Fatal(...) }` instead of a testify assertion;
//   - a differently spelled violation assertion — `require.Equal(t, 1, len(x.Violations))`
//     rather than require.Len or require.NotEmpty.
//
// All three make the census RED rather than silently green, because the census asserts it
// found at least one biting arm. That is the fail-safe direction: the failure mode is being
// told to re-witness a claim that is in fact held, never being told a claim holds when it
// does not.
func barM23BitingTests(f *ast.File) []string {
	var biting []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Body == nil || !strings.HasPrefix(fn.Name.Name, "Test") {
			continue
		}
		verdicts := map[string]bool{} // identifiers bound from a Passed() call
		assertsFalse, assertsViolation := false, false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if as, ok := n.(*ast.AssignStmt); ok && len(as.Rhs) == 1 {
				if call, ok := as.Rhs[0].(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Passed" {
						if id, ok := as.Lhs[0].(*ast.Ident); ok && id.Name != "_" {
							verdicts[id.Name] = true
						}
					}
				}
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || (pkg.Name != "require" && pkg.Name != "assert") {
				return true
			}
			switch sel.Sel.Name {
			case "False":
				if len(call.Args) >= 2 {
					if id, ok := call.Args[1].(*ast.Ident); ok && verdicts[id.Name] {
						assertsFalse = true
					}
				}
			case "Len":
				if len(call.Args) >= 3 && barM23IsViolationsField(call.Args[1]) {
					if lit, ok := call.Args[2].(*ast.BasicLit); ok && lit.Kind == token.INT && lit.Value != "0" {
						assertsViolation = true
					}
				}
			case "NotEmpty":
				if len(call.Args) >= 2 && barM23IsViolationsField(call.Args[1]) {
					assertsViolation = true
				}
			}
			return true
		})
		if assertsFalse && assertsViolation {
			biting = append(biting, fn.Name.Name)
		}
	}
	sort.Strings(biting)
	return biting
}

// barM23IsViolationsField reports whether an expression selects the Violations field.
func barM23IsViolationsField(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Violations"
}

// TestBARM23_ZeroBoundariesIsNotAPass is the anti-vacuity floor on the VERDICT, and it is
// the arm phase 119 depends on without knowing it.
//
// 119's success criterion is "oracle green over N generated cases" — a generator calling
// Passed() in a loop. Before this, a generated DAG that declared no boundary returned true
// and counted as a passing case, so vacuous greens were indistinguishable from real ones in
// the phase this oracle exists to gate.
//
// The fixture is a real built DAG driven to completion with NO WithBoundary call, which is
// the state a generator reaches by omission rather than by error — nothing is seeded and
// nothing is malformed.
func TestBARM23_ZeroBoundariesIsNotAPass(t *testing.T) {
	c := newExecCounter()
	wb := NewWorkflowBuilder().WithWorkflowID("barm23-noboundary")
	wb.AddStartNode("seed").WithAction(resumeCountAction(c, "seed"))
	wb.AddNode("after").DependsOn("seed").WithAction(resumeCountAction(c, "after"))
	dag, err := wb.Build()
	require.NoError(t, err)

	data := NewWorkflowData("barm23-noboundary")
	require.NoError(t, dag.Execute(context.Background(), data))

	// CONFIRM THE FIXTURE IS THE ONE THE ARM IS ABOUT: a clean, fully successful run that
	// simply declares nothing. If the run had failed, a non-pass would prove nothing.
	require.Equal(t, 1, c.get("after"), "the graph must actually have run")
	assertNodeStatus(t, data, "after", Completed)

	rep := barM23Oracle(dag, data)
	require.Zero(t, rep.Boundaries, "the fixture must declare NO boundary")
	require.Empty(t, rep.Violations)
	require.Empty(t, rep.Unresolved)

	passed, scope := rep.Passed()
	require.False(t, passed,
		"quantifying over the EMPTY SET is not a pass. Every other conjunct is satisfied here — zero "+
			"violations, zero undecided — which is exactly why the floor has to sit on Boundaries:\n%s",
		scope)
	assert.Contains(t, scope, "NO DECLARED BOUNDARY",
		"the render must say it quantified over nothing, not merely withhold the pass")
	// ONE verdict line, not two. The zero-boundary arm was additive when it landed, so the
	// report printed its own "NOT a pass" line and then fell through to "no counterexample"
	// — two verdicts, the second of which reads as reassurance (qa, LOW). A reader quoting
	// the wrong line quotes a green off a report that is not one.
	assert.NotContains(t, scope, "no counterexample",
		"a zero-boundary report must render exactly ONE verdict line; the no-counterexample "+
			"branch is for reports that actually quantified over something")
}

// TestBARM23_TheDetectorIsBitten gives the oracle's falsification power a MECHANICAL FLOOR,
// and it closes a hole in the instrument that exists to prevent exactly this.
//
// # The drift, walked through by qa
//
// TestBARM23_PopulationBoundIsAccurate pins the COUNT of call sites. NOTHING pinned that
// any of them asserts a RED. Delete the three violation-asserting tests and the pin moves
// 10 → 7 and reds with "Update barM23OracleCallSites" — you update the const, everything is
// green, and barM23Bounds still PRINTS "The DETECTOR is bitten: a seeded violation reds
// it." False, on every report, with nothing anywhere noticing.
//
// # The part that makes it worth its own guard
//
// This file already names that exact drift direction — barM23CountOracleCalls warns that
// "the const is updated DOWN to keep the pin green" — but only for the POPULATION claim,
// never for the BITE claim, which is the more important of the two. The instrument built to
// stop a hand-maintained number drifting out of agreement with what it describes left the
// number that matters most unguarded. A correction is not exempt from the failure it
// corrects: the difference is not care, it is whether something re-runs.
//
// The forms this census cannot see are declared on barM23BitingTests, and all of them fail
// CLOSED.
func TestBARM23_TheDetectorIsBitten(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "bar_m23_oracle_test.go", nil, 0)
	require.NoError(t, err)

	// ANTI-VACUITY: a walker that saw no tests at all reports no biting arms and would red
	// for the wrong reason — "the claim is unwitnessed" instead of "the sweep is broken".
	tests := 0
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") {
			tests++
		}
	}
	require.Greater(t, tests, 10,
		"the sweep found only %d test functions in this file — it is BROKEN, not the suite", tests)

	biting := barM23BitingTests(f)
	require.NotEmpty(t, biting,
		"NO test in this file asserts that the oracle REPORTS A VIOLATION.\n"+
			"barM23Bounds prints \"The DETECTOR is bitten: a seeded violation reds it\" on every "+
			"report, and 119-121 cite this oracle as their exit gate. Without an arm that drives the "+
			"oracle to a violation and asserts it, that sentence is a claim about code nobody runs — "+
			"and the population pin will NOT catch its loss: deleting the biting arms moves the pin's "+
			"count down, and updating the const to match turns the suite green with the claim still "+
			"printed. An arm qualifies by asserting BOTH that a value bound from Passed() is false "+
			"AND that Violations is non-empty; asserting only the first is what the UNDECIDED arms do, "+
			"and an undecided report is not the detector firing.")
	t.Logf("the detector is bitten by %d arm(s): %v", len(biting), biting)
}

// TestBARM23_ASTFiltersMatchTheFormsTheyClaim is the anti-vacuity witness for all three
// filters above, and it is the reason they are functions.
//
// Independent review found every one of the five type assertions in this file evadable, two
// of them by a SECOND form beyond the first — and none of it was visible from the real
// tree, because the real tree contains none of the evasive forms. An anti-vacuity check
// that can only assert "the sweep found the things that are there" cannot witness that.
//
// So the filters run over a SYNTHETIC source carrying every form at once. It is parsed,
// never compiled, which is what makes it free to contain a qualified type from a package
// that does not exist. Each fix below is bitten by construction: revert the fix and this
// test names the form that got through.
func TestBARM23_ASTFiltersMatchTheFormsTheyClaim(t *testing.T) {
	const synthetic = `package workflow

const (
	SynthAlpha NodeStatus = "alpha"
	SynthBeta              = "beta"
	SynthGamma NodeStatus = "gam" + "ma"
	SynthDelta NodeStatus = synthSomewhereElse
	SynthNotAStatus int   = 3
)

func (r barM23Report) SynthValueBool() bool         { return false }
func (r *barM23Report) SynthPointerBool() bool      { return false }
func (r barM23Report) SynthNamedBool() synthVerdict { return false }
func (r barM23Report) SynthQualified() other.Type   { return nil }
func (r barM23Report) String() string               { return "" }
func (r barM23Report) Passed() (bool, string)       { return false, "" }
func (r barM23Report) SynthNoReturn()               {}

func synthCallForms() {
	_ = barM23Oracle(nil, nil)
	_ = (barM23Oracle)(nil, nil)
	g := barM23Oracle
	_ = g(nil, nil)
}

func TestSynthBites(t *testing.T) {
	passed, scope := rep.Passed()
	require.False(t, passed, scope)
	require.Len(t, rep.Violations, 1)
}

func TestSynthUndecidedIsNotABite(t *testing.T) {
	passed, scope := rep.Passed()
	require.False(t, passed, scope)
	require.Len(t, rep.Unresolved, 1)
	require.Zero(t, len(rep.Violations))
}

func TestSynthVerdictWithoutViolationAssertion(t *testing.T) {
	passed, _ := rep.Passed()
	require.False(t, passed)
}

func TestSynthViolationAssertionWithoutVerdict(t *testing.T) {
	require.Len(t, rep.Violations, 1)
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic_forms.go", synthetic, 0)
	require.NoError(t, err, "the synthetic fixture must parse, or this witness proves nothing")

	// --- the call counter (the population pin's filter) ---
	//
	// TWO of the three call forms are counted. The third, the function value, is the
	// DECLARED bound: `g(nil, nil)` is a call to g and no call-site count can see it. This
	// asserts the declared number so that the bound stays a measured statement rather than
	// a hedge in a comment.
	require.Equal(t, 2, barM23CountOracleCalls(f),
		"the counter must see the bare and PARENTHESISED call forms, and must not claim to see "+
			"the function-value form, which is declared uncountable in barM23CountOracleCalls")

	// --- the status census filter ---
	statuses, unresolved := barM23StatusConstants(f)
	require.Contains(t, statuses, NodeStatus("alpha"), "a plainly typed status must be seen")
	require.Contains(t, statuses, NodeStatus("beta"),
		"an UNTYPED constant in a NodeStatus block must be seen: it is implicitly assignable to "+
			"NodeStatus and reaches SetNodeStatus with no conversion (118B-16)")
	require.Contains(t, statuses, NodeStatus("gamma"),
		"a DERIVED value must be folded, not skipped: concatenation is still a status (118B-16)")
	require.NotContains(t, statuses, NodeStatus("3"), "a differently typed constant is not a status")
	require.Equal(t, []string{"SynthDelta"}, unresolved,
		"a value the census CANNOT resolve must be reported, never silently dropped — that is the "+
			"inversion 118B-16 is about")

	// --- the verdict-accessor filter ---
	methods, offenders := barM23ReportMethods(f)
	require.Contains(t, methods, "SynthPointerBool",
		"the sweep must see POINTER-receiver methods at all; before 118B-15 its population was "+
			"value receivers only, so it could not have reported one")
	require.Equal(t,
		[]string{"SynthNamedBool", "SynthPointerBool", "SynthQualified", "SynthValueBool"},
		offenders,
		"every single-value accessor is scopeless whatever its receiver form or the spelling of "+
			"its return type; String returns the scope alone and Passed returns both, so neither "+
			"is an offender, and a method returning nothing is not a verdict")

	// --- the biting-arm census ---
	//
	// The CONJUNCTION is the property under test, and the three negative fixtures are what
	// witness it: an UNDECIDED-shaped arm asserts a false verdict AND zero violations, and
	// each half on its own appears in an arm that proves something else. Only the arm
	// carrying both qualifies.
	require.Equal(t, []string{"TestSynthBites"}, barM23BitingTests(f),
		"a biting arm is one that asserts BOTH a false verdict and a non-empty Violations. An "+
			"undecided arm asserts the first and explicitly denies the second, so counting it would "+
			"let the detector claim rest on arms that never make the oracle fire")
}

// TestBARM23_StatusMappingIsTotal is what makes barM23Evidence's totality MECHANICAL
// rather than asserted in its own doc comment.
//
// 118B-1 and 118B-6 are the same defect one layer apart: a status set standing in for an
// event, sound for every kind the author tested and unsound for a class they did not. The
// remedy for the second instance cannot be "and I remembered the ninth status", because
// the tenth is the one that bites. So the taxonomy is read from node.go MECHANICALLY and
// every constant it declares must be classified here. Adding a NodeStatus without
// deciding what it proves about the past reds this test by name.
//
// go/ast rather than grep, for the same reason the population pin uses it: this file's
// own comments name statuses repeatedly and a grep counts substrings.
//
// # 🔴 DO NOT JUDGE THIS GUARD BY THE ARMS IT DOES NOT RED
//
// MEASURED, and it is the reason this census is not redundant with the behavioural arms:
// adding a tenth status to node.go, and separately dropping a case from the mapping, each
// red THIS TEST AND NOTHING ELSE. A status no fixture can produce appears in no run, so
// every behavioural arm in this file is structurally blind to it. **This is the only
// instrument that can observe the taxonomy growing.**
//
// A future reader deciding whether it earns its keep will otherwise reason from the arms
// it fails to red, conclude it is decorative, and delete it — and the file returns to
// "I remembered the ninth status", which is exactly the state 118B-6 was filed against.
func TestBARM23_StatusMappingIsTotal(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	var statuses []NodeStatus
	var unresolved []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, e.Name(), nil, 0)
		require.NoError(t, perr, "parsing %s", e.Name())
		found, bad := barM23StatusConstants(f)
		statuses = append(statuses, found...)
		for _, b := range bad {
			unresolved = append(unresolved, e.Name()+": "+b)
		}
	}

	// A constant whose value this census cannot evaluate is a member of the population it
	// cannot read, so it reds rather than vanishing (118B-16). The filter's forms and its
	// declared limits are documented on barM23StatusConstants.
	sort.Strings(unresolved)
	require.Empty(t, unresolved,
		"NodeStatus constant(s) whose value the census could not resolve: %v\n"+
			"The census cannot certify that the oracle classifies every status while it cannot read "+
			"what these ARE. Either write the value as a string literal or a concatenation of them, "+
			"or widen barM23ConstStringValue — do not let it skip them, which is exactly the defect "+
			"118B-16 recorded.", unresolved)

	// ANTI-VACUITY, and it is the whole difference between this guard and a decorative
	// one: a sweep aimed at the wrong directory finds no constants, classifies all zero of
	// them, and passes looking exactly like a correct census. The floor is the taxonomy as
	// it stands (M23 ships nine); it is a floor rather than an equality so that ADDING a
	// properly classified status stays green.
	require.GreaterOrEqual(t, len(statuses), 9,
		"the census found only %d NodeStatus constants — it is BROKEN, not the taxonomy: %v",
		len(statuses), statuses)
	require.Contains(t, statuses, CompensationFailed,
		"the census must see the status 118B-6 is about, or it is not aimed at node.go")

	var unclassified []string
	for _, st := range statuses {
		if !barM23Evidence(st).classified {
			unclassified = append(unclassified, string(st))
		}
	}
	sort.Strings(unclassified)
	require.Empty(t, unclassified,
		"NodeStatus value(s) the oracle's status mapping does not classify: %v\n"+
			"barM23Evidence answers two HAS-THIS-EVER-HAPPENED questions from a status that a saga "+
			"rollback rewrites, so an unclassified status is not a neutral omission: it is a boundary "+
			"the oracle reports UNDECIDED, and 119-121 cite this oracle as their exit gate. Decide, in "+
			"barM23Evidence, whether this status proves the action was ENTERED and whether it proves "+
			"the action ever reached COMPLETED — node.go's doc comment for the constant is where that "+
			"answer lives.", unclassified)

	// everCompleted ⊆ invoked. A node cannot have run to Completed without having entered
	// its action, so a mapping that claims otherwise is internally inconsistent whatever
	// the taxonomy says.
	for _, st := range statuses {
		ev := barM23Evidence(st)
		if ev.everCompleted {
			require.True(t, ev.invoked,
				"status %v is classified as having reached Completed but not as having been invoked", st)
		}
	}
}

// TestBARM23_NoScopelessVerdictAccessor keeps barM23Report's central claim — that a
// caller cannot obtain a verdict without also obtaining the scope of that verdict — TRUE
// rather than merely written down.
//
// The compiler already enforces it for the API as it stands: Passed returns two values, so
// no caller can use it in a boolean context. What the compiler cannot do is stop the
// scopeless accessor from being ADDED BACK. That is not hypothetical here — `Violated()
// bool` sat under that exact sentence through three review passes (118B-12), and the
// sentence read as true to every reader who checked it against its immediate
// neighbourhood.
//
// BOUNDS, stated because the guard is narrower than the sentence, and both are on
// barM23ReportMethods where the filter lives: it inspects METHODS on barM23Report, so a
// free function taking a report would evade it; and it keys on ARITY, so a two-value
// method returning neither the scope nor anything like it would too. The claim it makes
// mechanical is "the report type exposes no scopeless verdict accessor", which is the form
// the defect actually took.
//
// 🔴 THIS GUARD ITSELF HAD TWO HOLES, both found by independent review and both exactly
// where the deleted accessor would be re-added (118B-15): a POINTER receiver walked through
// the receiver filter, and a NAMED BOOLEAN return type walked through the return filter,
// each leaving `if !rep.Violated()` compiling with the guard green. The extraction now
// lives in barM23ReportMethods and its forms are witnessed by
// TestBARM23_ASTFiltersMatchTheFormsTheyClaim against a synthetic source, because the real
// tree contains none of the evasive forms and therefore cannot witness them.
func TestBARM23_NoScopelessVerdictAccessor(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "bar_m23_oracle_test.go", nil, 0)
	require.NoError(t, err)

	methods, offenders := barM23ReportMethods(f)

	// ANTI-VACUITY: a parser that found no methods at all reports no offenders and looks
	// exactly like a clean sweep. The FORM-specific half — that the sweep can see a pointer
	// receiver and a named boolean at all — is witnessed on the synthetic source, since
	// asserting it here would mean planting an offending method in this type to prove the
	// guard can see one.
	require.Contains(t, methods, "Passed", "the sweep did not find the verdict accessor — it is BROKEN")
	require.Contains(t, methods, "String", "the sweep did not find the scope renderer — it is BROKEN")

	require.Empty(t, offenders,
		"barM23Report method(s) returning a SINGLE value: %v\n"+
			"One return value cannot carry both a verdict and its scope, whatever its type is named "+
			"and whatever the receiver form — that is the defect 118B-12 recorded, where Violated() "+
			"ignored Unresolved and handed back a scopeless boolean, and phases citing \"oracle "+
			"green\" reach for exactly such a method. Return the scope alongside the verdict, as "+
			"Passed does. If you are adding a genuine non-verdict accessor, it still has to hand back "+
			"the scope or be the renderer: this guard keys on ARITY precisely because a named "+
			"boolean type walked through the previous return-type check (118B-15).", offenders)
}

// TestBARM23_UnclassifiedStatusIsUndecidedNotVacuous is the BEHAVIOURAL witness for
// barM23Evidence's fail-safe default, and it exists because the mutation matrix showed
// nothing else could see it.
//
// Mutating node.go to add a tenth status reds TestBARM23_StatusMappingIsTotal and NOTHING
// ELSE: a status no fixture can produce is invisible to every behavioural arm, so the
// census was the only instrument watching. That is a blind cell worth closing directly,
// because the census guards the MAPPING and this guards what the ORACLE DOES with an
// answer the mapping could not give.
//
// The reachable instance is an operand with NO recorded status. GetNodeStatus returns the
// empty NodeStatus and false for a node the journal has never seen, and the empty status
// is by construction not one of node.go's constants. Before this fix that boundary took
// the "S never entered its action; clause 1 is vacuous" path and the report was a clean
// green. An oracle handed a journal it cannot read has established nothing, and a green
// over a never-run workflow is the exact vacuous pass this phase exists to refuse.
func TestBARM23_UnclassifiedStatusIsUndecidedNotVacuous(t *testing.T) {
	c := newExecCounter()
	dag := buildBoundaryResumeDAG(t, "barm23-unclassified", c)

	// A journal that has never been driven: no node has a status.
	data := NewWorkflowData("barm23-unclassified")
	st, ok := data.GetNodeStatus("sink")
	require.False(t, ok, "the fixture requires an UNRECORDED status; the journal has one")
	require.False(t, barM23Evidence(st).classified,
		"the empty status must be unclassified, or this fixture is not exercising the default")

	rep := barM23Oracle(dag, data)
	require.Equal(t, 1, rep.Boundaries, "the oracle must quantify over the declared boundary")
	passed, scope := rep.Passed()
	require.Len(t, rep.Unresolved, 1,
		"a boundary the oracle cannot read must be UNDECIDED, not vacuously skipped:\n%s", scope)
	require.False(t, passed,
		"an oracle that could not read the journal has established nothing, and 119-121 cite this "+
			"verdict:\n%s", scope)
	assert.Contains(t, scope, "NO ENTRY IN THE JOURNAL AT ALL",
		"the report must name WHICH unreadable case this is — a reader who gets UNDECIDED needs to "+
			"know the journal is empty rather than that some status went unclassified")
	assert.Zero(t, len(rep.Violations), "it is undecided, not a violation")
}

// TestBARM23_CompensatedStatusesDoNotRewriteTheVerdict is 118B-6's regression. It is one
// test with two arms because they are two symptoms of ONE root — the oracle answered a
// has-this-ever-happened question from a status a rollback rewrites — and splitting them
// would let a fix for either read as a fix for the finding.
//
// Both arms drive a REAL saga rollback through Workflow.Execute with a store. Nothing is
// seeded into a status.
func TestBARM23_CompensatedStatusesDoNotRewriteTheVerdict(t *testing.T) {
	// ARM A — the SILENT FALSE GREEN. A sink whose action ran to Completed and whose undo
	// then failed sits at CompensationFailed. Before the fix that status was absent from
	// the invoked set, so a genuine clause-1 violation — S invoked, V never Completed —
	// reached the "S never entered its action" continue and was reported as a clean green.
	t.Run("sinkAtCompensationFailedIsAViolation", func(t *testing.T) {
		const id = "barm23-compfail"
		c := newExecCounter()
		store := NewInMemoryStore()

		wb := NewWorkflowBuilder().WithWorkflowID(id)
		wb.AddStartNode("seed").WithAction(resumeCountAction(c, "seed"))
		wb.AddNode("verifier").DependsOn("seed").WithAction(ActionFunc(func(_ context.Context, _ *WorkflowData) error {
			c.inc("verifier")
			return errors.New("verifier failed")
		}))
		// sink is a SIBLING of verifier — nothing orders them, so sink runs whatever
		// verifier does. Its compensation FAILS, which is what lands CompensationFailed.
		wb.AddNode("sink").DependsOn("seed").WithAction(resumeCountAction(c, "sink")).
			WithCompensationFunc(func(context.Context, *WorkflowData) error { return errors.New("undo failed") })
		dag, err := wb.Build()
		require.NoError(t, err)

		// build() refuses this declaration (sink is not dominated by verifier); the
		// oracle's subject is the declaration, so it is applied directly — same reasoning
		// as TestBARM23_Oracle_Bites. The COMPENSATION STATUSES, which are this arm's
		// subject, are produced by a real rollback and are not seeded.
		dag.boundaries = []boundaryDecl{{doer: "seed", verifier: "verifier", sink: "sink"}}

		w := &Workflow{dag: dag, WorkflowID: id, Store: store}
		require.Error(t, w.Execute(context.Background()), "V fails, so the run fails and rollback drives")

		data, lerr := store.Load(id)
		require.NoError(t, lerr)

		// CONFIRM THE FIXTURE REACHED THE CLASS, before consulting the oracle — otherwise a
		// green would be attributable to an unreached fixture rather than to the oracle.
		require.Equal(t, 1, c.get("sink"), "S's action must ACTUALLY have been invoked")
		sinkSt, _ := data.GetNodeStatus("sink")
		require.Equal(t, CompensationFailed, sinkSt,
			"the arm is about CompensationFailed; the fixture landed %v", sinkSt)
		vSt, _ := data.GetNodeStatus("verifier")
		require.NotEqual(t, Completed, vSt, "V must never have Completed; got %v", vSt)

		rep := barM23Oracle(dag, data)
		passed, scope := rep.Passed()
		require.False(t, passed,
			"S ran to Completed and its undo failed — that is an INVOCATION, and V never completed. "+
				"A clean green here is 118B-6 instance A:\n%s", scope)
		require.Len(t, rep.Violations, 1, "one declared boundary, one violation:\n%s", scope)
		assert.Contains(t, scope, "S was invoked", "the message must name what went wrong")
	})

	// ARM B — the FALSE POSITIVE, through the FULLY PUBLIC API. A verifier that ran to
	// Completed and was then compensated sits at Compensated, and 047e55a reported a
	// violation on a boundary that was HONOURED. The property is PRECEDENCE
	// (DEC-M23-NAMING): V ran before S was invoked, and a rollback that afterwards undoes
	// what V did does not un-run it.
	t.Run("compensatedVerifierIsHonoured", func(t *testing.T) {
		// The control runs the SAME graph with no compensations. The only difference
		// between the two is the rollback statuses, which is what makes this arm a
		// measurement rather than an anecdote.
		run := func(t *testing.T, id string, comp func(context.Context, *WorkflowData) error) (*DAG, *WorkflowData, *execCounter) {
			t.Helper()
			c := newExecCounter()
			store := NewInMemoryStore()
			wb := NewWorkflowBuilder().WithWorkflowID(id)
			wb.AddStartNode("seed").WithAction(resumeCountAction(c, "seed"))
			doer := wb.AddNode("doer").DependsOn("seed").WithAction(resumeCountAction(c, "doer"))
			verifier := wb.AddNode("verifier").DependsOn("doer").WithAction(resumeCountAction(c, "verifier"))
			sink := wb.AddNode("sink").DependsOn("verifier").WithAction(resumeCountAction(c, "sink"))
			if comp != nil {
				doer.WithCompensationFunc(comp)
				verifier.WithCompensationFunc(comp)
				sink.WithCompensationFunc(comp)
			}
			// The rollback trigger sits AFTER the sink, so the boundary is fully executed
			// and honoured before anything is undone.
			wb.AddNode("boom").DependsOn("sink").WithAction(ActionFunc(func(_ context.Context, _ *WorkflowData) error {
				return errors.New("trigger the rollback")
			}))
			wb.WithBoundary("doer", "verifier", "sink")
			dag, err := wb.Build()
			require.NoError(t, err, "the declared boundary must satisfy the root-anchored predicate")

			w := &Workflow{dag: dag, WorkflowID: id, Store: store}
			require.Error(t, w.Execute(context.Background()), "boom fails, so the run fails")
			data, lerr := store.Load(id)
			require.NoError(t, lerr)
			return dag, data, c
		}

		dag, data, c := run(t, "barm23-compensated", okCompFn)
		require.Equal(t, 1, c.get("sink"), "S must actually have been invoked")
		vSt, _ := data.GetNodeStatus("verifier")
		sSt, _ := data.GetNodeStatus("sink")
		require.Equal(t, Compensated, vSt, "the arm is about a COMPENSATED verifier; got %v", vSt)

		rep := barM23Oracle(dag, data)
		passed, scope := rep.Passed()
		require.True(t, passed,
			"V ran to Completed BEFORE S was invoked and was compensated afterwards — the boundary was "+
				"HONOURED. A violation here is 118B-6 instance B (V=%v, S=%v):\n%s", vSt, sSt, scope)
		require.Equal(t, 1, rep.Boundaries, "pin the fixture's population, per the Passed() floor")

		// CONTROL: same graph, no compensations, rollback still drives.
		cdag, cdata, _ := run(t, "barm23-compensated-control", nil)
		cvSt, _ := cdata.GetNodeStatus("verifier")
		require.Equal(t, Completed, cvSt, "the control's V must stay Completed; got %v", cvSt)
		crep := barM23Oracle(cdag, cdata)
		cPassed, cScope := crep.Passed()
		require.True(t, cPassed, "the control must be clean:\n%s", cScope)
		t.Logf("118B-6 arm B: compensated V=%v S=%v reports honoured; control V=%v", vSt, sSt, cvSt)
	})
}

// TestBARM23_BoundsHaveAnInvocationThatPrintsThem is the OPERATOR half of 118B-9's
// remedy, and it exists because the other half is impossible.
//
// The bounds are unconditionally part of every rendered report and Passed hands that
// render back with the verdict, so a caller IN CODE cannot miss them. A caller reading
// test OUTPUT can: `go test` discards a passing package's entire binary output, so on a
// green there is no print channel at all — measured, with controls, in barM23Bounds' doc
// comment. Nothing placed inside a test can change that.
//
// What can be made mechanical is that a named invocation exists whose output IS the
// bounds. `make bar-oracle` runs this selection with -v; this guard reds if that target
// is deleted, loses its -v, or narrows away from the oracle's tests. Without it the
// target is a convention, and a convention is the thing this whole finding is about.
//
// BOUND: it checks the Makefile's TEXT, not that anyone runs it. `make test` and CI still
// print zero bounds on a green, which barM23Bounds records as the residual rather than
// leaving it implied.
func TestBARM23_BoundsHaveAnInvocationThatPrintsThem(t *testing.T) {
	src, err := os.ReadFile("../../Makefile")
	require.NoError(t, err, "the Makefile must be readable from the package directory")

	// ANTI-VACUITY: a truncated or misaddressed read finds no target and reports nothing.
	require.Contains(t, string(src), "\ntest: ", "the sweep is not reading the project Makefile")

	var recipe []string
	inTarget := false
	for _, line := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(line, "bar-oracle:") {
			inTarget = true
			continue
		}
		if inTarget {
			// A recipe line is TAB-indented; the first line that is not ends the target.
			if !strings.HasPrefix(line, "\t") {
				break
			}
			recipe = append(recipe, line)
		}
	}
	require.NotEmpty(t, recipe,
		"the Makefile has no bar-oracle recipe. It is the ONLY invocation that prints this "+
			"oracle's population bound and arm-availability table: on a passing non-verbose run "+
			"go test discards them entirely (118B-9), so deleting the target deletes the bound "+
			"from every channel an operator has.")

	joined := strings.Join(recipe, "\n")
	assert.Contains(t, joined, " -v ",
		"bar-oracle must pass -v; without it the bounds print ZERO times and the target is a "+
			"no-op with respect to the reason it exists")
	assert.Contains(t, joined, "TestBARM23_",
		"bar-oracle must select this file's tests, or it prints some other package's output")
	assert.Contains(t, joined, "-timeout 30m",
		"pkg/workflow has exceeded the 10-minute default under load and panics with ZERO FAIL "+
			"lines when it does (H-M23-5)")
}

// TestBARM23_SuspendableSinkIsUndecidedNotGreen is 118B-1's regression, and it is keyed
// on the signal the reviewer used rather than on the one that reads the same on both
// sides: the PREDICATE INVOCATION COUNT. A suspendable sink parks BY running, so the
// discriminating fact is that its predicate was invoked while its status stayed
// non-terminal — an error-based or status-based probe cannot see that at all, which is
// precisely how the original oracle returned a silent green.
//
// Before the fix this configuration produced ZERO violations and ZERO undecided: a clean
// green over a boundary whose sink had actually run. Now it is reported as UNDECIDED,
// which is not a pass.
func TestBARM23_SuspendableSinkIsUndecidedNotGreen(t *testing.T) {
	c := newExecCounter()
	wb := NewWorkflowBuilder().WithWorkflowID("barm23-parked")
	wb.AddStartNode("seed").WithAction(resumeCountAction(c, "seed"))
	// V fails, so it never reaches Completed.
	wb.AddNode("verifier").DependsOn("seed").WithAction(ActionFunc(func(_ context.Context, _ *WorkflowData) error {
		c.inc("verifier")
		return errors.New("verifier failed")
	}))
	// S is a SUSPENDABLE kind that DEC-M23-VB08-R3 rules eligible as V/S. Its predicate is
	// false, so it enters its action, evaluates, and parks non-terminal.
	wb.AddWaitForCondition("sink", func(_ *WorkflowData) bool {
		c.inc("sink-predicate")
		return false
	}).DependsOn("seed")
	dag, err := wb.Build()
	require.NoError(t, err)

	// build() refuses this declaration (sink is not dominated by verifier), and the
	// oracle's subject is the declaration, so it is applied directly — same reasoning as
	// TestBARM23_Oracle_Bites.
	dag.boundaries = []boundaryDecl{{doer: "seed", verifier: "verifier", sink: "sink"}}

	data := NewWorkflowData("barm23-parked")
	_ = dag.Execute(context.Background(), data) //nolint:errcheck // parked+failed run errors; the statuses are the subject

	// CONFIRM THE BLIND SPOT IS REACHED, on the signal that discriminates.
	require.NotZero(t, c.get("sink-predicate"),
		"S's action must actually have been ENTERED — otherwise this fixture proves nothing")
	sinkSt, _ := data.GetNodeStatus("sink")
	require.False(t, barM23Invoked(sinkSt),
		"the whole finding is that a parked sink at %v reads as NOT invoked", sinkSt)
	vSt, _ := data.GetNodeStatus("verifier")
	require.NotEqual(t, Completed, vSt, "V must not be Completed; got %v", vSt)

	// The oracle must NOT report this as a clean green.
	rep := barM23Oracle(dag, data)
	passed, out := rep.Passed()
	require.Len(t, rep.Unresolved, 1,
		"a suspendable sink whose action ran must be UNDECIDED, not silently skipped:\n%s", out)

	// 118B-12's REGRESSION, and it is what makes the remedy above non-inert. This is a
	// REAL all-undecided report — zero violations, one unresolved — and the predecessor
	// accessor, len(Violations) > 0, called it a pass. Three of the five call sites
	// computed exactly that. The string changed and no verdict did.
	require.Zero(t, len(rep.Violations), "the fixture must be all-UNDECIDED for this to be 118B-12's case")
	require.False(t, passed,
		"a report the oracle COULD NOT DECIDE is not a pass, and the pass predicate is where that "+
			"has to be true — the report's own text already said it:\n%s", out)

	assert.Contains(t, out, "UNDECIDED")
	assert.Contains(t, out, "this is NOT a pass")
	assert.Contains(t, out, "SUSPENDABLE", "the message must name why it is undecided")
	t.Logf("118B-1 regression, sink parked at %v:\n%s", sinkSt, out)
}
