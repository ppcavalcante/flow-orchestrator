package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// THE FANOUT FALSIFIER — acceptance test for completing checkJSONDepth coverage.
//
// This is deliberately NOT a coverage count. The claim "the depth cap now covers all
// 12 marshal sites" is satisfiable by adding 8 calls and proving nothing; this asserts
// the property the coverage is FOR, on the one surface where it was predicted to be
// missing, so it can distinguish a real completion from a bookkeeping one.
//
// # The predicted wedge
//
// The fan-out journal is marshaled (fanout.go: per item, then the whole journal) and
// then stored as a JSON *string*:
//
//	parentData.Set(fanOutItemsKey(node), string(journal))
//
// So in the snapshot the journal's nesting lives INSIDE a string literal — and
// jsonNestingDepth correctly skips string contents (`inString`). The snapshot depth
// guard therefore CANNOT see it, structurally, and that is the guard behaving properly.
//
// But the resume path reads it back with json.Unmarshal, which IS depth-capped at 10^4
// by the stdlib scanner. Unguarded on write, capped on read:
//
//	write succeeds  ->  resume decode fails  ->  PERMANENTLY
//
// That is a WEDGE, not a refusal. The run cannot make progress and cannot be recovered
// by retrying, because the durable state it must read is one the reader refuses. A
// refusal at write time is a bad input; a refusal at read time is a lost workflow.
//
// # What each arm establishes
//
// The first arm is the mechanism, provable without the engine: the guard cannot see
// through a string, and the decoder can. The second drives the real fan-out path.
func TestFanOutJournalDepth_UnguardedOnWriteCappedOnRead(t *testing.T) {
	// A value nested past the decoder's 10^4 ceiling.
	const depth = maxJSONNestingDepth + 500
	deep := strings.Repeat(`{"k":`, depth) + `1` + strings.Repeat(`}`, depth)

	t.Run("the snapshot guard cannot see nesting inside a string, by design", func(t *testing.T) {
		// The journal as it is actually stored: a JSON string whose CONTENT is deep.
		snapshotShaped, err := json.Marshal(map[string]interface{}{
			fanOutItemsKey("fan"): deep,
		})
		require.NoError(t, err)

		require.NoError(t, checkJSONDepth(snapshotShaped, "wf"),
			"the guard must PASS this: the nesting is inside a string literal and "+
				"jsonNestingDepth skips string contents. That is correct behaviour, and it is "+
				"exactly why the journal needs its own check at the marshal site")

		// The same bytes, un-stringified, are refused — proving the depth is real and it
		// is only the string wrapper that hides it.
		require.Error(t, checkJSONDepth([]byte(deep), "wf"),
			"the identical nesting NOT wrapped in a string must be refused")
	})

	t.Run("decoder refuses what the writer accepted", func(t *testing.T) {
		var j fanOutJournal
		err := json.Unmarshal([]byte(`{"n":1,"items":[`+deep+`]}`), &j)
		require.Error(t, err,
			"the resume path's decode must fail on an over-deep journal — this is the read "+
				"half of the wedge, and it is the stdlib scanner, not our guard")
	})

	// THE FALSIFIER PROPER: drive the real fan-out expander with an over-deep item and
	// assert the WRITE refuses. Before the 12-site completion this is expected to
	// SUCCEED at write time and strand the run on resume; after it, the write must be
	// refused loudly with ErrValidation instead.
	t.Run("an over-deep expander item must be refused at WRITE time", func(t *testing.T) {
		// Built in Go, NOT by decoding `deep`: the decoder caps at maxJSONNestingDepth,
		// so it refuses to construct the very value under test. A host builds this kind
		// of value in memory, which is exactly the vector — it never passes a decoder on
		// the way in.
		deepItem := map[string]interface{}{}
		cur := deepItem
		for i := 0; i < depth; i++ {
			nxt := map[string]interface{}{}
			cur["k"] = nxt
			cur = nxt
		}
		cur["leaf"] = 1

		// A store is required: the fan-out expansion checkpoints its journal, so without
		// one the node fails earlier with "no parent store in scope" and never reaches
		// the depth check. A red for the wrong reason is not a bite.
		const id = "wf-fanout-depth"
		store := NewInMemoryStore()
		b := NewWorkflowBuilder().WithWorkflowID(id)
		b.AddFanOut("fan",
			func(context.Context, *WorkflowData) ([]interface{}, error) {
				return []interface{}{deepItem}, nil
			},
			ActionFunc(func(context.Context, *WorkflowData) error { return nil }),
		)
		dag, err := b.Build()
		require.NoError(t, err)
		w := newWorkflowForTest(store)
		w.WorkflowID = id
		w.dag = dag

		execErr := w.Execute(context.Background())

		require.Error(t, execErr,
			"an item too deep for the resume path's decoder must be refused at WRITE time. "+
				"Accepting it here writes a journal that can never be read back: the run wedges "+
				"on resume rather than failing now, which is strictly worse.")
		require.ErrorIs(t, execErr, ErrValidation,
			"and it must be a validation refusal, in the same domain as every other depth cap")
	})
}

// builder.WithInput — the other marshal site that was already fallible, so it needed a
// check rather than new plumbing.
//
// The sub-workflow input is carried into the durable work_queue and read back by a
// decoder that HAS a depth cap. Same asymmetry as the fan-out journal: unguarded write,
// capped read, and the failure lands on the far side where it is a wedge instead of a
// rejection. Build time is the earliest and cheapest place to catch it.
func TestWithInput_DepthCappedAtBuildTime(t *testing.T) {
	deep := map[string]any{}
	cur := deep
	for i := 0; i < maxJSONNestingDepth+500; i++ {
		nxt := map[string]any{}
		cur["k"] = nxt
		cur = nxt
	}
	cur["leaf"] = 1

	b := NewWorkflowBuilder().WithWorkflowID("wf-withinput-depth")
	b.AddSubWorkflowQueued("child", "childType").WithInput(map[string]any{"deep": deep})

	_, err := b.Build()

	require.Error(t, err,
		"an over-deep WithInput must be refused at BUILD time; accepting it writes a "+
			"work_queue row that its own reader will refuse")
	require.ErrorIs(t, err, ErrValidation)
	require.Contains(t, err.Error(), "sub-workflow input",
		"the refusal must name which value it rejected — a bare depth error at build time "+
			"is unactionable when several values could be the cause")
}

// WorkflowData.Snapshot() — the EXPORTED serializer, which carried no depth cap.
//
// This site was wrongly counted as covered: SaveToJSON has a checkJSONDepth, and it is
// easy to read the four existing checks as guarding the four marshal sites. They do not
// pair up. SaveToJSON's check guards its OWN bytes; Snapshot() reaches createSnapshot's
// marshal without passing it or any other. A count of guards is not a count of guarded
// sites.
//
// Snapshot() is JSONFileStore's serializer, so an uncapped document here is one the
// store's own reader refuses on load — the same write-accepts/read-refuses wedge as the
// fan-out journal and WithInput, on a third surface.
func TestSnapshot_DepthCapped(t *testing.T) {
	d := NewWorkflowData("wf-snapshot-depth")

	deep := map[string]interface{}{}
	cur := deep
	for i := 0; i < maxJSONNestingDepth+500; i++ {
		nxt := map[string]interface{}{}
		cur["k"] = nxt
		cur = nxt
	}
	cur["leaf"] = 1
	d.Set("deep", deep)

	_, err := d.Snapshot()

	require.Error(t, err,
		"Snapshot() is the exported serializer; an over-deep document must be refused at "+
			"WRITE time rather than produced for a reader that will refuse it")
	require.ErrorIs(t, err, ErrValidation)
}
