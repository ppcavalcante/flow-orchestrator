package workflow

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestAdv116_TakeVsAck_ConcurrentAckMakesTheWholeReadFail is the shrunk, minimal form
// of a defect the chaos arm surfaced.
//
// takeSignalsFromDir does os.ReadDir(dir) and THEN opens each entry it listed, holding
// no lock. ackSignalsInDir removes entries, also holding no lock — deliberately, on the
// (correct) reasoning that a removal cannot push a mailbox OVER its cap. But an entry
// that vanishes between the ReadDir and the open makes readBoundedFileCapped fail
// ENOENT, and takeSignalsFromDir returns that as ErrIO for the WHOLE mailbox.
//
// The mailbox read is all-or-nothing, so one concurrently-acked entry loses every OTHER
// signal in that call.
//
// The codebase already treats this exact condition as benign on the other side:
// ackSignalsInDir explicitly swallows os.IsNotExist ("acking an absent ID is not an
// error", SignalStore doc). The read path treats the same vanished file as an I/O
// failure. That asymmetry is the defect.
//
// The interleaving is not exotic. The SignalStore contract's own consume ordering is
// take -> apply -> checkpoint -> ack, and M17 competing consumers means two consumers
// legitimately run that loop against one mailbox at once.
//
// VIOLATED PROPERTY: a concurrent legal AckSignals must not turn a TakeSignals into an
// I/O error. ErrIO means the storage failed; nothing failed here.
//
// ORACLE: ackSignalsInDir's own IsNotExist tolerance, plus the interface contract —
// TakeSignals' documented failure modes are an empty slice, the signals, or
// ErrCorruptData for an over-cap mailbox. A vanished entry is none of those.
func TestAdv116_TakeVsAck_ConcurrentAckMakesTheWholeReadFail(t *testing.T) {
	for _, storeName := range []string{"JSONFileStore", "FlatBuffersStore"} {
		t.Run(storeName, func(t *testing.T) {
			stores := signalStores(t)
			store := stores[storeName]
			const wf = "wf-take-vs-ack"
			const n = 300

			ids := make([]string, 0, n)
			for i := 0; i < n; i++ {
				id := fmt.Sprintf("s%04d", i)
				ids = append(ids, id)
				require.NoError(t, store.DeliverSignal(wf, Signal{ID: id, Name: "n", Payload: "p"}))
			}

			var (
				wg       sync.WaitGroup
				takes    atomic.Int64
				badErr   atomic.Value // the first non-conforming error
				stop     = make(chan struct{})
				stopOnce sync.Once
			)

			// Reader: the consume loop's take.
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					takes.Add(1)
					_, err := store.TakeSignals(wf)
					if err != nil {
						badErr.Store(err.Error())
						stopOnce.Do(func() { close(stop) })
						return
					}
				}
			}()

			// Acker: the same loop's ack, from a second consumer.
			wg.Add(1)
			go func() {
				defer wg.Done()
				for _, id := range ids {
					select {
					case <-stop:
						return
					default:
					}
					_ = store.AckSignals(wf, []string{id}) //nolint:errcheck // adversarial test: ack is best-effort
				}
				stopOnce.Do(func() { close(stop) })
			}()

			go func() { time.Sleep(20 * time.Second); stopOnce.Do(func() { close(stop) }) }()
			wg.Wait()

			require.Greater(t, takes.Load(), int64(0), "anti-vacuity: the reader must have run")
			if v := badErr.Load(); v != nil {
				t.Fatalf("TakeSignals failed during a concurrent AckSignals after %d takes.\n"+
					"  observed: %v\n"+
					"  ackSignalsInDir tolerates this exact condition (os.IsNotExist) and the read does not;\n"+
					"  the read is all-or-nothing, so every other signal in the mailbox is lost for that call.",
					takes.Load(), v)
			}
		})
	}
}
