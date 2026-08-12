package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// AF1's PROPERTY, and it is the obligation the tolerance has to earn.
//
// takeSignalsFromDir now skips an entry that vanished between ReadDir and open,
// because a concurrent legal AckSignals is allowed to remove it. That is only
// correct if the skip is confined to entries that were genuinely acked:
//
//	the returned set ⊇ every signal delivered and NOT acked before the call started
//
// A tolerance that also swallowed a real read failure would satisfy "no error" and
// silently lose a live signal — and a dropped signal is a park that never wakes. So
// the assertion here is about the CONTENT of the returned set, never about the
// absence of an error.
//
// Across all four stores via signalStores(t): InMemory and SQLite have no ReadDir/open
// window at all, so they are the control — the four must not diverge under the same
// interleaving. A fix that made the file stores behave differently from the other two
// would be a conformance break even with every file-store test green.
func TestAF1_TakeReturnsEverySignalNotYetAcked(t *testing.T) {
	for name, store := range signalStores(t) {
		t.Run(name, func(t *testing.T) {
			const wf = "wf-af1-property"
			const total = 24
			const ackFirst = 8 // acked BEFORE the take starts — legitimately absent

			all := make([]string, 0, total)
			for i := 0; i < total; i++ {
				id := fmt.Sprintf("sig-%03d", i)
				require.NoError(t, store.DeliverSignal(wf, Signal{ID: id, Name: "n", Payload: i}))
				all = append(all, id)
			}

			// Ack a prefix BEFORE the take. These may legally be absent from the result.
			require.NoError(t, store.AckSignals(wf, all[:ackFirst]))
			mustSurvive := all[ackFirst:]

			got, err := store.TakeSignals(wf)
			require.NoError(t, err, "a take must not fail because of an ack that completed before it started")

			have := make(map[string]bool, len(got))
			for _, s := range got {
				have[s.ID] = true
			}
			for _, id := range mustSurvive {
				require.True(t, have[id],
					"signal %q was delivered and never acked, so the take MUST return it; "+
						"a tolerance that drops it is silent signal loss, not concurrency safety", id)
			}
		})
	}
}

// The same property under a CONCURRENT ack — the window AF1 is actually about.
//
// The ack runs while the take is in flight, so which entries vanish mid-read is
// genuinely racy. The property still holds for the signals the ack never names: those
// are untouched on disk, so no interleaving can excuse losing them.
//
// This is the invariant form, not the schedule form: it asserts what must be true of
// the RESULT rather than staging a particular interleaving, so it survives the fix
// rather than encoding the bug's timing.
func TestAF1_ConcurrentAckNeverDropsAnUnackedSignal(t *testing.T) {
	for name, store := range signalStores(t) {
		t.Run(name, func(t *testing.T) {
			const wf = "wf-af1-concurrent"
			const total = 40
			const ackHalf = 20

			all := make([]string, 0, total)
			for i := 0; i < total; i++ {
				id := fmt.Sprintf("sig-%03d", i)
				require.NoError(t, store.DeliverSignal(wf, Signal{ID: id, Name: "n", Payload: i}))
				all = append(all, id)
			}
			acked := all[:ackHalf]
			untouched := all[ackHalf:] // never named by the ack — must always come back

			var wg sync.WaitGroup
			var takeErr error
			var got []Signal

			wg.Add(2)
			go func() {
				defer wg.Done()
				_ = store.AckSignals(wf, acked) //nolint:errcheck // property test: ack is best-effort
			}()
			go func() {
				defer wg.Done()
				got, takeErr = store.TakeSignals(wf)
			}()
			wg.Wait()

			require.NoError(t, takeErr,
				"a concurrent, entirely legal ack must not fail the whole take — that is AF1")

			have := make(map[string]bool, len(got))
			for _, s := range got {
				have[s.ID] = true
			}
			for _, id := range untouched {
				require.True(t, have[id],
					"signal %q was never named by the concurrent ack, so nothing could have "+
						"legitimately removed it; the take dropped a live signal", id)
			}
		})
	}
}

// The tolerance must be NOT-EXIST AND NOTHING ELSE.
//
// readBoundedFileCapped returns three distinct things, and only one of them is the
// vanished-entry case: the open error verbatim (incl. ENOENT), a real I/O error, and
// ErrCorruptData when the file exceeds the byte ceiling — which is HYG-00's read half.
// The tempting fix for AF1 is a blanket `continue`, which would convert that ceiling
// guard into silent truncation of the mailbox.
//
// Seeded directly on disk because the delivery guard refuses an over-ceiling signal at
// the write side; the read side has to be probed independently, which is the same
// reason TestAdv116_F1_ReadSideIsUnprotectedByTheWriteGuard exists.
func TestAF1_ToleranceDoesNotSwallowTheByteCeiling(t *testing.T) {
	const ceiling = 256
	dir := t.TempDir()
	store, err := NewJSONFileStore(dir, WithJSONMaxFileSize(ceiling))
	require.NoError(t, err)

	const wf = "wf-af1-ceiling"
	require.NoError(t, store.DeliverSignal(wf, Signal{ID: "ok-1", Name: "n", Payload: 1}))

	// Plant an over-ceiling entry beside the legitimate one, the way an external
	// writer would (M9) — straight at the backing store, bypassing DeliverSignal,
	// whose write guard would refuse it.
	mailbox := filepath.Join(dir, wf+signalDirSuffix)
	oversized := filepath.Join(mailbox, "zz-oversized"+signalFileSuffix)
	require.NoError(t, os.WriteFile(oversized, []byte(strings.Repeat("x", ceiling+64)), 0o600))

	_, err = store.TakeSignals(wf)
	require.Error(t, err,
		"an over-ceiling entry must still fail the take loudly; if AF1's tolerance were a "+
			"blanket skip, this would silently return only the other signal")
	require.ErrorIs(t, err, ErrCorruptData,
		"the byte-ceiling guard must keep its own error domain, not be reclassified or dropped")
}
