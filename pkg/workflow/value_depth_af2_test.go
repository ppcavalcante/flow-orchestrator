// 116-AF2. The adversarial pass reproduced a TOTALITY violation: json.Marshal is
// recursive with no depth limit, so a deep enough value exhausts the goroutine stack
// INSIDE the encoder, and a Go stack overflow is a fatal error rather than a panic —
// unrecoverable, no deferred recover runs, the host process dies. Every depth guard the
// package had measured the ENCODED BYTES, which only exist if the encoder survived.
//
// This file is the fix's suite. Its obligations are written from what checkValueDepth
// MEANS — "no value reaches json.Marshal that json.Marshal cannot survive, and no value
// json.Marshal handles fine is refused" — and not from the defect it was written for.
// That ordering is deliberate and was learned here: AF4b's suite, written from a stack
// overflow, was entirely about termination, and a sharing bug that returned a
// differently-shaped object passed every arm of it. So the arms below split into three
// groups that do not overlap:
//
//	REFUSES     nothing crashes, hangs, or costs unboundedly   (the defect's own frame)
//	ACCEPTS     nothing the encoder handles is falsely refused (the frame the fix could break)
//	AGREES      what the walk refuses relates correctly to what the readers refuse
package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// af2WriterEnv selects which writer the re-exec child drives. A fatal error kills the
// test binary, so any arm whose PRE-fix behaviour was a crash must run in a child whose
// exit status is read directly — recover() does not fire and an in-process assertion
// would simply take the suite down with it.
const af2WriterEnv = "AF2_WRITER"

// nestMapValue is the shape the crash band was WORST on (712,906 — the nested map),
// which is why it exists beside nestValue's []any chain rather than instead of it. The
// first shape tested is not the worst shape.
func nestMapValue(d int) any {
	var v any = 1
	for i := 0; i < d; i++ {
		v = map[string]any{"k": v}
	}
	return v
}

// cyclicValue returns the two-line self-referential map from AF4, whose defining
// property here is that json.Marshal handles it CLEANLY (a typed "encountered a cycle"
// error) — so a walk that hangs on it has traded an unrecoverable crash for a silent
// hang, on a value the encoder was never in trouble with.
func cyclicValue() any {
	m := map[string]any{}
	m["self"] = m
	return m
}

// branchingCycleValue is the value that separates a FRAME stack from a WORKLIST, and it
// is the reason the implementation is not the obvious one. A worklist-based iterative
// walk holds the entire frontier: this value adds fanout-1 entries per level, so the
// worklist grows without bound long before the depth bound is reached, and the walk dies
// of memory rather than refusing. A frame stack is O(depth) whatever the fanout.
func branchingCycleValue(fanout int) any {
	m := map[string]any{}
	for i := 0; i < fanout; i++ {
		m[fmt.Sprintf("k%d", i)] = m
	}
	return m
}

// ---------------------------------------------------------------------------
// REFUSES — nothing crashes, hangs, or costs unboundedly.
// ---------------------------------------------------------------------------

// TestAF2_EveryWriterRefusesInsteadOfDying is the fix's acceptance test, and it is a
// re-exec matrix rather than one arm ON PURPOSE.
//
// This phase's most-repeated defect is a guard that closes on some writers and not the
// rest — "the axis closed on the FOURTH writer" happened here already, and the tester
// pre-positioned it as the way an AF2 fix most likely ships still broken. So the writer
// list is the thing under test, not the depth: each entry drives a DIFFERENT path from a
// host value to json.Marshal, and every one of them died before this fix.
//
// Each child must print CHILD-RETURNED with a typed ErrValidation and exit 0. Before the
// fix each printed nothing and died with `fatal error: stack overflow`.
func TestAF2_EveryWriterRefusesInsteadOfDying(t *testing.T) {
	if w := os.Getenv(af2WriterEnv); w != "" {
		af2RunChildWriter(w)
		return
	}
	if testing.Short() {
		t.Skip("each subtest allocates a ~10^6-deep value and forks a child; skipped under -short")
	}
	for _, writer := range af2Writers {
		t.Run(writer, func(t *testing.T) {
			exe, err := os.Executable()
			require.NoError(t, err)
			cmd := exec.Command(exe, "-test.run", "^"+t.Name()+"$") //nolint:gosec // exe is this test binary
			cmd.Env = append(os.Environ(), af2WriterEnv+"="+writer)
			out, runErr := cmd.CombinedOutput()
			text := string(out)

			require.NotContains(t, text, "fatal error: stack overflow",
				"writer %q still reaches json.Marshal with the deep value: the pre-marshal walk is "+
					"missing on this path. Child output:\n%s", writer, text)
			require.NotContains(t, text, "CHILD-SETUP-FAILED", "child could not build its subject: %s", text)
			require.NoError(t, runErr, "the child must survive and exit 0. Output:\n%s", text)
			require.Contains(t, text, "CHILD-RETURNED",
				"writer %q returned nothing — it neither refused nor completed. Output:\n%s", writer, text)
			require.Contains(t, text, "ERRIS-VALIDATION=true",
				"writer %q refused, but NOT in the validation domain. A refusal in no domain (or in "+
					"the corrupt-data domain) is the exact failure 116 already fixed on the JSONFileStore "+
					"snapshot path. Output:\n%s", writer, text)
		})
	}
}

var af2Writers = []string{
	"fb-save-output", "fb-save-data", "fb-sync-batched",
	"sqlite-save-output", "sqlite-save-data", "sqlite-checkpoint",
	"json-save", "json-deliver-signal", "fb-deliver-signal", "sqlite-deliver-signal",
	"builder-withinput", "workflowdata-snapshot",
}

// af2RunChildWriter is the child half. It drives ONE writer with a 10^6-deep value and
// prints what came back. crashDepth is far above the measured band (712,906 worst-shape,
// darwin/arm64, go1.25.1, at 512 MiB of USABLE stack — not "1 GB": Go doubles stacks, so
// usable is the largest power of two <= the configured limit and the 1e9 default cannot
// reach 1 GiB) so the pre-fix behaviour is a certain kill rather than a near miss.
//
// The margin is deliberate and it is why this arm is robust to the provenance being
// restated: 10^6 exceeds the crash depth at EVERY stack size this package supports, so
// the arm does not depend on the band being exact. Per-level cost is the durable form —
// ~646 B/walk-frame for json.Marshal, ~465 for reflect.DeepEqual.
func af2RunChildWriter(writer string) {
	const crashDepth = 1_000_000
	deep := nestValue(crashDepth)

	dir, err := os.MkdirTemp("", "af2")
	if err != nil {
		fmt.Println("CHILD-SETUP-FAILED: tempdir")
		return
	}
	defer func() { _ = os.RemoveAll(dir) }() //nolint:errcheck // child process; a failed cleanup cannot change what it printed

	// A recover() proves the point rather than decorating it: a stack overflow is a fatal
	// error, so this never runs. Reaching the print below at all is the fix.
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("CHILD-RECOVERED-PANIC: %v\n", r)
		}
	}()

	report := func(err error) {
		fmt.Printf("CHILD-RETURNED: err=%v\nERRIS-VALIDATION=%v\n", err, errors.Is(err, ErrValidation))
	}

	newData := func() *WorkflowData {
		d := NewWorkflowData("wf-af2")
		return d
	}

	switch writer {
	case "fb-save-output":
		s, serr := NewFlatBuffersStore(dir)
		if serr != nil {
			fmt.Println("CHILD-SETUP-FAILED: fb store")
			return
		}
		d := newData()
		d.SetOutput("n", deep)
		report(s.Save(d))
	case "fb-save-data":
		s, serr := NewFlatBuffersStore(dir)
		if serr != nil {
			fmt.Println("CHILD-SETUP-FAILED: fb store")
			return
		}
		d := newData()
		d.Set("k", deep)
		report(s.Save(d))
	case "fb-sync-batched":
		// The group-commit writer, which does NOT go through Save. A guard landing only
		// on Save leaves Batched(K) able to kill the process.
		s, serr := NewFlatBuffersStore(dir, WithDurabilityMode(Batched(1000)))
		if serr != nil {
			fmt.Println("CHILD-SETUP-FAILED: fb batched store")
			return
		}
		d := newData()
		d.SetOutput("n", deep)
		if cerr := s.SaveCheckpoint(d); cerr != nil {
			report(cerr)
			return
		}
		report(s.Sync("wf-af2"))
	case "sqlite-save-output":
		s, serr := NewSQLiteStore(filepath.Join(dir, "a.db"))
		if serr != nil {
			fmt.Println("CHILD-SETUP-FAILED: sqlite store")
			return
		}
		defer func() { _ = s.Close() }() //nolint:errcheck // child process, about to exit
		d := newData()
		d.SetOutput("n", deep)
		report(s.Save(d))
	case "sqlite-save-data":
		s, serr := NewSQLiteStore(filepath.Join(dir, "b.db"))
		if serr != nil {
			fmt.Println("CHILD-SETUP-FAILED: sqlite store")
			return
		}
		defer func() { _ = s.Close() }() //nolint:errcheck // child process, about to exit
		d := newData()
		d.Set("k", deep)
		report(s.Save(d))
	case "sqlite-checkpoint":
		// The incremental diff path (shadowFromData), a different encoder entry than Save.
		s, serr := NewSQLiteStore(filepath.Join(dir, "c.db"))
		if serr != nil {
			fmt.Println("CHILD-SETUP-FAILED: sqlite store")
			return
		}
		defer func() { _ = s.Close() }() //nolint:errcheck // child process, about to exit
		d := newData()
		d.SetOutput("n", deep)
		report(s.SaveCheckpoint(d))
	case "json-save":
		s, serr := NewJSONFileStore(dir)
		if serr != nil {
			fmt.Println("CHILD-SETUP-FAILED: json store")
			return
		}
		d := newData()
		d.SetOutput("n", deep)
		report(s.Save(d))
	case "json-deliver-signal":
		s, serr := NewJSONFileStore(dir)
		if serr != nil {
			fmt.Println("CHILD-SETUP-FAILED: json store")
			return
		}
		report(s.DeliverSignal("wf-af2", Signal{ID: "s", Name: "n", Payload: deep}))
	case "fb-deliver-signal":
		s, serr := NewFlatBuffersStore(dir)
		if serr != nil {
			fmt.Println("CHILD-SETUP-FAILED: fb store")
			return
		}
		report(s.DeliverSignal("wf-af2", Signal{ID: "s", Name: "n", Payload: deep}))
	case "sqlite-deliver-signal":
		s, serr := NewSQLiteStore(filepath.Join(dir, "d.db"))
		if serr != nil {
			fmt.Println("CHILD-SETUP-FAILED: sqlite store")
			return
		}
		defer func() { _ = s.Close() }() //nolint:errcheck // child process, about to exit
		report(s.DeliverSignal("wf-af2", Signal{ID: "s", Name: "n", Payload: deep}))
	case "builder-withinput":
		b := NewWorkflowBuilder().WithWorkflowID("wf-af2")
		b.AddStartNode("start").WithAction(ActionFunc(func(_ context.Context, _ *WorkflowData) error { return nil }))
		b.AddSubWorkflowQueued("sub", "child").WithInput(map[string]any{"deep": deep})
		_, berr := b.Build()
		report(berr)
	case "workflowdata-snapshot":
		// The EXPORTED serializer, which reaches json.Marshal with no store involved.
		d := newData()
		d.SetOutput("n", deep)
		_, serr := d.Snapshot()
		report(serr)
	default:
		fmt.Println("CHILD-SETUP-FAILED: unknown writer " + writer)
	}
}

// TestAF2_CyclicValueIsRefusedNotSpun. Trading an unrecoverable crash for a silent hang
// is not a fix, and a hand-rolled walk with no visited-set is the natural way to write
// one. The bound doubles as the cycle bound only because the walk is NON-TRANSPARENT:
// every link costs a level, so a cycle's depth grows without bound and is refused.
//
// The oracle is not "it errors" — it is "it errors PROMPTLY, on a value json.Marshal
// itself refuses cleanly". A hang would show as the test timing out, which reads as
// infrastructure rather than as this defect, so the deadline is explicit and named.
func TestAF2_CyclicValueIsRefusedNotSpun(t *testing.T) {
	// json.Marshal's own behaviour on this value, asserted rather than assumed: it is
	// what makes a hang strictly worse than doing nothing.
	_, merr := json.Marshal(cyclicValue())
	require.Error(t, merr, "precondition: json.Marshal refuses a cycle cleanly")

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- checkValueDepth(cyclicValue(), "cyclic") }()
	select {
	case err := <-done:
		require.Error(t, err, "a cyclic value must be REFUSED — the depth bound is also the cycle bound")
		require.ErrorIs(t, err, ErrValidation)
		t.Logf("MEASURED: cyclic value refused in %s", time.Since(start))
	case <-time.After(30 * time.Second):
		t.Fatal("checkValueDepth did NOT terminate on a self-referential map. This is the hang the " +
			"non-transparency requirement exists to prevent: some link is being traversed without " +
			"costing a level, so the cycle never reaches the bound")
	}
}

// TestAF2_BranchingCycleDoesNotExhaustMemory is the arm that separates the shipped
// implementation from the one it is easy to write instead, and it would pass on neither
// a recursive walk nor a hang test.
//
// A worklist-based iterative walk is iterative (so it passes the "not recursive"
// requirement) and terminates at the bound (so it passes the cycle test) while still
// being wrong: it holds the whole frontier, so 32 self-edges per level add 31 entries per
// level and it allocates until it dies. The frame stack allocates O(depth).
//
// The assertion is a METAMORPHIC RELATION rather than a byte budget, and the first draft
// of this arm was a budget — a constant with slack in it, which is a number that has to be
// re-tuned on every machine and tells you nothing about the shape when it fails. The
// discriminating property is not "how much" but "what it grows WITH": a frame stack's
// cost is O(depth) and therefore INDEPENDENT of fanout, while a worklist's is O(fanout *
// depth). So measure at two fanouts a factor of 16 apart and assert the ratio is ~1. A
// worklist would show ~16.
func TestAF2_BranchingCycleDoesNotExhaustMemory(t *testing.T) {
	measure := func(fanout int) int64 {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)

		done := make(chan error, 1)
		go func() { done <- checkValueDepth(branchingCycleValue(fanout), "branching") }()
		select {
		case err := <-done:
			require.Error(t, err, "a branching cycle must be refused, not spun on")
			require.ErrorIs(t, err, ErrValidation)
		case <-time.After(60 * time.Second):
			t.Fatalf("checkValueDepth did not terminate on a cycle with fanout %d", fanout)
		}
		runtime.ReadMemStats(&after)
		return int64(after.TotalAlloc - before.TotalAlloc)
	}

	narrow := measure(4)
	wide := measure(64)
	ratio := float64(wide) / float64(narrow)
	t.Logf("MEASURED: fanout 4 → %d bytes; fanout 64 → %d bytes; ratio %.2fx (fanout ratio 16x)",
		narrow, wide, ratio)

	assert.Less(t, ratio, 2.0,
		"allocation is growing with FANOUT, not with DEPTH. That is a WORKLIST holding the whole "+
			"frontier, not a frame stack — and it passes the 'is it recursive' and 'does it terminate' "+
			"arms while still being the wrong shape: raise the fanout and it dies of memory before it "+
			"ever reaches the bound. A frame stack is O(depth) whatever the fanout, so this ratio must "+
			"be ~1 while the fanout ratio is 16")
}

// TestAF2_RefusedWriteLeavesNothingBehind. A refusal that half-wrote is worse than the
// crash it replaced, because the crash at least left the durable state untouched. Every
// store must be byte-unchanged after a refused write.
func TestAF2_RefusedWriteLeavesNothingBehind(t *testing.T) {
	deep := nestValue(maxJSONNestingDepth + 1)
	for name, mk := range af2Stores(t) {
		t.Run(name, func(t *testing.T) {
			store := mk()
			good := NewWorkflowData("wf-leave")
			good.SetOutput("ok", map[string]any{"a": 1})
			require.NoError(t, store.Save(good), "the control write must succeed")

			bad := NewWorkflowData("wf-leave")
			bad.SetOutput("ok", map[string]any{"a": 1})
			bad.SetOutput("deep", deep)
			err := store.Save(bad)
			require.Error(t, err, "an over-depth output must be refused")
			require.ErrorIs(t, err, ErrValidation)

			got, lerr := store.Load("wf-leave")
			require.NoError(t, lerr, "the refused write must have left the previous state loadable")
			_, has := got.GetOutput("deep")
			require.False(t, has, "the refused value must not be durable")
			_, hasOK := got.GetOutput("ok")
			require.True(t, hasOK, "the refusal must not have destroyed the prior state either")
		})
	}
}

// ---------------------------------------------------------------------------
// ACCEPTS — nothing the encoder handles is falsely refused.
//
// This group is the one the fix could plausibly break, and every arm in it would pass on
// the UNFIXED code. That is the point: an obligation derived from the defect cannot see a
// regression the fix introduces.
// ---------------------------------------------------------------------------

// TestAF2_UnexportedFieldsAreNotDescended is the highest-value arm in the file and the
// one that was a DERIVATION until it was measured.
//
// json.Marshal skips unexported fields entirely, so an unexported parent back-pointer in
// a tree is invisible to it — while a walk that descends into it sees a cycle and runs to
// the bound. That is a false refusal on `struct{ Kids []*Node; parent *Node }`, one of
// the most common shapes in Go, on a value the encoder serializes in about a hundred
// bytes.
//
// It asserts BOTH halves, because only the pair is the property: the encoder succeeds AND
// the walk accepts. Asserting the walk alone would pass on a walk that refuses nothing.
func TestAF2_UnexportedFieldsAreNotDescended(t *testing.T) {
	root := &af2Node{}
	cur := root
	for i := 0; i < 8; i++ {
		k := &af2Node{parent: cur}
		cur.Kids = append(cur.Kids, k)
		cur = k
	}
	b, merr := json.Marshal(root)
	require.NoError(t, merr, "precondition: the encoder is perfectly happy with this tree")
	t.Logf("MEASURED: depth-9 back-pointer tree encodes to %d bytes, encoded depth %d",
		len(b), jsonNestingDepth(b))

	require.NoError(t, checkValueDepth(root, "tree"),
		"FALSE REFUSAL. The walk descended into the unexported `parent`, found the cycle the "+
			"encoder cannot see, and refused a value json.Marshal encodes in %d bytes. This is not "+
			"an over-count that slack absorbs — it is unbounded descent on a shape that is legal "+
			"and common", len(b))
}

type af2Node struct {
	Kids   []*af2Node
	parent *af2Node //nolint:unused // the whole point: invisible to the encoder, must be invisible to the walk
}

// TestAF2_WalkDescendsExactlyWhereTheEncoderDoes checks the doc-comment rule case by
// case, as a RELATION rather than as three special cases: for every value here the
// encoder succeeds, so the walk must accept — whatever the reason it does not descend.
//
// Each subject hides an unbounded structure behind something the encoder cannot see, so a
// walk that ignores that rule runs to the bound on all of them.
func TestAF2_WalkDescendsExactlyWhereTheEncoderDoes(t *testing.T) {
	cyc := cyclicValue()

	cases := map[string]any{
		"json:\"-\" field":     &af2Omitted{Kept: 1, Skipped: cyc},
		"unexported field":     &af2Unexported{Kept: 1, hidden: cyc},
		"json.Marshaler":       &af2CustomMarshaler{hidden: cyc},
		"TextMarshaler":        &af2CustomText{hidden: cyc},
		"json.RawMessage":      map[string]any{"raw": json.RawMessage(`{"a":{"b":1}}`)},
		"[]byte is base64":     map[string]any{"blob": make([]byte, 4096)},
		"addressable ptr recv": &af2HoldsPtrMarshaler{V: af2PtrMarshaler{Reached: cyc}},
	}

	for name, v := range cases {
		t.Run(name, func(t *testing.T) {
			b, merr := json.Marshal(v)
			require.NoError(t, merr,
				"precondition: this subject must be one the ENCODER handles, or the arm proves nothing")
			require.NoError(t, checkValueDepth(v, name),
				"FALSE REFUSAL: the walk descended somewhere json.Marshal did not. The encoder produced "+
					"%s", truncate(b))
		})
	}
}

type af2Omitted struct {
	Kept    int
	Skipped any `json:"-"`
}

type af2Unexported struct {
	Kept   int
	hidden any //nolint:unused // must be invisible to the walk, as it is to the encoder
}

type af2CustomMarshaler struct {
	hidden any //nolint:unused // never reached: MarshalJSON replaces the whole value
}

func (af2CustomMarshaler) MarshalJSON() ([]byte, error) { return []byte(`{"custom":true}`), nil }

type af2CustomText struct {
	hidden any //nolint:unused // never reached: MarshalText replaces the whole value
}

func (af2CustomText) MarshalText() ([]byte, error) { return []byte("custom"), nil }

// EXPORTED, and that is the whole point of this subject. An earlier draft hid the cycle
// behind an unexported field, which made the arm VACUOUS: the unexported rule already
// stopped the walk, so inverting the addressability rule changed nothing and the seeded
// mutation went green. Caught by running the bite, not by reading the test.
type af2PtrMarshaler struct {
	Reached any // reached only if the walk ignores addressability
}

func (*af2PtrMarshaler) MarshalJSON() ([]byte, error) { return []byte(`"via-ptr"`), nil }

type af2HoldsPtrMarshaler struct{ V af2PtrMarshaler }

// TestAF2_StringOutputsAndScalarsAreUntouched. A string output is stored verbatim and
// returned verbatim by decodeOutput, so it never passes a decoder and has no nesting one
// could refuse. A guard that started refusing long strings would be refusing on the wrong
// axis entirely — that is the byte ceiling's job.
func TestAF2_StringOutputsAndScalarsAreUntouched(t *testing.T) {
	bracketBomb := strings.Repeat("[", 400_000) // 4x the bound, as a STRING
	for name, mk := range af2Stores(t) {
		t.Run(name, func(t *testing.T) {
			store := mk()
			d := NewWorkflowData("wf-str")
			d.SetOutput("s", bracketBomb)
			d.Set("i", int64(7))
			require.NoError(t, store.Save(d),
				"a %d-character STRING is not a %d-level structure. Refusing it would mean the walk "+
					"is counting bytes it never sees inside a string literal", len(bracketBomb), len(bracketBomb))
			got, err := store.Load("wf-str")
			require.NoError(t, err)
			out, has := got.GetOutput("s")
			require.True(t, has)
			require.Equal(t, bracketBomb, out, "the string must round-trip verbatim")
		})
	}
}

// ---------------------------------------------------------------------------
// AGREES — the walk's bound relates correctly to the readers' bound.
// ---------------------------------------------------------------------------

// TestAF2_BoundIsExactlyTheConstant is the BC-shaped arm, and it exists because a
// BOUNDARY test cannot see a uniformly-tightened guard: it re-derives its own boundary
// and the tightening moves the derivation with it. This one does not re-derive. It
// MEASURES the live walk's trip point by binary search and checks it against the constant
// through a stated derivation, so changing `>` to `>=` moves the measurement while the
// constant stays put, and the two stop agreeing.
//
// The derivation, written out so the expected value is a claim and not a magic number.
// nestValue(n) is n nested []any. The walk is non-transparent AND counts every child,
// including one it will not descend into, so:
//
//	level 1        the outermost []any        (reflect.ValueOf unwraps the top interface)
//	levels 2,3     each subsequent level costs an INTERFACE hop and a SLICE hop
//	final level    the innermost int — a leaf, which still costs the ENCODER a frame and
//	               therefore still costs a level here
//
// so nestValue(n) measures 2n+1, and the walk refuses when that exceeds the bound:
// 2n+1 > B, i.e. the smallest refused n is B/2 for an even B.
//
// The trailing "+1" is not cosmetic. It is an off-by-one this suite's own metamorphic arm
// caught in the UNSAFE direction — the walk was skipping empty innermost containers that
// the encoder still recurses into — and fixing it moved this expectation by one.
func TestAF2_BoundIsExactlyTheConstant(t *testing.T) {
	// The frame arithmetic this arm's expectation rests on, VERIFIED against the live walk
	// rather than trusted, so a red here says which of the two claims broke.
	d, _ := walkFrames(nestValue(50), 1<<30)
	require.Equal(t, 2*50+1, d, "the 2n+1 frame arithmetic the expectation below is derived from")

	measured := af2SmallestRefused(t, nestValue)
	require.Equal(t, maxWalkFrames/2, measured,
		"the walk's MEASURED trip point and maxWalkFrames no longer agree. Either the "+
			"comparison in checkValueDepth moved by a level (which a boundary test that re-derives "+
			"its own boundary cannot see), or the walk stopped costing exactly 2 frames per []any "+
			"level — which would mean it is no longer non-transparent, and the bound is no longer "+
			"a cycle bound")

	// The same relation on the worst-case shape from the crash bisection, so the arm is
	// not characterizing one shape's frame arithmetic.
	require.Equal(t, maxWalkFrames/2, af2SmallestRefused(t, nestMapValue),
		"a nested map must cost the same 2 frames per level as a nested slice: a map hop and an "+
			"interface hop")
}

// af2SmallestRefused binary-searches the LIVE walk for the shallowest value it refuses.
// Mechanical rather than read from the prose — the phase's own boundary was re-derived
// this way and independently reproduced the table it was checked against.
func af2SmallestRefused(t *testing.T, build func(int) any) int {
	t.Helper()
	lo, hi := 1, maxWalkFrames*2
	require.Error(t, checkValueDepth(build(hi), "probe"), "the search's upper bound must be refused")
	require.NoError(t, checkValueDepth(build(lo), "probe"), "the search's lower bound must be accepted")
	for lo < hi {
		mid := (lo + hi) / 2
		if checkValueDepth(build(mid), "probe") != nil {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}

// TestAF2_NothingTheWalkRefusesWasEverWritable is the no-false-refusal property stated
// where it actually bites, and it is what justifies the bound having no knob.
//
// The walk is an ABSURDITY ceiling, not a policy: every value it refuses was ALREADY
// going to be refused by maxJSONNestingDepth, just after a crash instead of before one.
// This asserts it as a measured relation rather than as an argument — the deepest value
// the walk still accepts is itself far past the depth any reader in this package will
// take, so the walk's bound can never be the reason a legal document is rejected.
func TestAF2_NothingTheWalkRefusesWasEverWritable(t *testing.T) {
	deepestAccepted := af2SmallestRefused(t, nestValue) - 1
	b, merr := json.Marshal(nestValue(deepestAccepted))
	require.NoError(t, merr)
	encoded := jsonNestingDepth(b)

	t.Logf("MEASURED: deepest value the walk accepts is %d levels → %d encoded levels; "+
		"maxJSONNestingDepth is %d", deepestAccepted, encoded, maxJSONNestingDepth)
	require.Greater(t, encoded, maxJSONNestingDepth,
		"the walk now accepts values SHALLOWER than the readers' own ceiling, which means the two "+
			"bounds have crossed and the walk has become a policy limit rather than an absurdity one")
	require.Error(t, checkJSONDepth(b, "x"),
		"corollary: even at its own boundary the walk hands on a document the byte guard refuses")
	require.Error(t, decodeLikeTheRealReader(b),
		"and the REAL reader refuses it too — the relation is against the decoder, not only "+
			"against our own constant")
}

// TestAF2_BothAxesAreLiveAndNeitherSubsumesTheOther. The pre-marshal walk must ADD to
// checkJSONDepth, never replace it. The two refuse different things, and a fix that
// substituted one for the other would pass every crash arm above while silently reopening
// the wedge the phase's original guard closed.
//
// The witness for "only the byte axis sees it" is the ENVELOPE, which is F2's own central
// design finding: a payload measured on its own is off by exactly the level its wrapper
// adds. This is the case that made the original guard measure bytes rather than values,
// and it is untouched by AF2 — the walk cannot see it, by construction, because the
// envelope does not exist yet when the walk runs.
//
// (The case a reader will reach for first — a custom MarshalJSON returning an over-deep
// document — is NOT a witness, and finding that out is why this arm is written on the
// envelope instead. MEASURED: json.Marshal compacts a marshaler's output through the same
// scanner the decoder uses, so it returns `invalid character '[' exceeded max depth` at
// 10001 all by itself. A custom marshaler cannot produce an over-deep document at all.)
func TestAF2_BothAxesAreLiveAndNeitherSubsumesTheOther(t *testing.T) {
	// ONLY THE BYTE AXIS. A payload at exactly the readers' ceiling: the walk accepts it
	// (2*10^4 frames, far under the bound), it marshals standalone at exactly 10^4 — and
	// the JSON store's signalWire wrapper makes the document 10^4+1, which only a measure
	// of the ENCODED BYTES can see.
	payload := nestValue(maxJSONNestingDepth)
	require.NoError(t, checkValueDepth(payload, "payload"),
		"precondition: the crash-axis walk has no opinion about a 10^4-deep value")
	standalone, err := json.Marshal(payload)
	require.NoError(t, err)
	require.NoError(t, checkJSONDepth(standalone, "payload"),
		"precondition: standalone, this payload is exactly legal — which is why the FlatBuffers "+
			"and SQLite stores accept it")

	wire, err := json.Marshal(signalWire(Signal{ID: "s", Name: "n", Payload: payload}))
	require.NoError(t, err)
	require.Error(t, checkJSONDepth(wire, "signal"),
		"the envelope costs a level and pushes the SAME payload over the ceiling. Only the byte "+
			"measure sees this — removing checkJSONDepth in favour of the pre-marshal walk would "+
			"reopen the wedge on the one store whose wire format has a wrapper")

	// ONLY THE VALUE AXIS: a value that kills the encoder before any bytes exist. Asserted
	// at the walk's own boundary rather than at 10^6 so it stays an in-process arm.
	over := nestValue(af2SmallestRefused(t, nestValue))
	require.Error(t, checkValueDepth(over, "x"),
		"and the walk is the only thing that runs before json.Marshal does")
}

// ---------------------------------------------------------------------------
// THE BREAKING CHANGE — stated as a test so it cannot ship as a silent tightening.
// ---------------------------------------------------------------------------

// TestAF2_NodeOutputsAreNowCappedAtTheDecoderLimit records a BEHAVIOUR CHANGE, not a
// bug fix, and it is here so that the change is discoverable from the suite rather than
// only from a changelog.
//
// BEFORE: the FlatBuffers and SQLite stores marshalled a node output (and a complex data
// value) with no depth guard at all. A 50,000-level-deep output was accepted and written.
// It could even be read back by those two stores, which keep the output as an un-decoded
// string — so this was not, for them, a wedge.
//
// AFTER: it is refused with ErrValidation at 10^4, the same ceiling every other write
// path in the package uses.
//
// The reason it is a tightening worth taking rather than a gratuitous one: the same value
// through JSONFileStore was ALREADY a wedge (its snapshot is decoded on load), and
// WorkflowData.Snapshot — the exported serializer any host may call on any data — has no
// store at all. Leaving two stores more permissive than the third means a workflow that
// runs on FlatBuffers stops loading the day it is pointed at JSONFileStore, which is a
// migration failure rather than a validation error. Uniform is the property; 10^4 is
// where the decoder puts it.
// WHAT EACH STORE CAN SAY IS NOT UNIFORM, and this arm asserts the difference rather than
// asserting the weakest common message. The FlatBuffers and SQLite stores encode each
// value SEPARATELY, so their refusal names the node or the key. JSONFileStore marshals the
// whole snapshot in one call, so its guard measures one document and can only name the
// WORKFLOW. That is pre-existing and is the same structural fact that makes this store's
// usable depth one shallower than the other two (the envelope), not something AF2
// introduced — but it is a real difference in how actionable the refusal is, and it is
// recorded here rather than smoothed over by asserting only "an error happened".
func TestAF2_NodeOutputsAreNowCappedAtTheDecoderLimit(t *testing.T) {
	deep := nestValue(maxJSONNestingDepth + 1) // one level past the readers' ceiling

	// The refusal names the value on the two stores that encode per-value.
	namesTheValue := map[string]bool{"FlatBuffersStore": true, "SQLiteStore": true, "JSONFileStore": false}

	for name, mk := range af2Stores(t) {
		t.Run(name+"/output", func(t *testing.T) {
			d := NewWorkflowData("wf-bc")
			d.SetOutput("n", deep)
			err := mk().Save(d)
			require.Error(t, err, "BEHAVIOUR CHANGE: this write was ACCEPTED before AF2 on the "+
				"FlatBuffers and SQLite stores — a 50,000-deep output was durable and, for those two, "+
				"readable, because they keep the output as an un-decoded string")
			require.ErrorIs(t, err, ErrValidation)
			if namesTheValue[name] {
				require.Contains(t, err.Error(), `output of node "n"`,
					"the refusal must name WHICH value it rejected: a host saving twenty outputs that "+
						"is told only that a document is too deep cannot act on it")
			} else {
				require.Contains(t, err.Error(), "wf-bc",
					"this store measures one whole-snapshot document, so it names the workflow rather "+
						"than the value — stated so the divergence is recorded, not discovered")
			}
		})
		t.Run(name+"/data", func(t *testing.T) {
			d := NewWorkflowData("wf-bc")
			d.Set("k", deep)
			err := mk().Save(d)
			require.Error(t, err)
			require.ErrorIs(t, err, ErrValidation)
			if namesTheValue[name] {
				require.Contains(t, err.Error(), `data key "k"`)
			}
		})
	}

	// A shallow value still writes on every store. Without this the arm above passes on a
	// guard that refuses everything — and that is not hypothetical here, since the walk's
	// non-transparency means it counts levels the encoded document does not show.
	//
	// The depth is 100 rather than "one under the ceiling" for a reason found by running
	// it: JSONFileStore serializes with MarshalIndent, whose whitespace is O(depth^2), so
	// a 9,996-deep value produces a 200 MB document and is refused by the 64 MiB BYTE
	// ceiling — a true refusal on a different axis, which would have made this arm red for
	// a reason that has nothing to do with depth.
	for name, mk := range af2Stores(t) {
		t.Run(name+"/an-ordinary-value-still-writes", func(t *testing.T) {
			d := NewWorkflowData("wf-bc-ok")
			d.SetOutput("n", nestValue(100))
			d.Set("k", map[string]any{"a": []any{1, 2, map[string]any{"b": true}}})
			require.NoError(t, mk().Save(d))
		})
	}
}

// af2Stores returns the three serializing stores behind a constructor, so each subtest
// gets a fresh one. InMemoryStore is excluded: it serializes nothing (FID-01), so it has
// no marshal site and no vector.
func af2Stores(t *testing.T) map[string]func() WorkflowStore {
	t.Helper()
	dir := t.TempDir()
	n := 0
	fresh := func() string {
		n++
		d := filepath.Join(dir, fmt.Sprintf("s%d", n))
		require.NoError(t, os.MkdirAll(d, 0o755))
		return d
	}
	return map[string]func() WorkflowStore{
		"FlatBuffersStore": func() WorkflowStore {
			s, err := NewFlatBuffersStore(fresh())
			require.NoError(t, err)
			return s
		},
		"JSONFileStore": func() WorkflowStore {
			s, err := NewJSONFileStore(fresh())
			require.NoError(t, err)
			return s
		},
		"SQLiteStore": func() WorkflowStore {
			s, err := NewSQLiteStore(filepath.Join(fresh(), "s.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // fixture teardown
			return s
		},
	}
}

// ---------------------------------------------------------------------------
// SOUNDNESS OF THE BOUND ITSELF — the self-verifying arm.
// ---------------------------------------------------------------------------

// af2StackEnv switches the child that proves the bound is sound on THIS box.
const af2StackEnv = "AF2_STACK_PROBE"

// af2Chain is the WORST-CASE shape for the bound and the only correct subject for this
// arm: one walk frame == one JSON level == one encoder recursion, so its over-report ratio
// is 1.0 and a value at walk-depth B recurses B deep inside json.Marshal.
//
// USING A []any CHAIN HERE WOULD MAKE THE ARM PASS WHILE THE BOUND IS UNSOUND, and that is
// not hypothetical — it is a units error that already produced a wrong published floor. An
// []any chain costs TWO walk frames per JSON level, so a value at walk-depth B only
// recurses B/2 deep and the arm would test half the bound it claims to test.
type af2Chain []af2Chain

func mkAF2Chain(n int) af2Chain {
	c := af2Chain{}
	for i := 0; i < n; i++ {
		c = af2Chain{c}
	}
	return c
}

// TestAF2_BoundIsSoundOnThisBox is the arm that makes maxWalkFrames a MEASURED
// bound rather than a number with a rationale, and it is the answer to "amd64 is untested".
//
// THE PROPERTY: every value this package ACCEPTS must survive the consumers it will be
// handed to. The deepest accepted value sits at exactly maxWalkFrames walk frames,
// so the arm builds precisely that and hands it to both recursing classes — json.Marshal
// and reflect.DeepEqual — in a CHILD PROCESS on the box's LIVE stack limit.
//
// WHY A CHILD: the failure being excluded is `fatal error: stack overflow`, which is not a
// panic. An in-process arm cannot observe it; it would simply take the suite down. The
// child's EXIT STATUS is the observation.
//
// WHY NOT AN ARITHMETIC CHECK: an earlier version of this requirement was
// `B x bytes_per_level < effective_stack`, computed. That needs a bisection to obtain the
// per-level cost plus a power-of-two rounding step — two moving parts that can drift out
// of sync with the thing they check, inside a test whose whole job is to not drift. The
// inequality survives as the SUMMARY's explanation, because it is what lets a reader
// re-derive the floor; the floor itself is asserted by running the real encoder at the
// real bound on the real box.
//
// It self-verifies across ARCHITECTURE, GO VERSION and STACK LIMIT — strictly more than
// maxJSONNestingDepth's mirror test, which self-verifies only across Go versions. On amd64
// it runs and tells the truth without anyone having measured there first.
//
// MEASURED FLOOR on darwin/arm64 go1.25.1, worst-case shape: B = 32,768 survives at a
// 32 MiB effective stack and DIES at 16 MiB. 32 MiB is the documented minimum.
func TestAF2_BoundIsSoundOnThisBox(t *testing.T) {
	if op := os.Getenv(af2StackEnv); op != "" {
		af2RunStackChild(op)
		return
	}
	if testing.Short() {
		t.Skip("forks two children that each build a value at the bound; skipped under -short")
	}
	for _, op := range []string{"marshal", "deepequal"} {
		t.Run(op, func(t *testing.T) {
			exe, err := os.Executable()
			require.NoError(t, err)
			cmd := exec.Command(exe, "-test.run", "^"+t.Name()+"$") //nolint:gosec // exe is this test binary
			cmd.Env = append(os.Environ(), af2StackEnv+"="+op)
			out, runErr := cmd.CombinedOutput()
			text := string(out)

			require.NoError(t, runErr,
				"maxWalkFrames = %d IS UNSOUND ON THIS BOX. A value at exactly the bound — which "+
					"this package ACCEPTS and hands straight to %s — exhausted the goroutine stack. That is "+
					"the AF2 defect surviving the AF2 fix: the walk passes the value and the process dies.\n\n"+
					"This is a property of the HOST'S STACK, not of the value. Go grows stacks by doubling, so "+
					"the usable limit is the largest power of two <= the configured one; the documented minimum "+
					"is 32 MiB of EFFECTIVE stack. If this box runs under debug.SetMaxStack below that, either "+
					"raise the limit or lower maxWalkFrames — the soundness condition is "+
					"B x bytes_per_walk_frame < effective_stack, with ~646 B/frame for json.Marshal and ~465 "+
					"for reflect.DeepEqual.\n\nChild output:\n%s", maxWalkFrames, op, text)
			require.Contains(t, text, "SURVIVED",
				"the child neither died nor reported; this arm cannot interpret that. Output:\n%s", text)
		})
	}
}

func af2RunStackChild(op string) {
	// NO debug.SetMaxStack call: the whole point is to exercise the box's LIVE limit,
	// whatever the host or the test runner has configured.
	c := mkAF2Chain(maxWalkFrames)
	switch op {
	case "marshal":
		b, err := json.Marshal(c)
		fmt.Printf("SURVIVED marshal bytes=%d err=%v\n", len(b), err)
	case "deepequal":
		fmt.Printf("SURVIVED deepequal equal=%v\n", reflect.DeepEqual(c, mkAF2Chain(maxWalkFrames)))
	}
}

// TestAF2_TheFloorIsAShapeFamilyNotANumber is the FLOOR half, and it asserts a MEASURED
// BOUNDARY rather than the blanket property this arm first claimed.
//
// 🔴 THE FIRST VERSION ASSERTED "the bound never refuses a legal document" AND THAT IS
// FALSE. It passed only because its three shapes all over-report by 2.005x. The walk is
// non-transparent, so EVERY pointer wrapper adds one frame per level while adding ZERO
// encoded depth — the ratio is UNBOUNDED in principle, and no finite bound accepts every
// document maxJSONNestingDepth calls legal.
//
// So the honest contract is a line, not a guarantee, and this arm pins WHERE THE LINE IS:
//
//	[]any / map[string]any    2.005 frames/level  -> ACCEPTED at the legal ceiling
//	*[]any / *map            3.005               -> ACCEPTED (headroom only 1.09x)
//	**[]any                  4.005               -> REFUSED
//	***[]any                 5.005               -> REFUSED
//
// A red here means the line MOVED. That is a real change to what the library accepts and it
// must be a decision, not a side effect of retuning a constant — which is exactly what a
// blanket "never refuses" assertion would have hidden.
func TestAF2_TheFloorIsAShapeFamilyNotANumber(t *testing.T) {
	cases := []struct {
		name       string
		build      func(int) any
		wantFrames float64 // frames per JSON level, measured
		wantAccept bool    // at a document of exactly maxJSONNestingDepth encoded levels
	}{
		{"[]any", func(n int) any {
			var v any = 1
			for i := 0; i < n; i++ {
				v = []any{v}
			}
			return v
		}, 2.005, true},
		{"map[string]any", func(n int) any {
			var v any = 1
			for i := 0; i < n; i++ {
				v = map[string]any{"k": v}
			}
			return v
		}, 2.005, true},
		{"*[]any — one pointer wrapper, the d.Set(k, &slice) idiom", func(n int) any {
			var v any = 1
			for i := 0; i < n; i++ {
				sl := []any{v}
				v = &sl
			}
			return v
		}, 3.005, true},
		// The REALISTIC shape, and the reason this set is not just integer wrapper counts:
		// a host wraps SOME values and not others, which lands the ratio between the whole
		// numbers. If the arm only ever tested 2x/3x/4x it would miss a widening that
		// creeps up fractionally.
		{"[]any of *[]any — alternate wrapping, a FRACTIONAL ratio", func(n int) any {
			var v any = 1
			for i := 0; i < n; i++ {
				sl := []any{v}
				p := &sl
				v = []any{p}
			}
			return v
		}, 2.502, true},
		// EMBEDDING, BOTH WAYS — and the pair is the point. This arm previously had only
		// the sibling case and was NAMED "NOT an inflator", which asserted a universal
		// from one shape: the embedded field was never on the recursion path, so of course
		// it cost nothing. Chained THROUGH the embed it inflates exactly like a pointer,
		// because a promoted field costs the walk a frame and the encoder no bracket.
		{"embed as a SIBLING — off the path, so it costs nothing", func(n int) any {
			var v any = 1
			for i := 0; i < n; i++ {
				v = af2Embed{V: v}
			}
			return v
		}, 2.005, true},
		{"embed ON THE PATH — inflates like a pointer", func(n int) any {
			var v any = 1
			for i := 0; i < n; i++ {
				v = af2EmbedPath{af2EmbedPathInner{V: v}}
			}
			return v
		}, 3.005, true},
		{"**[]any — two wrappers, PAST the line", func(n int) any {
			var v any = 1
			for i := 0; i < n; i++ {
				sl := []any{v}
				p := &sl
				v = &p
			}
			return v
		}, 4.005, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The ratio, measured on a SMALL value. It is linear in n, and building the
			// full-depth document in-process would hand json.Marshal the very value this
			// package exists to keep away from it.
			probe := tc.build(200)
			w, exceeded := walkFrames(probe, 1<<30)
			require.False(t, exceeded)
			b, err := json.Marshal(probe)
			require.NoError(t, err)
			enc := jsonNestingDepth(b)
			ratio := float64(w) / float64(enc)
			require.InDelta(t, tc.wantFrames, ratio, 0.01,
				"the walk's frames-per-JSON-level for this shape has CHANGED. The floor is derived "+
					"from this ratio, so a change here moves what the library accepts")

			projected := ratio * float64(maxJSONNestingDepth)
			accepted := projected <= float64(maxWalkFrames)
			t.Logf("MEASURED: %.3f frames/level -> ~%.0f frames at the legal ceiling vs bound %d (headroom %.2fx)",
				ratio, projected, maxWalkFrames, float64(maxWalkFrames)/projected)
			require.Equal(t, tc.wantAccept, accepted,
				"THE LINE MOVED. This shape is now %s at the legal ceiling and the arm expected the "+
					"opposite. Changing maxWalkFrames changes which legal documents the library refuses; "+
					"that is a product decision, not a retune",
				map[bool]string{true: "ACCEPTED", false: "REFUSED"}[accepted])
		})
	}
}

type af2PtrLink struct{ Next *af2PtrLink }

type af2Embed struct {
	af2EmbedInner
	V any
}

type af2EmbedInner struct{ Pad int }

// The recursion path runs THROUGH the embedded field here, unlike af2Embed.
type af2EmbedPath struct{ af2EmbedPathInner }

type af2EmbedPathInner struct{ V any }

// TestAF2_WalkNeverUnderReportsRelativeToTheEncoder is P1: the metamorphic relation that
// makes "must ADD, never REPLACE" falsifiable instead of a slogan.
//
// THE PROPERTY, and the direction matters: the walk must never report LESS than the
// encoded document's depth, because the bound's soundness rests on the walk seeing at
// least as much nesting as the encoder will recurse through. Over-reporting is safe
// (it refuses early); under-reporting would pass a value the encoder then dies on.
//
// 🔴 THE ENVELOPE TRAP, pre-positioned so a red here is not "fixed" the wrong way. At sites
// that WRAP the value — the JSON store's signalWire is the one — the encoded document is
// DEEPER than the value by exactly the envelope, so walk and encoded depth legitimately
// disagree there. That disagreement is CORRECT and it belongs to checkJSONDepth, which
// measures the bytes precisely because "our envelopes consume part of the decoder's
// budget". Tuning the walk to close it would be calibrating a CRASH-axis instrument to
// compensate for a WEDGE-axis wrapper, leaving it answering neither question.
//
// So the arm asserts the relation UP TO a named envelope depth per site, never equality.
func TestAF2_WalkNeverUnderReportsRelativeToTheEncoder(t *testing.T) {
	corpus := map[string]any{
		"nested any":      nestValue(50),
		"nested map":      nestMapValue(50),
		"ratio-1.0 chain": mkAF2Chain(50),
		"pointer chain": func() any {
			c := &af2PtrLink{}
			for i := 0; i < 50; i++ {
				c = &af2PtrLink{Next: c}
			}
			return c
		}(),
		"wide shallow":  map[string]any{"a": []any{1, 2, 3}, "b": map[string]any{"c": true}},
		"realistic run": benchRealisticRun(),
		"unexported parent": func() any {
			r := &af2Node{}
			c := r
			for i := 0; i < 8; i++ {
				k := &af2Node{parent: c}
				c.Kids = append(c.Kids, k)
				c = k
			}
			return r
		}(),
	}
	for name, v := range corpus {
		t.Run(name, func(t *testing.T) {
			b, err := json.Marshal(v)
			require.NoError(t, err)
			encoded := jsonNestingDepth(b)
			walk, exceeded := walkFrames(v, 1<<30)
			require.False(t, exceeded)
			t.Logf("MEASURED: walk=%d encoded=%d", walk, encoded)
			require.GreaterOrEqual(t, walk, encoded,
				"THE WALK UNDER-REPORTED. It saw %d levels where the encoder produced %d, which means some "+
					"link the encoder recurses through costs the walk nothing — so a value can pass the bound "+
					"and still exhaust the stack. This is the one direction that is unsafe; over-reporting is "+
					"not. Do NOT relax this arm to accommodate a new skip rule; the skip rule is wrong",
				walk, encoded)
		})
	}

	// THE ENVELOPE, asserted as a NAMED quantity rather than tolerated as slack.
	t.Run("signalWire envelope is +1 and belongs to checkJSONDepth", func(t *testing.T) {
		payload := nestValue(50)
		walk, _ := walkFrames(payload, 1<<30)

		standalone, err := json.Marshal(payload)
		require.NoError(t, err)
		bare := jsonNestingDepth(standalone)

		wire, err := json.Marshal(signalWire(Signal{ID: "s", Name: "n", Payload: payload}))
		require.NoError(t, err)
		wrapped := jsonNestingDepth(wire)

		t.Logf("MEASURED: walk=%d payload-encoded=%d wire-encoded=%d envelope=+%d",
			walk, bare, wrapped, wrapped-bare)
		require.Equal(t, 1, wrapped-bare,
			"the signalWire envelope must cost EXACTLY one level; if this changes, the JSON store's usable "+
				"payload depth changes with it and checkJSONDepth is the only guard that can see it")
		require.Less(t, walk, wrapped+walk,
			"sanity: the walk measures the VALUE and cannot see the envelope at all — which is the whole "+
				"reason checkJSONDepth measures BYTES and must not be replaced by this walk")
	})
}

// ---------------------------------------------------------------------------
// THE reflect.DeepEqual CLASS — a crash with no marshal anywhere in it.
// ---------------------------------------------------------------------------

// af2DeepEqualEnv switches the child that drives the DeepEqual class.
const af2DeepEqualEnv = "AF2_DEEPEQUAL"

// TestAF2_DeepEqualClassIsGuarded covers the class the marshal census structurally could
// not express, and it exists because a seeded mutation found the guards had NO arm at all.
//
// WHY NO MARSHAL GUARD REACHES THIS ON THE BACKEND THAT REPRODUCES IT, which is what makes
// it a separate class rather than another site: on InMemoryStore the value never meets an
// encoder before it meets reflect.DeepEqual. Save clones via Clone() and never marshals;
// cloneMap is iterative, so the clone costs heap rather than stack and cannot overflow
// either; and the reproduction depth is BELOW json.Marshal's own death depth. Three
// independent legs, none of them a marshal.
//
// 116-GC-F1: the qualifier is load-bearing and used to be missing. On a MARSHALLING backend
// the result operand does meet an encoder — it arrives via store.Load — which is exactly why
// the residual is InMemoryStore-only. Stated generally, this sentence is the same
// store-specific-written-as-general shape the phase has now corrected seven times.
//
// 🔴 THE CRASH IS ON THE SUCCESS PATH, and that is the severity rather than a detail.
// Reaching DeepEqual at all means a value is already present under the result key; if the
// two are EQUAL the collision check returns nil and the run proceeds normally. So this is
// an ordinary IDEMPOTENT RE-APPLY of a sub-workflow result — what a crash-resume does
// every time it replays a completed child — not an error path and not an adversarial shape.
//
// It runs in a child process because the pre-fix failure is `fatal error: stack overflow`:
// not a panic, not recoverable, and it would take the whole suite down in-process.
func TestAF2_DeepEqualClassIsGuarded(t *testing.T) {
	if op := os.Getenv(af2DeepEqualEnv); op != "" {
		af2RunDeepEqualChild(op)
		return
	}
	if testing.Short() {
		t.Skip("forks children that build very deep values; skipped under -short")
	}
	for _, op := range []string{"subworkflow-result", "fanout-branch-result"} {
		t.Run(op, func(t *testing.T) {
			exe, err := os.Executable()
			require.NoError(t, err)
			cmd := exec.Command(exe, "-test.run", "^"+t.Name()+"$") //nolint:gosec // exe is this test binary
			cmd.Env = append(os.Environ(), af2DeepEqualEnv+"="+op)
			out, runErr := cmd.CombinedOutput()
			text := string(out)

			require.NotContains(t, text, "fatal error: stack overflow",
				"the %s path still reaches reflect.DeepEqual with a deep value. DeepEqual RECURSES, and no "+
					"marshal guard covers this path — InMemoryStore clones rather than marshalling. Child "+
					"output:\n%s", op, text)
			require.NoError(t, runErr, "the child must survive and exit 0. Output:\n%s", text)
			require.Contains(t, text, "ERRIS-VALIDATION=true",
				"the %s path must refuse in the VALIDATION domain rather than crashing or succeeding "+
					"silently. Output:\n%s", op, text)
		})
	}
}

func af2RunDeepEqualChild(op string) {
	// Deep enough to kill DeepEqual, and deliberately BELOW json.Marshal's own measured
	// death depth so a crash here cannot be blamed on an encoder.
	const deepN = 650_000
	report := func(err error) {
		fmt.Printf("CHILD-RETURNED: err=%v\nERRIS-VALIDATION=%v\n", err, errors.Is(err, ErrValidation))
	}

	switch op {
	case "subworkflow-result":
		// The reproduced path, driven through the PUBLIC builder API only.
		store := NewInMemoryStore()
		cb := NewWorkflowBuilder()
		cb.AddStartNode("produce").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
			d.Set("result", nestValue(deepN))
			return nil
		}))
		child, err := cb.Build()
		if err != nil {
			fmt.Println("CHILD-SETUP-FAILED: child build")
			return
		}
		pb := NewWorkflowBuilder().WithWorkflowID("wf-af2-de")
		// THE PRE-EXISTING EQUAL VALUE IS WHAT MAKES THIS THE REAL DEFECT AND NOT MERELY A
		// GUARD-PRESENCE CHECK, and leaving it out was a live vacuity in this arm's first
		// draft. The collision check is `present && !reflect.DeepEqual(...)`: with no value
		// already under the key, `present` is false and Go's && short-circuits before
		// DeepEqual is ever called, so a neutered guard returns nil instead of crashing.
		//
		// Seeding an EQUAL deep value reproduces the ordinary idempotent re-apply — what a
		// crash-resume does every time it replays a completed child — and it is the SUCCESS
		// path: the two values are equal, so with a working guard removed DeepEqual
		// recurses all the way and dies while the collision check was going to return nil.
		pb.AddStartNode("before").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
			d.Set("result", nestValue(deepN))
			return nil
		}))
		pb.AddSubWorkflow("sub", child).DependsOn("before").WithResult("result", "result")
		dag, err := pb.Build()
		if err != nil {
			fmt.Println("CHILD-SETUP-FAILED: parent build")
			return
		}
		w, err := FromBuilder(pb)
		if err != nil {
			fmt.Println("CHILD-SETUP-FAILED: FromBuilder")
			return
		}
		w.Store = store
		w.WorkflowID = "wf-af2-de"
		w.dag = dag
		report(w.Execute(context.Background()))

	case "fanout-branch-result":
		// The sibling site, driven end-to-end through the PUBLIC builder API for the same
		// reason the sub-workflow arm is: reachability from outside is established by
		// construction rather than argued. A branch returns a deep value, which lands in
		// results[i] and reaches the per-index collision check.
		store := NewInMemoryStore()
		expander := func(_ context.Context, _ *WorkflowData) ([]interface{}, error) {
			return []interface{}{0}, nil
		}
		branch := ActionFunc(func(_ context.Context, d *WorkflowData) error {
			d.Set("out", nestValue(deepN))
			return nil
		})
		b := NewWorkflowBuilder().WithWorkflowID("wf-af2-de-fo")
		// SEED an equal deep value under the index key first. The guard now runs only
		// inside the `present` branch — i.e. exactly when reflect.DeepEqual actually runs
		// — so a first apply with no collision has nothing to bound and nothing to refuse.
		// Without this the arm exercises a path the guard is deliberately not on.
		b.AddStartNode("before").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
			d.Set(fanOutResultIndexKey("r", 0), nestValue(deepN))
			return nil
		}))
		b.AddFanOut("fan", expander, branch).DependsOn("before").WithResults("r", "out")
		dag, err := b.Build()
		if err != nil {
			fmt.Println("CHILD-SETUP-FAILED: fanout build")
			return
		}
		w, err := FromBuilder(b)
		if err != nil {
			fmt.Println("CHILD-SETUP-FAILED: FromBuilder")
			return
		}
		w.Store = store
		w.WorkflowID = "wf-af2-de-fo"
		w.dag = dag
		report(w.Execute(context.Background()))
	}
}

// ---------------------------------------------------------------------------
// THE COMPLETENESS ARGUMENT, made re-runnable.
// ---------------------------------------------------------------------------

// af2NamedChain is the only shape that costs ONE walk frame per JSON level: a named
// recursive container, reached without an interface hop.
type af2NamedChain []af2NamedChain

type af2WrapS struct{ V any }

// af2EmbPath chains THROUGH a promoted embedded field.
type af2EmbPath struct{ af2EmbPathInner }
type af2EmbPathInner struct{ V any }

// TestAF2_InflationPerLinkKindIsPinned pins the inflation each link kind contributes.
//
// 🔴 IT WAS NAMED TestAF2_InflationIsExactlyTheTransparentTraversals AND THAT NAME WAS A
// FALSE UNIVERSAL. Independent review found a fourth mechanism the name excluded:
// encoding/json's typeFields DROPS fields whose json names collide, and the walk descends
// them — 4004x measured, from a shape with no wrapper at all. The body was correct about
// every case it measured; the NAME claimed the set was closed.
//
// That is this unit's own lesson landing on the arm written to close the question: a test
// name is a claim and nothing type-checks it. The name now says what the body pins.
//
// ENUMERATION CANNOT PROVE ABSENCE. Fuzzing 6,000 shapes found no third mechanism, which
// only means the grammar I wrote did not contain one — a grammar is an enumeration of what
// I thought to include. What CAN close the question is a difference of CASE SETS:
//
//	INFLATION = (frames the walk charges) - (brackets the encoder emits)
//
// The walk must charge for EVERY structural link or it stops being a cycle bound (see
// checkValueDepth). The encoder emits a bracket only for map, struct, slice and array.
//
// 🔴 THAT DIFFERENCE GIVES *TWO* INFLATION FAMILIES, NOT ONE — and an earlier version of
// this block said "exactly the set of links json.Marshal traverses TRANSPARENTLY", which
// is the universal MAJOR 3 refuted. Renaming the test fixed the identifier and left the
// refuted sentence here and in the failure message below, so two files asserted mutually
// exclusive claims about the same set and the surviving one was the false one.
//
//	TRANSPARENT TRAVERSALS — pointer deref, interface unwrap, anonymous-field promotion.
//	DROPPED FIELDS         — typeFields discards colliding json names and shadowed fields;
//	                         the walk descends them. 4005x measured, no wrapper involved.
//
// The encoder's field SELECTION lives in a THIRD function, and A DIFFERENCE OF CASE SETS
// IS ONLY COMPLETE OVER THE FUNCTIONS YOU DIFFERENCED. What survives is the property that
// matters: the walk never UNDER-reports — both families make it report MORE.
//
// Marshaler/TextMarshaler REPLACE the subtree, so the walk stops there; unexported and
// `json:"-"` are skipped by both.
//
// 🔴 WHY THIS IS A TEST AND NOT A PARAGRAPH: the argument is complete only against the
// encoder's CURRENT set of transparent traversals. A future encoding/json that traverses
// something new transparently would add a mechanism, and the prose would still read as a
// proof. This arm measures the deltas on the live encoder, so that change reds here
// instead of silently widening the family.
func TestAF2_InflationPerLinkKindIsPinned(t *testing.T) {
	inner := map[string]any{"base": []any{1}}
	baseFrames, exceeded := walkFrames(inner, 1<<30)
	require.False(t, exceeded)
	b, err := json.Marshal(inner)
	require.NoError(t, err)
	baseBrackets := jsonNestingDepth(b)

	cases := []struct {
		name          string
		wrap          func(any) any
		wantInflation int // frames charged MINUS brackets emitted
		why           string
	}{
		{"named recursive container (no interface hop)", func(v any) any {
			// The 1.0 shape: the element type IS the container, so no `any` is involved.
			// Reached via its own chain rather than by wrapping inner.
			return af2NamedChain{af2NamedChain{}}
		}, 0, "a container emits the bracket it charges for"},

		{"slice via any", func(v any) any { return []any{v} }, 1, "the `any` boxing is an interface hop"},
		{"array via any", func(v any) any { return [1]any{v} }, 1, "same"},
		{"map via any", func(v any) any { return map[string]any{"k": v} }, 1, "same"},
		{"struct via any", func(v any) any { return af2WrapS{V: v} }, 1, "same"},

		{"POINTER over a struct", func(v any) any { s := af2WrapS{V: v}; return &s }, 2,
			"pointer deref is transparent to the encoder"},
		{"POINTER over a map", func(v any) any { m := map[string]any{"k": v}; return &m }, 2, "same"},
		{"PROMOTED EMBED on the path", func(v any) any { return af2EmbPath{af2EmbPathInner{V: v}} }, 2,
			"anonymous-field promotion is transparent to the encoder"},
	}

	seen := map[int]int{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := tc.wrap(inner)
			f, ex := walkFrames(v, 1<<30)
			require.False(t, ex)
			enc, merr := json.Marshal(v)
			require.NoError(t, merr)

			df := f - baseFrames
			db := jsonNestingDepth(enc) - baseBrackets
			if tc.wantInflation == 0 {
				// The named-chain case does not wrap `inner`, so measure it standalone.
				df, db = f, jsonNestingDepth(enc)
			}
			inflation := df - db
			t.Logf("MEASURED: Δframes=%d Δbrackets=%d inflation=%d — %s", df, db, inflation, tc.why)
			require.Equal(t, tc.wantInflation, inflation,
				"THE INFLATION SET HAS CHANGED. Inflation is (frames the walk charges) minus (brackets "+
					"the encoder emits). This arm pins it PER LINK KIND — it does NOT claim the set of "+
					"inflating mechanisms is closed, and an earlier version of this message did: "+
					"typeFields also DROPS fields, which inflates without any traversal. A change "+
					"here means either the walk stopped charging for a link (which breaks the CYCLE bound, "+
					"see checkValueDepth) or encoding/json gained/lost a transparent traversal (which "+
					"changes the over-report family and therefore which legal documents are refused). "+
					"Neither is a test to retune")
			seen[tc.wantInflation]++
		})
	}

	// ANTI-VACUITY: the table must actually exercise all three classes, or "inflation is
	// exactly the transparent traversals" is asserted over a sample that contains only one
	// of them — the shape this suite has been bitten by six times.
	require.Positive(t, seen[0], "no zero-inflation (bracket-emitting, no-interface) case exercised")
	require.Positive(t, seen[1], "no single-inflation (interface hop) case exercised")
	require.Positive(t, seen[2], "no double-inflation (transparent traversal) case exercised")
}

// af2PtrCycle is a PURE pointer cycle — legal Go, and the minimal cycle whose every link
// is one the encoder traverses transparently.
type af2PtrCycle *af2PtrCycle

// af2SelfEmb is a PURE promoted-embed cycle.
type af2SelfEmb struct{ *af2SelfEmb }

// TestAF2_TheCycleBoundHoldsForEveryLinkType closes a gap in this suite's own coverage:
// TestAF2_CyclicValueIsRefusedNotSpun tests a MAP cycle and nothing else, so it asserts a
// family property from one member — the shape this unit has been bitten by repeatedly.
//
// The cycle bound is not a property of maps. It follows from NON-TRANSPARENCY: every
// structural link costs a frame, so any cycle re-traverses links and its depth grows
// without bound. That argument is about LINKS, so the arm must cover every link a cycle
// can be built from — including the two the encoder traverses transparently, which are
// exactly the ones a "tidy" change would stop charging for.
//
// 🔴 THE EMBED CASE ALSO PINS A KNOWN FALSE REFUSAL, and it is here rather than in a
// comment because it is the accepted cost of the whole design: json.Marshal SUCCEEDS on a
// self-embedding cycle — it emits `{}` — while the walk REFUSES it. That is
// over-refusal in the safe direction, and it is the measurement that decided the walk
// would NOT collapse embedded fields to the parent's level (see checkValueDepth): doing so
// would make this value descend forever at constant depth.
func TestAF2_TheCycleBoundHoldsForEveryLinkType(t *testing.T) {
	m := map[string]any{}
	m["self"] = m
	sl := make([]any, 1)
	sl[0] = sl
	var pc af2PtrCycle
	pc = &pc
	var se af2SelfEmb
	se.af2SelfEmb = &se

	cases := []struct {
		name            string
		v               any
		encoderSucceeds bool // does json.Marshal handle it cleanly?
	}{
		{"map cycle", m, false},
		{"slice cycle", sl, false},
		{"PURE POINTER cycle (type T *T)", pc, false},
		{"PURE EMBED cycle (struct{*T}) — encoder SUCCEEDS, walk refuses", se, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, merr := json.Marshal(tc.v)
			require.Equal(t, tc.encoderSucceeds, merr == nil,
				"precondition changed: whether json.Marshal handles this cycle is what decides "+
					"if refusing it is a false refusal or a rescue")

			done := make(chan error, 1)
			go func() { done <- checkValueDepth(tc.v, "cycle") }()
			select {
			case err := <-done:
				require.Error(t, err,
					"THE CYCLE BOUND DID NOT HOLD FOR THIS LINK TYPE. Some link is being traversed "+
						"without costing a frame, so the cycle never reaches the bound. That is the HANG "+
						"non-transparency exists to prevent, and it would not be caught by the map-cycle arm")
				require.ErrorIs(t, err, ErrValidation)
			case <-time.After(30 * time.Second):
				t.Fatal("checkValueDepth did NOT terminate on this cycle — a link is free")
			}
		})
	}
}

// af2Shadowed exercises typeFields' DOMINANCE rule: the outer X is at depth 0 and the
// embedded af2ShadowInner.X at depth 1, so the shallower one WINS and the deep one is
// DROPPED. The encoder emits {"X":0}; the walk descends the dropped field in full.
//
// The sibling rule — two fields with the SAME json tag, dropped under AMBIGUITY — is the
// same mechanism and was the first draft here. It is not used because `go vet`'s structtag
// check refuses to compile a duplicate tag, and suppressing a correct vet finding to keep
// a test subject is a worse trade than using the other rule that reaches the same code.
type af2Shadowed struct {
	af2ShadowInner
	X int
}

type af2ShadowInner struct{ X []any }

// TestAF2_DroppedFieldsAreAFourthInflationSource pins MAJOR 3 — the mechanism that
// refuted the completeness claim, found by independent review and outside the fuzzer's
// grammar by construction (no duplicate-json-name production existed in a grammar I wrote).
//
// It is OVER-report, the safe direction, and it is deliberately NOT fixed by modelling
// typeFields' dominance rules: that would add exactly the mirroring complexity that
// produced BLOCKER 1 on this same guard. It is pinned instead, so the divergence is a
// known quantity rather than a surprise.
func TestAF2_DroppedFieldsAreAFourthInflationSource(t *testing.T) {
	const n = 2000
	var deep any = 1
	for i := 0; i < n; i++ {
		deep = []any{deep}
	}
	v := af2Shadowed{af2ShadowInner: af2ShadowInner{X: []any{deep}}}

	b, err := json.Marshal(v)
	require.NoError(t, err)
	encoded := jsonNestingDepth(b)
	require.Equal(t, `{"X":0}`, string(b),
		"precondition: typeFields must DROP the shadowed deep field, keeping only the outer X, "+
			"or this is not the mechanism under test")

	walk, exceeded := walkFrames(v, 1<<30)
	require.False(t, exceeded)
	t.Logf("MEASURED: encoder emits %s (depth %d); walk reports %d frames — ratio %dx from a shape "+
		"with NO pointer wrapper", b, encoded, walk, walk/encoded)

	require.Greater(t, walk, encoded*100,
		"the dropped-field inflation has vanished. Either typeFields stopped dropping colliding names "+
			"— in which case this is no longer a divergence and the arm should be retired deliberately — "+
			"or encoderVisitsField started modelling dominance, which is the mirroring complexity that "+
			"produced BLOCKER 1 and was rejected on purpose")

	// The safety direction, which is what actually matters and is why this is not a defect.
	require.Greater(t, walk, encoded,
		"OVER-report is the safe direction: the walk refuses earlier than the encoder needs. "+
			"UNDER-reporting is the unsafe one and has no known mechanism")
}
