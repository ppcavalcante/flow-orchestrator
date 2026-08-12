package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The cost of the F1 write-side mailbox count guard, as a re-runnable measurement
// rather than a number in a commit message.
//
// It needs measuring because deliverSignalToDir was O(1) before the guard: it writes to
// a path built from sig.ID, so idempotency falls out of the filename and nothing ever
// scanned the directory. A guard that scans makes an N-fill O(N^2), and it is slowest
// at exactly the size it defends against. This project has twice been bitten by an
// unmeasured hot-path cost claim (F-PG-09, the det-tax per-level alloc), so the claim
// gets a benchmark.
//
// Measured darwin/arm64 APFS, go1.25, -benchtime 100x-2000x:
//
//	mailbox n:              1       100      1000     10000
//	guard, new entry:      31us     124us    799us    9.75ms   <- O(N)
//	guard, re-delivery:    ~24us    ~26us    ~25us    ~24us    <- O(1), ReadDir short-circuit
//	one full delivery:   ~9-10ms, n-independent (writeFileAtomic's fsync dominates)
//
// The re-delivery row is a BAND over three consecutive runs (23.8-28.2us across all n), not a
// frozen exact, because that is what it measures as. A single earlier draw put n=1000 at 49.7us
// and re-running showed it as an outlier — reporting one draw would have manufactured a
// non-existent O(N) knee out of scheduler noise. Read it as flat-in-n with a ~24-28us constant.
//
// So the new-entry scan is 0.3% / 1.2% / 8% / ~100% of a delivery at those sizes, and
// filling a 1000-entry mailbox measured 10.31s -> 10.46s end to end (+1.5%). It only
// becomes material around n~10^4 — a size that is already the host-contract violation
// the guard exists to refuse.
//
// THE RE-DELIVERY ROW MOVED, and the reason is correctness, not drift. It read
// 3.8/3.1/2.7/2.2us when re-delivery ran through a lock-free fast path — a bare os.Stat,
// no flock. That path was deleted: an AckSignals racing its in-flight write breached the
// cap (deliverSignalToDir has the interleaving). Re-delivery now takes the lock like every
// other delivery, which is the ~20us open+flock pair, re-measured above rather than
// predicted. What had to survive is the SHAPE, and it did: still FLAT in n, because an
// existing entry still skips the O(N) ReadDir. Against an ~11.8ms delivery, ~24-28us is
// ~0.2% — the same trade the new-entry path already made for the same reason.
//
// The lock is taken on the SIBLING lock file, not the mailbox dir, so this arm locks
// <mbox>.lock to match. That is not cosmetic: locking the directory let a Delete's
// os.RemoveAll destroy the locked inode. See signalLockSuffix.
//
// Run: go test -run XXX -bench BenchmarkMailboxGuardCost -benchtime 200x ./pkg/workflow/

// BenchmarkMailboxGuardCost isolates the guard from the fsync that dominates a real
// delivery, so the O(N) new-entry arm and the O(1) re-delivery arm are both visible.
func BenchmarkMailboxGuardCost(b *testing.B) {
	for _, n := range []int{1, 100, 1000, 10000} {
		dir := b.TempDir()
		mbox := filepath.Join(dir, "wf"+signalDirSuffix)
		if err := os.MkdirAll(mbox, 0750); err != nil {
			b.Fatal(err)
		}
		for j := 0; j < n; j++ {
			p := filepath.Join(mbox, fmt.Sprintf("s%06d%s", j, signalFileSuffix))
			if err := os.WriteFile(p, []byte("{}"), 0600); err != nil {
				b.Fatal(err)
			}
		}

		// New entry: no existing file, so the Stat misses and the locked scan runs. O(N).
		b.Run(fmt.Sprintf("newEntry/n=%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				unlock, err := lockMailboxDir(mbox+".lock", true)
				if err != nil {
					b.Fatal(err)
				}
				e, rerr := os.ReadDir(mbox)
				unlock()
				if rerr != nil || len(e) != n {
					b.Fatal(rerr, len(e))
				}
			}
		})

		// Re-delivery: the Stat hits UNDER the lock and short-circuits the scan. Must stay FLAT
		// in n — skipping the O(N) ReadDir is the whole point, and this is the arm an
		// at-least-once host exercises most.
		//
		// This arm used to model a bare Stat with NO lock, matching a lock-free fast path that
		// has since been deleted: it was breachable by an AckSignals racing the in-flight write
		// (see deliverSignalToDir). A benchmark that models code the package no longer has is a
		// claim nobody is checking.
		//
		// WHAT IT MODELS, stated precisely because the previous wording said "the real sequence"
		// and that was an overstatement of exactly the kind this phase keeps producing: it is
		// lock + Stat + unlock ONLY. The real delivery also does an os.Stat(dir) and an
		// os.MkdirAll BEFORE the lock, and neither is here. Both are sub-microsecond against
		// 22us, so the number and the flat-in-n SHAPE are unaffected — but this is a LOWER BOUND
		// on the guard's cost, not the guard.
		b.Run(fmt.Sprintf("redelivery/n=%d", n), func(b *testing.B) {
			p := filepath.Join(mbox, fmt.Sprintf("s%06d%s", 0, signalFileSuffix))
			lockPath := mbox + ".lock" // the sibling lock file the real path locks, not the dir
			for i := 0; i < b.N; i++ {
				unlock, err := lockMailboxDir(lockPath, true)
				if err != nil {
					b.Fatal(err)
				}
				_, serr := os.Stat(p)
				unlock()
				if serr != nil {
					b.Fatal(serr)
				}
			}
		})
	}
}

// BenchmarkMailboxFill measures the end-to-end shape the guard could have wrecked:
// filling a mailbox to n. Divide ns/op by n for the per-delivery cost — flat across n
// means the scan is still hiding behind the fsync.
func BenchmarkMailboxFill(b *testing.B) {
	for _, n := range []int{1, 100, 1000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				s, err := NewJSONFileStore(b.TempDir())
				if err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				for j := 0; j < n; j++ {
					if derr := s.DeliverSignal("wf", Signal{ID: fmt.Sprintf("s%06d", j), Name: "n"}); derr != nil {
						b.Fatal(derr)
					}
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*n), "ns/delivery")
		})
	}
}
