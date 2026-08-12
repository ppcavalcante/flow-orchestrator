package workflow

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// AUD-029 / C-14: ForEach, ForEachNodeStatus, ForEachOutput, and ForEachWait invoked
// their callback while holding w.mu.RLock. The RWMutex is non-reentrant, so a callback
// that called any writer (Set/SetNodeStatus/SetOutput/SetWait) deadlocked -- and a slow
// callback blocked every writer for the whole iteration. The fix snapshots entries under
// the lock, releases it, then invokes the callback, so a re-entrant write is safe.
//
// runWithDeadline runs fn in a goroutine and fails if it does not finish in time -- a
// deadlock would otherwise hang the whole test binary.
func runWithDeadline(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("AUD-029: %s deadlocked -- the callback could not write back while the iterator held the lock", what)
	}
}

func TestAUD029_ForEachIsReentrantSafe(t *testing.T) {
	t.Run("ForEach", func(t *testing.T) {
		w := NewWorkflowData("aud029-data")
		w.Set("a", 1)
		w.Set("b", 2)
		runWithDeadline(t, "ForEach", func() {
			w.ForEach(func(key string, _ interface{}) {
				w.Set(key+"-seen", true) // re-entrant WRITE
			})
		})
		v, ok := w.GetBool("a-seen")
		require.True(t, ok)
		require.True(t, v)
	})

	t.Run("ForEachNodeStatus", func(t *testing.T) {
		w := NewWorkflowData("aud029-status")
		w.SetNodeStatus("n1", Completed)
		w.SetNodeStatus("n2", Pending)
		runWithDeadline(t, "ForEachNodeStatus", func() {
			w.ForEachNodeStatus(func(name string, _ NodeStatus) {
				w.SetNodeStatus(name, Running) // re-entrant WRITE
			})
		})
		st, ok := w.GetNodeStatus("n1")
		require.True(t, ok)
		require.Equal(t, Running, st)
	})

	t.Run("ForEachOutput", func(t *testing.T) {
		w := NewWorkflowData("aud029-output")
		w.SetOutput("n1", "x")
		w.SetOutput("n2", "y")
		runWithDeadline(t, "ForEachOutput", func() {
			w.ForEachOutput(func(name string, _ interface{}) {
				w.SetOutput(name+"-copy", "z") // re-entrant WRITE
			})
		})
		got, ok := w.GetOutput("n1-copy")
		require.True(t, ok)
		require.Equal(t, "z", got)
	})

	t.Run("ForEachWait", func(t *testing.T) {
		w := NewWorkflowData("aud029-wait")
		w.SetWait("n1", 100)
		w.SetWait("n2", 200)
		runWithDeadline(t, "ForEachWait", func() {
			w.ForEachWait(func(name string, _ int64) {
				w.ClearWait(name) // re-entrant WRITE
			})
		})
		_, armed := w.GetWait("n1")
		require.False(t, armed, "the re-entrant ClearWait must have taken effect")
	})
}
