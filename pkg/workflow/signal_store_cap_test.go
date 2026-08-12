package workflow

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSignalStore_MailboxEntryCountCap exercises the F37-LOW-1 mailbox entry-count
// cap across all three stores. The production bound is defaultMaxElements (2^20);
// materializing that many entries per store (2^20 files for the file stores) is
// impractical in a unit test, so the test lowers the overridable signalMailboxCap
// to a small value (restored via defer) and asserts:
//   - a mailbox AT the cap reads cleanly, and
//   - a mailbox that EXCEEDS the cap is rejected with ErrCorruptData rather than
//     driving an unbounded allocation.
//
// This mirrors the snapshot decode's element-count guard (workflow_store.go) and
// documents the host contract: ack consumed signals; do not over-deliver.
func TestSignalStore_MailboxEntryCountCap(t *testing.T) {
	const cap = 3
	orig := signalMailboxCap
	signalMailboxCap = cap
	defer func() { signalMailboxCap = orig }()

	deliver := func(t *testing.T, store SignalStore, wf string, n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			require.NoError(t, store.DeliverSignal(wf, Signal{
				ID:   fmt.Sprintf("s%03d", i),
				Name: "approve",
			}))
		}
	}

	t.Run("at cap reads clean", func(t *testing.T) {
		for name, store := range signalStores(t) {
			t.Run(name, func(t *testing.T) {
				const wf = "wf-at-cap"
				deliver(t, store, wf, cap)
				got, err := store.TakeSignals(wf)
				require.NoError(t, err)
				require.Len(t, got, cap)
			})
		}
	})

	t.Run("over cap rejected as ErrCorruptData", func(t *testing.T) {
		for name, store := range signalStores(t) {
			t.Run(name, func(t *testing.T) {
				const wf = "wf-over-cap"
				// Reaching the over-cap state now requires stepping AROUND the write
				// guard (F1): DeliverSignal refuses the delivery that would exceed the
				// cap, so delivering cap+1 in a row no longer produces this state — it
				// produces an ErrValidation, which is the fix. This subtest previously
				// did exactly that under require.NoError, i.e. it encoded the wedge as
				// expected behavior.
				//
				// Deliver above the bound with the cap raised, then lower it. That is
				// not a contrivance: it is the shape of the two ways an over-cap mailbox
				// can still exist — state written before the guard landed, and an
				// external writer to the mailbox dir or signals table (the channel is
				// external-writable by design, M9 threat model). The READ guard this
				// subtest exists to prove is exactly what defends those.
				signalMailboxCap = orig
				deliver(t, store, wf, cap+1)
				signalMailboxCap = cap

				_, err := store.TakeSignals(wf)
				require.Error(t, err)
				require.True(t, errors.Is(err, ErrCorruptData),
					"over-cap mailbox must reject as ErrCorruptData, got %v", err)
			})
		}
	})
}
