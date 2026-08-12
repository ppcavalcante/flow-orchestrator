package workflow

import (
	"fmt"
	"strings"
)

// reservedKeyPrefix marks engine-internal WorkflowData keys — the boundary envelope
// (__boundaries__), the definition digest (__def_digest__), the fan-out item/items
// (__fanout_item__ / __fanout_items__:), and the per-DAG current level
// (__current_level_*). They share the flat data map with consumer keys, so a consumer
// write to one would clobber engine metadata (AUD-018). The sealed view refuses a
// consumer write to any such key; the engine reaches them via setReserved (or the
// unsealed instance the executor holds). Reads are never restricted.
const reservedKeyPrefix = "__"

// isReservedKey reports whether a data key belongs to the engine-reserved namespace.
func isReservedKey(k string) bool { return strings.HasPrefix(k, reservedKeyPrefix) }

// ErrSealed is returned (wrapped) as a node failure when a consumer action attempts
// to mutate engine-authority state through its sealed per-node view — forging a node
// status/output, or touching run-level saga/wait state. (M24 AUD-019.)
var ErrSealed = fmt.Errorf("%w: action tried to mutate engine journal state", ErrValidation)

// engineTrustedAction marks the INTERNAL action types that legitimately mutate engine
// journal state from their own Execute — a ChoiceNode marks not-taken branches
// Bypassed, and the timer / wait-for-signal actions arm and disarm durable waits. The
// executor hands these the UNSEALED WorkflowData; every other action (all consumer
// actions, and the merge/fan-out wrappers that DELEGATE to consumer user actions) runs
// against a sealed per-node view. This is a package-internal capability marker: it is
// unexported, so a consumer cannot forge it.
type engineTrustedAction interface{ engineTrusted() }

// isEngineTrusted reports whether an action is allowed to mutate engine journal state.
func isEngineTrusted(a Action) bool {
	_, ok := a.(engineTrustedAction)
	return ok
}

// sealedViewFreelist recycles the per-node sealed views so the seal costs ZERO heap
// allocation on the drive hot path (the det-tax moat): a view lives only for the
// synchronous span of one action's Execute and is returned immediately after. It is a
// buffered channel, NOT a sync.Pool, deliberately — sync.Pool is emptied on every GC
// cycle, which reintroduced ~1-2 allocs/drive on the diamond det-tax bench; a channel
// holds live references that survive GC, so after warmup acquire always recycles.
// Capacity comfortably exceeds any single level's concurrent width (default
// MaxConcurrency 16). Both overflow paths self-correct: a release that finds the buffer
// full drops the view to GC, and an acquire that finds it empty allocates a fresh one.
//
// Because a view is recycled, an action MUST NOT retain its *WorkflowData past Execute —
// already the contract (the executor moves on and may hand the same slot to another node).
var sealedViewFreelist = make(chan *WorkflowData, 256)

// Pre-fill the freelist so acquire never starts empty (and so warmup allocations don't
// leak into the det-tax measurement). 64 comfortably exceeds any single level's concurrent
// width at the default MaxConcurrency (16); the buffer absorbs in-flight views beyond that.
func init() {
	for i := 0; i < 64; i++ {
		sealedViewFreelist <- new(WorkflowData)
	}
}

// acquireSealedView returns a per-node SEALED view of w: a recycled *WorkflowData whose
// fields are copied from w, so it SHARES w's backing maps, mutex, interner, and metrics
// (the action's reads and its legitimate consumer-data / own-node-output writes reach the
// real state), but with the seal engaged. The engine-authority mutators refuse to mutate
// through it and record a sealedViolation the executor turns into a node failure. w itself
// (the engine/host instance) is never sealed. Pair with releaseSealedView.
// (M24 DEC-M24-MEDIATION.)
//
// Sharing is sound precisely because mu is a *sync.RWMutex: the copy shares the one lock
// rather than duplicating a lock value over shared maps.
func acquireSealedView(w *WorkflowData, nodeName string) *WorkflowData {
	var v *WorkflowData
	select {
	case v = <-sealedViewFreelist:
	default:
		v = new(WorkflowData)
	}
	*v = *w // shares maps + mu pointer + interner + metrics
	v.sealed = true
	v.sealedNode = nodeName
	v.sealedViolation = nil
	return v
}

// releaseSealedView clears a sealed view (dropping the shared references so the recycled
// slot never pins backing state) and returns it to the freelist, or to GC if it is full.
func releaseSealedView(v *WorkflowData) {
	*v = WorkflowData{}
	select {
	case sealedViewFreelist <- v:
	default:
	}
}

// recordSealViolation records the FIRST forge attempt against a sealed view. Guarded by
// the shared lock because a pathological action could call mutators from goroutines it
// spawned; the executor reads sealFault() only after Execute returns (happens-before).
func (w *WorkflowData) recordSealViolation(op string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sealedViolation == nil {
		w.sealedViolation = fmt.Errorf("%w (%s)", ErrSealed, op)
	}
}

// sealFault returns the recorded seal violation (nil on an unsealed view or when the
// action made no forge attempt). Read by the executor after the action returns.
func (w *WorkflowData) sealFault() error { return w.sealedViolation }
