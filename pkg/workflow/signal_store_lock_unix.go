//go:build unix

package workflow

import (
	"fmt"
	"os"
	"syscall"
)

// lockMailboxDir takes an exclusive advisory lock on a workflow's mailbox LOCK FILE and
// returns the release func. It is what makes the entry-count guard hold under concurrent
// delivery: the count and the write that depends on it must happen under ONE lock, or the
// count is stale by the time the entry lands.
//
// lockPath is the sibling <id>.signals.lock, NOT the mailbox directory, and the file is created
// on demand. This function used to open the mailbox directory itself, which was wrong for a
// reason that is invisible until you look for it: flock binds to an INODE, and removeSignalDir's
// os.RemoveAll destroys the directory's inode — so a Delete could unlink the very object a
// parked deliverer held, after which the next deliverer locked a brand-new inode and both wrote
// through one path. The lock object must be one no writer can unlink. See signalLockSuffix,
// which also records why the lock file is never deleted.
//
// Measured, because the claim it replaces was that this "cannot be made atomic cheaply" —
// an inferred shape, not a number. On darwin/arm64 APFS, flock + ReadDir vs a bare ReadDir:
//
//	mailbox n:        1        100      1000
//	unlocked:        26.8us    90.9us   667.2us
//	flock:           46.6us   112.6us   667.9us
//	overhead:       +19.8us   +21.7us    +0.6us
//
// Against a measured 11.76ms delivery, that is **0.17%** — it disappears into
// writeFileAtomic's fsync. Correctness was not expensive here; it was free.
//
// flock(2) rather than an O_CREATE|O_EXCL lockfile (measured 136.8us at n=1, ~5x more, and
// still cheap) because the kernel releases a flock when the holder dies. A lockfile carries a
// stale-lock class: a deliverer killed mid-write leaves a file that blocks every future
// delivery to that mailbox until someone removes it by hand — an availability footgun on a
// channel the M9 threat model already treats as externally writable.
// create decides whether an absent lock file is MATERIALIZED or treated as "nothing to
// serialize with". Only the DELIVERY path passes true, and that asymmetry is load-bearing:
// creating on every call meant Delete minted a permanent lock file for a workflow that never
// existed — five Deletes of nonexistent ids left five files, and a Delete of a signal-less
// workflow turned [wf.json] into [wf.signals.lock], i.e. the one API whose job is reclamation
// replacing a workflow with a permanent artifact. Unbounded in the number of distinct ids any
// caller ever passes to Delete.
//
// create=false returning a no-op release when the file is ABSENT is safe because absent means
// no delivery has ever reached its acquire — deliverSignalToDir takes this lock before any
// directory work, so an in-flight delivery has already created the file and will be seen.
// RESIDUAL, narrow and stated rather than claimed away: a FIRST-EVER delivery can create the
// file between Delete's failed open and Delete's os.RemoveAll, and those two then race.
//
// WHY THE WINDOW IS BENIGN — and this argument is the reviewer's, because it is stronger than
// the one it replaces. deliverSignalToDir acquires BEFORE MkdirAll, so
//
//	no lock file  =>  no mailbox directory either.
//
// Therefore in exactly the case where Delete skips the lock, its os.RemoveAll has NOTHING TO
// REMOVE and no signal can be lost. The only outcome the race can actually produce is a first
// delivery creating a mailbox for a workflow whose snapshot was just deleted — which is not a
// corruption at all but EARLY-SIGNAL BUFFERING, a state this package explicitly models and
// supports, named in Delete's own comment. The first version of this note said the race merely
// "loses an ordering nobody can define anyway", which is true but much weaker: it argued the
// harm was unimportant instead of showing there is none.
//
// IT IS STILL A TRADE, NOT A STRICT IMPROVEMENT, and the first version got that wrong too by
// choosing a flattering baseline. It said the window is "strictly smaller than the status quo it
// replaces" — true only against bc6cb3b, where Delete took no lock at all. Against the IMMEDIATE
// predecessor, where Delete always acquired unconditionally, this window did not exist. Honestly
// stated: it trades an UNBOUNDED RESIDUE (one permanent lock file per id ever passed to Delete,
// including ids that never existed) for a NARROW window that the paragraph above shows is
// harmless. Right trade — but "strictly smaller" was a comparison against whichever baseline
// made the claim easiest, which is how the other wrong claims in this phase were built.
func lockMailboxDir(lockPath string, create bool) (func(), error) {
	flags := os.O_RDONLY
	if create {
		// The lock file is materialized by the first delivery and NEVER removed, so from then
		// on every holder locks the same inode — the property that makes "the same lock" mean
		// the same object across processes.
		flags |= os.O_CREATE
	}
	f, err := os.OpenFile(lockPath, flags, 0600) //nolint:gosec // lockPath is baseDir + a validated single path segment + a package constant suffix
	if err != nil {
		if !create && os.IsNotExist(err) {
			return func() {}, nil // no lock file => no delivery ever ran => nothing to exclude
		}
		return nil, fmt.Errorf("%w: cannot open signal mailbox lock: %w", ErrIO, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close() //nolint:errcheck,gosec // best-effort cleanup on the error path
		return nil, fmt.Errorf("%w: cannot lock signal mailbox: %w", ErrIO, err)
	}
	return func() {
		// Unlock before close for clarity; closing the fd would release it anyway.
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck // release is best-effort; the close below also releases
		f.Close()                                       //nolint:errcheck,gosec // nothing actionable on a read-only fd close
	}, nil
}
