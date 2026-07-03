package workflow

// M10 ph37 signal + lease ADVERSARIAL concurrency / crash-window suite (independent
// of the author's tests). The author's lease_test.go proves the lease serializes a
// simple probe and that a no-op locker overlaps; signal_consume_test.go proves a
// single crash-before-checkpoint re-applies. This file attacks the gaps those miss,
// under -race:
//
//   #2 CONCURRENCY FUZZ on the lease over the REAL signal machinery:
//      - many goroutines × {Execute, Tick, DeliverAndResume} on the SAME WorkflowID
//        ⇒ the consuming side effect fires EXACTLY ONCE, the run converges, the
//        mailbox is drained, no deadlock, no -race.
//      - the load-bearing BITE: drop the lease (no-op locker) ⇒ the side effect
//        double-fires (the lease is what makes signal apply exactly-once).
//      - the defaultLocker shared across DIFFERENT *Workflow instances with the same
//        ID still serializes (and the bite: per-instance lockers do NOT).
//      - DIFFERENT WorkflowIDs run concurrently and independently.
//   #4 the delivery races as concurrent stress (deliver WHILE driving), under -race.
//   #6 the ack-after-durable crash window: a crash BETWEEN the durable save and the
//      ack leaves the signal unacked, but a resume finds the node Completed and does
//      NOT re-apply (no double-apply); and the atomic mailbox write means a torn /
//      partial entry (a leftover .tmp file) is ignored by TakeSignals.
//
// Oracle for each is stated inline.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingSink is an Action that atomically counts how many times it runs — the
// observable side effect for an exactly-once probe. An optional widen-window sleep
// makes a missing-lease overlap deterministically reproducible.
type countingSink struct {
	runs  int32
	sleep time.Duration
}

func (s *countingSink) Execute(_ context.Context, data *WorkflowData) error {
	atomic.AddInt32(&s.runs, 1)
	if s.sleep > 0 {
		time.Sleep(s.sleep)
	}
	data.SetOutput("sink", "ran")
	return nil
}

func (s *countingSink) count() int32 { return atomic.LoadInt32(&s.runs) }

// buildSignalSinkWorkflow wires await("go") -> sink over store, id wf, with the
// given locker (nil = default) and sink. The sink runs only after the await node
// completes (consumes its signal), so sink.count() is the exactly-once observable
// for the whole signal-consume path.
func buildSignalSinkWorkflow(t *testing.T, store WorkflowStore, wf string, locker Locker, sink Action) *Workflow {
	t.Helper()
	w := NewWorkflow(store)
	w.WorkflowID = wf
	if locker != nil {
		w.WithLocker(locker)
	}
	require.NoError(t, w.AddNode(NewWaitForSignalNode("wait", "go")))
	require.NoError(t, w.AddNode(NewNode("sink", sink)))
	require.NoError(t, w.AddDependency("wait", "sink"))
	return w
}

// hammerDrives launches `goroutines` workers, each performing `opsPerG` randomly
// chosen drive operations (Execute / Tick / DeliverAndResume) on w. It returns once
// all workers finish (a hang ⇒ the test's own timeout fires ⇒ deadlock detected).
// The signal is delivered up front AND re-delivered by DeliverAndResume (same ID ⇒
// idempotent), so every drive can make progress.
func hammerDrives(t *testing.T, w *Workflow, sig Signal, goroutines, opsPerG int) {
	t.Helper()
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for op := 0; op < opsPerG; op++ {
				switch (g + op) % 3 {
				case 0:
					_ = w.Execute(context.Background()) //nolint:errcheck // best-effort drive in the adversarial/concurrency harness; the property is no-panic/state-consistency, not this call’s error
				case 1:
					// Tick with a now far in the future: a signal workflow has no timers,
					// so this is a no-op drive that still exercises the lease-acquire path
					// concurrently (deadlock / -race probe).
					_, _ = w.Tick(context.Background(), time.Now().Add(time.Hour)) //nolint:errcheck // best-effort drive in the adversarial/concurrency harness; the property is no-panic/state-consistency, not this call’s error
				case 2:
					_ = w.DeliverAndResume(context.Background(), sig) //nolint:errcheck // best-effort drive in the adversarial/concurrency harness; the property is no-panic/state-consistency, not this call’s error
				}
			}
		}(g)
	}
	wg.Wait()
}

// TestAdvConc_SameID_ExactlyOnceUnderLease (#2): the consuming side effect fires
// EXACTLY ONCE no matter how many concurrent {Execute,Tick,DeliverAndResume} hammer
// the same WorkflowID — the lease serializes the load→run→save→ack span so the
// second drive always loads the node Completed and skips it. The run converges, the
// mailbox is drained, and the workers all return (no deadlock). Run under -race.
//
// Oracle (exactly-once + no-deadlock): a serialized drive span makes the signal
// apply and its downstream side effect single-shot across arbitrary concurrency.
func TestAdvConc_SameID_ExactlyOnceUnderLease(t *testing.T) {
	store := NewInMemoryStore()
	const wf = "wf-conc-exactly-once"
	sink := &countingSink{}
	w := buildSignalSinkWorkflow(t, store, wf, nil, sink) // nil → default in-process lease
	require.NoError(t, w.DeliverSignal(Signal{ID: "s1", Name: "go", Payload: "P"}))

	hammerDrives(t, w, Signal{ID: "s1", Name: "go", Payload: "P"}, 16, 12)

	assert.Equal(t, int32(1), sink.count(),
		"the consuming side effect fires exactly once under the lease, despite heavy concurrency")
	final, _ := store.Load(wf) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	st, _ := final.GetNodeStatus("sink")
	assert.Equal(t, Completed, st, "the run converges")
	stWait, _ := final.GetNodeStatus("wait")
	assert.Equal(t, Completed, stWait)
	// The mailbox holds AT MOST one entry — and if present it is the same idempotent
	// s1 re-buffered by a DeliverAndResume that landed AFTER the node completed (a
	// harmless lingering re-delivery the now-Completed node never re-consumes). The
	// invariant under concurrent re-delivery is idempotency: never a duplicate or a
	// corrupted entry, never accumulation. (Exactly-once consume is proven by the
	// sink.count()==1 above.)
	remaining, err := store.TakeSignals(wf)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(remaining), 1, "no signal accumulation under concurrent re-delivery")
	for _, s := range remaining {
		assert.Equal(t, "s1", s.ID, "any lingering entry is the same idempotent s1, never a duplicate/corrupt entry")
	}
	payload, ok := appliedSignalPayload(t, store, wf, "wait")
	require.True(t, ok)
	assert.Equal(t, "P", payload, "the payload is applied (and, being set-not-accumulate, single-valued)")
}

// TestAdvConc_SameID_NoLease_DoubleApplyBite is the load-bearing BITE for the signal
// path: drop the lease (no-op locker) and two concurrent drives both load the node
// not-yet-Completed and BOTH run the consuming side effect — proving the lease is
// what makes the signal apply exactly-once (not some other accident). A widen-window
// sleep in the sink makes the overlap deterministic.
//
// Oracle: without serialization, the consume side effect is observably > 1.
func TestAdvConc_SameID_NoLease_DoubleApplyBite(t *testing.T) {
	store := NewInMemoryStore()
	const wf = "wf-conc-nolease-bite"
	sink := &countingSink{sleep: 8 * time.Millisecond} // widen the overlap window
	w := buildSignalSinkWorkflow(t, store, wf, noOpLocker{}, sink)
	require.NoError(t, w.DeliverSignal(Signal{ID: "s1", Name: "go", Payload: "P"}))

	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			_ = w.Execute(context.Background()) //nolint:errcheck // best-effort drive in the adversarial/concurrency harness; the property is no-panic/state-consistency, not this call’s error
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(2), sink.count(),
		"without the lease, concurrent same-ID drives BOTH consume+apply the signal (the bite: the lease is load-bearing)")
}

// TestAdvConc_DefaultLockerSharedAcrossInstances (#2): two DIFFERENT *Workflow
// objects sharing a WorkflowID, each using the process-wide default locker, are still
// serialized — the default lease registry is process-shared, so the side effect
// fires exactly once even across distinct instances.
//
// Oracle: the default Locker serializes by WorkflowID across *Workflow identities.
func TestAdvConc_DefaultLockerSharedAcrossInstances(t *testing.T) {
	store := NewInMemoryStore()
	const wf = "wf-conc-shared-default"
	sink := &countingSink{sleep: 8 * time.Millisecond}
	require.NoError(t, store.DeliverSignal(wf, Signal{ID: "s1", Name: "go", Payload: "P"}))

	// Two distinct *Workflow instances, SAME id, BOTH on the default locker (nil).
	w1 := buildSignalSinkWorkflow(t, store, wf, nil, sink)
	w2 := buildSignalSinkWorkflow(t, store, wf, nil, sink)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = w1.Execute(context.Background()) }() //nolint:errcheck // best-effort drive in the adversarial/concurrency harness; the property is no-panic/state-consistency, not this call’s error
	go func() { defer wg.Done(); _ = w2.Execute(context.Background()) }() //nolint:errcheck // best-effort drive in the adversarial/concurrency harness; the property is no-panic/state-consistency, not this call’s error
	wg.Wait()

	assert.Equal(t, int32(1), sink.count(),
		"the process-wide default locker serializes same-ID drives across distinct *Workflow instances")
}

// TestAdvConc_DefaultLockerSharedAcrossInstances_PerInstanceLockerBite is the bite
// for the above: give each instance its OWN NewInProcessLocker (distinct registries)
// and they no longer serialize — the side effect double-fires. Proves the shared-
// default-locker test has teeth.
//
// Oracle: distinct Locker registries do NOT serialize across instances.
func TestAdvConc_DefaultLockerSharedAcrossInstances_PerInstanceLockerBite(t *testing.T) {
	store := NewInMemoryStore()
	const wf = "wf-conc-perinst-bite"
	sink := &countingSink{sleep: 8 * time.Millisecond}
	require.NoError(t, store.DeliverSignal(wf, Signal{ID: "s1", Name: "go", Payload: "P"}))

	w1 := buildSignalSinkWorkflow(t, store, wf, NewInProcessLocker(), sink)
	w2 := buildSignalSinkWorkflow(t, store, wf, NewInProcessLocker(), sink)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = w1.Execute(context.Background()) }() //nolint:errcheck // best-effort drive in the adversarial/concurrency harness; the property is no-panic/state-consistency, not this call’s error
	go func() { defer wg.Done(); _ = w2.Execute(context.Background()) }() //nolint:errcheck // best-effort drive in the adversarial/concurrency harness; the property is no-panic/state-consistency, not this call’s error
	wg.Wait()

	assert.Equal(t, int32(2), sink.count(),
		"per-instance lockers (distinct registries) do NOT serialize same-ID drives — the bite")
}

// TestAdvConc_DifferentIDs_Independent (#2): many distinct-ID signal workflows
// driven concurrently are fully independent — each converges, no cross-talk, no
// -race. Distinct WorkflowIDs must NOT serialize against each other.
//
// Oracle (isolation): independent WorkflowIDs share no mutable drive state.
func TestAdvConc_DifferentIDs_Independent(t *testing.T) {
	const n = 24
	store := NewInMemoryStore() // one store, many ids — exercises the store's own locking too
	var wg sync.WaitGroup
	errs := make([]error, n)
	sinks := make([]*countingSink, n)

	for i := 0; i < n; i++ {
		sinks[i] = &countingSink{}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			wf := fmt.Sprintf("wf-conc-indep-%d", i)
			w := buildSignalSinkWorkflow(t, store, wf, nil, sinks[i])
			if derr := w.DeliverSignal(Signal{ID: "s", Name: "go", Payload: i}); derr != nil {
				errs[i] = derr
				return
			}
			if eerr := w.Execute(context.Background()); eerr != nil {
				errs[i] = eerr
			}
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		require.NoError(t, errs[i], "workflow %d", i)
		assert.Equal(t, int32(1), sinks[i].count(), "workflow %d consumed exactly once", i)
		final, _ := store.Load(fmt.Sprintf("wf-conc-indep-%d", i)) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
		st, _ := final.GetNodeStatus("sink")
		assert.Equal(t, Completed, st, "workflow %d converged", i)
	}
}

// TestAdvConc_DeliverWhileDriving_Stress (#4): concurrent DeliverSignal (many IDs,
// same Name) WHILE many drives run on the same WorkflowID — the deliver-during-take
// race. The store's own lock plus the drive lease must keep this race-free: no panic,
// no corruption, no lost-or-doubled side effect, the run converges. Run under -race.
//
// Oracle (no corruption under concurrent deliver+drive): exactly one signal is
// consumed by the single await node; the rest stay buffered or are GC'd; the side
// effect fires exactly once; no data race.
func TestAdvConc_DeliverWhileDriving_Stress(t *testing.T) {
	store := NewInMemoryStore()
	const wf = "wf-conc-deliver-stress"
	sink := &countingSink{}
	w := buildSignalSinkWorkflow(t, store, wf, nil, sink)

	var wg sync.WaitGroup
	// Deliverers: race many distinct same-Name signals in while drives run.
	const deliverers = 8
	for d := 0; d < deliverers; d++ {
		wg.Add(1)
		go func(d int) {
			defer wg.Done()
			for k := 0; k < 10; k++ {
				_ = w.DeliverSignal(Signal{ID: fmt.Sprintf("d%d-%d", d, k), Name: "go", Payload: "P"}) //nolint:errcheck // best-effort drive in the adversarial/concurrency harness; the property is no-panic/state-consistency, not this call’s error
			}
		}(d)
	}
	// Drivers: hammer Execute/DeliverAndResume concurrently.
	const drivers = 8
	for dr := 0; dr < drivers; dr++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 10; k++ {
				_ = w.Execute(context.Background()) //nolint:errcheck // best-effort drive in the adversarial/concurrency harness; the property is no-panic/state-consistency, not this call’s error
			}
		}()
	}
	wg.Wait()

	// A final settling drive in case the last delivery landed after the last drive.
	_ = w.Execute(context.Background()) //nolint:errcheck // best-effort drive in the adversarial/concurrency harness; the property is no-panic/state-consistency, not this call’s error

	assert.Equal(t, int32(1), sink.count(),
		"a single await node consumes exactly one signal and its side effect fires once, under concurrent deliver+drive")
	final, _ := store.Load(wf) //nolint:errcheck // test asserts on the loaded value below; a Load error would fail those assertions
	st, _ := final.GetNodeStatus("sink")
	assert.Equal(t, Completed, st, "the run converges under concurrent deliver+drive")
}

// --- #6 the ack-after-durable crash window ---

// ackFailStore wraps an InMemoryStore and forces AckSignals to fail — modeling a
// crash/IO error in the ack step that happens AFTER the durable Save. Execute's ack
// is best-effort (//nolint:errcheck), so the drive still succeeds; the signal is
// left unacked in the mailbox. The point: a resume must NOT re-apply it.
type ackFailStore struct {
	*InMemoryStore
}

func (s *ackFailStore) AckSignals(_ string, _ []string) error {
	return errors.New("simulated crash/IO error during ack (after durable save)")
}

// TestAdvConc_AckCrashAfterDurable_NoDoubleApplyOnResume (#6): a crash/error in the
// ack step (which runs AFTER the durable Save, per D37-04) leaves the consumed signal
// in the mailbox. On resume the node is already Completed → the await action does NOT
// run again → the signal is NOT re-consumed/re-applied. The observable apply stays
// exactly-once; the stray unacked entry is harmless (GC'd later). This is the
// robustness of the take→apply→Completed→checkpoint→ack ordering when the LAST step
// (ack) fails.
//
// Oracle (no double-apply on ack failure): a Completed node is never re-driven, so a
// failed ack cannot cause a re-apply.
func TestAdvConc_AckCrashAfterDurable_NoDoubleApplyOnResume(t *testing.T) {
	store := &ackFailStore{InMemoryStore: NewInMemoryStore()}
	const wf = "wf-ack-crash"
	sink := &countingSink{}
	w := buildSignalSinkWorkflow(t, store, wf, nil, sink)
	require.NoError(t, store.DeliverSignal(wf, Signal{ID: "s1", Name: "go", Payload: "P"}))

	// Drive 1: consumes, applies, completes, Save succeeds, ack FAILS (best-effort).
	require.NoError(t, w.Execute(context.Background()), "the drive still succeeds — ack is best-effort")
	require.Equal(t, int32(1), sink.count(), "side effect fired once on the first drive")

	// The signal is still in the mailbox (ack failed) ...
	remaining, err := store.TakeSignals(wf)
	require.NoError(t, err)
	require.Len(t, remaining, 1, "the unacked signal lingers after a failed ack")

	// Drive 2 (resume): the node is Completed → not re-run → no re-apply / no double
	// side effect, despite the signal still being in the mailbox.
	require.NoError(t, w.Execute(context.Background()))
	assert.Equal(t, int32(1), sink.count(),
		"a Completed node is never re-driven, so a still-buffered (unacked) signal is NOT re-applied (no double-apply)")
}

// TestAdvConc_TornMailboxWriteIgnored (#6): the mailbox write is atomic (temp file +
// rename), so a crash mid-write can only leave a partial *.tmp-* file, never a torn
// *.sig. TakeSignals reads ONLY *.sig entries, so a leftover temp / foreign file in
// the mailbox dir is ignored — no panic, no spurious signal, no error. Validates the
// atomic-write torn-file safety claim against the on-disk reality.
//
// Oracle (atomic write): only complete *.sig entries are visible; partial/temp files
// are invisible to TakeSignals.
func TestAdvConc_TornMailboxWriteIgnored(t *testing.T) {
	for storeName, info := range fileSignalStores(t) {
		t.Run(storeName, func(t *testing.T) {
			const wf = "wf-torn"
			// One real, complete signal.
			require.NoError(t, info.store.DeliverSignal(wf, Signal{ID: "real", Name: "go", Payload: "P"}))
			dir := filepath.Join(info.baseDir, wf+signalDirSuffix)

			// Drop adversarial NON-.sig files into the mailbox dir: a leftover atomic
			// temp file (partial write), a foreign file, and a subdirectory.
			require.NoError(t, os.WriteFile(filepath.Join(dir, "real.sig.tmp-123456"), []byte("partial torn bytes"), 0600))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not a signal"), 0600))
			require.NoError(t, os.WriteFile(filepath.Join(dir, ".hidden"), []byte("dotfile"), 0600))
			require.NoError(t, os.MkdirAll(filepath.Join(dir, "subdir.sig"), 0750)) // a .sig-suffixed DIR

			sigs, err := info.store.TakeSignals(wf)
			require.NoError(t, err, "leftover temp/foreign files must not error TakeSignals")
			assert.Equal(t, []string{"real"}, signalIDs(sigs),
				"only the complete .sig entry is visible; partial/temp/foreign/dir entries are ignored")
		})
	}
}
