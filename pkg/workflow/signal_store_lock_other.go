//go:build !unix

package workflow

// lockMailboxDir is a NO-OP on non-unix platforms, and that is a stated residual rather
// than an oversight.
//
// RESIDUAL: on these platforms the file-store entry-count guard is BEST-EFFORT, and it is open
// in THREE ways, not one. An earlier version of this comment named only the first and called it
// "the exact defect the unix build fixes" — singular, and wrong by two. Every class the unix
// lock closes is open here:
//
//  1. N concurrent deliveries of distinct new IDs each observe the same pre-count and all
//     commit, leaving the mailbox over the bound.
//  2. An AckSignals of an id racing an in-flight RE-delivery of that same id: the re-delivery
//     decided "this id already exists, so I cannot grow the mailbox" and the ack then frees
//     that slot for someone else before the re-delivery's write lands. Round 2's blocker.
//  3. A Delete racing a delivery: os.RemoveAll can reclaim the mailbox while a delivery is
//     mid-write, resurrecting an entry into a workflow that was just deleted.
//
// The read side still rejects an over-cap mailbox, so nothing silently corrupts; the failure
// mode is the original one, a mailbox that must be drained out of band. Class 3 is the one that
// is NOT merely an over-count — it leaves a deleted workflow holding a signal.
//
// Because the lock is a no-op here, the sibling <id>.signals.lock file is never created on
// these platforms: there is nothing to lock, so there is nothing to leave behind. The create
// parameter is therefore ignored — it exists so the unix build can distinguish the DELIVERY
// path (which materializes the lock file) from Delete (which must not).
//
// Why not close it here too: flock(2) has no portable equivalent, and the Windows primitive
// (LockFileEx via golang.org/x/sys/windows) would promote an indirect dependency to a direct
// one to harden a platform this project only ever COMPILES for — CI cross-builds
// GOOS=windows to catch host-only assumptions and never runs the test suite there. Shipping
// untested locking code is worse than a documented gap. If Windows becomes a tested target,
// this is the file to fix, and the unix version above is the shape to mirror.
func lockMailboxDir(_ string, _ bool) (func(), error) {
	return func() {}, nil
}
