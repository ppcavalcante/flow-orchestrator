// M21 ph105 — Branch-execution engine. The PRODUCTION fan-out mechanism, promoted from the ph104 spike
// (846c2e2, build-tagged) after the crux was proven. A fan-out is a SINGLE ordinary DAG node whose Execute:
//   (1) resolves N via an expander ONCE and journals {N + per-item keys} durably BEFORE branch 1 (expansion-once,
//       via the checkpointFrom(ctx) reserved-key flush — the moat's no-replay leg);
//   (2) drives N branches in the node's OWN MaxConcurrency-bounded pool (read from ctx via the withMaxConcurrency
//       seam), under a cancellable sub-context so a FailFast failure cancels in-flight siblings;
//   (3) aggregates node[i] in DISCOVERY order (index-addressed, assembled after the pool drains).
// Crash-after-branch-k idempotency comes from the DETERMINISTIC child ID + child.Execute resume-idempotency
// (subworkflow.go:274-278; the terminal-node skip that delivers it is parallel_execution.go:88) — NOT the
// terminal-fast-path (which is an optimization; the 104 correction).
//
// ADDITIVE: the fan-out is one ordinary node; Execute/dag.go/parallel_execution.go public behavior is unchanged.
// The ONLY executor change is the additive withMaxConcurrency set-site at dag.go (wraps the ctx handed to
// executeNodesInLevel) — non-fan-out nodes never read the value, so their behavior is byte-identical.
//
// The builder AddFanOut surface + width cap are ph106; CollectPartial is ph107. FailFast is the only fan-in
// policy here.

package workflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"time"
)

// ErrFanOutRequiresCheckpointer is returned when a fan-out node runs on a store WITHOUT a durable Checkpointer
// (F4). Expansion-once needs the {N+items} journal to survive a crash; without a checkpointer the expander would
// re-run on every re-drive (a different N breaks resume) — so we fail loudly rather than degrade silently. Mirrors
// ErrSuspendRequiresCheckpointer (suspend.go:45).
var ErrFanOutRequiresCheckpointer = errors.New("fan-out cannot run: no durable checkpoint configured (use Workflow.Execute with a Checkpointer Store)")

// ErrFanOutMaxWidth is returned when the expander resolves more branches than the width cap (default
// DefaultFanOutMaxWidth) allows — the unbounded-N DoS guard. Enforced AFTER the expander resolves N but BEFORE any
// branch (or child ID) is created, so a discover() returning millions of items fails loud + cheap, never a park or
// a silent truncation. Mirrors the loud-typed-error discipline of the M19 nesting cap (maxSubWorkflowDepthCap).
var ErrFanOutMaxWidth = errors.New("fan-out width exceeds the configured maximum")

// DefaultFanOutMaxWidth is the default per-fan-out branch-count ceiling (mirrors maxSubWorkflowDepthCap = 1024).
// Overridable per node via AddFanOut(...).WithMaxWidth(n).
const DefaultFanOutMaxWidth = 1024

// ErrFanOutResultKeyCollision is returned when a fan-out's declared result keys (the base key, an indexed
// key[i], or the count key) collide with a pre-existing foreign parent data key — a loud refusal (never
// last-writer-wins), mirroring ErrSubWorkflowResultKeyCollision.
var ErrFanOutResultKeyCollision = errors.New("fan-out declared result key collides with an existing parent data key")

// FanOutItemKey is the reserved WorkflowData key under which a branch's per-item value is placed for the branch
// action to read (AddFanOut items flow via WorkflowData — the non-generic expander contract). A branch action
// reads its item with data.Get(FanOutItemKey).
const FanOutItemKey = "__fanout_item__"

// fanOutResultCountKey / fanOutResultIndexKey / fanOutResultFailedKey are the parent-data keys the typed
// per-branch results + the CollectPartial partition land in. The count/failed keys are namespaced (".__count__" /
// ".__failed__") so they cannot be mistaken for an indexed element; indexed keys are baseKey[i]. All go through
// the collision guard. A consumer reads: count=N, __failed__=the failed indices (CollectPartial), baseKey[i]=the
// typed result for a SUCCEEDED i (absent for a failed i).
func fanOutResultCountKey(baseKey string) string  { return baseKey + ".__count__" }
func fanOutResultFailedKey(baseKey string) string { return baseKey + ".__failed__" }
func fanOutResultIndexKey(baseKey string, i int) string {
	return baseKey + "[" + strconv.Itoa(i) + "]"
}

// FanOutExpander resolves the runtime fan-out width. Returns the ordered discovery list of per-branch inputs;
// len(items) == N. Runs EXACTLY ONCE across a crash+resume (its result is journaled).
type FanOutExpander func(ctx context.Context, parentData *WorkflowData) ([]interface{}, error)

// fanOutBranch builds the single-node child DAG that processes item i. A factory (not a prebuilt DAG) so each
// branch gets a distinct action closure without sharing action state across branches.
type fanOutBranch func(index int, item interface{}) *DAG

// branchDAGFromAction wraps a user branchAction into the per-branch single-node child DAG factory. The wrapper
// node injects the item under FanOutItemKey into the branch child's data BEFORE the user action runs, so the
// action reads its item with data.Get(FanOutItemKey) (the non-generic item-via-WorkflowData contract). The
// result the action Sets under resultFrom is read back by driveBranch for the typed node[i] keying.
// If retry is non-nil, the user branchAction is wrapped in a RetryableAction so a failed branch re-drives up to
// retry.count times — WITHOUT re-expanding the fan-out and WITHOUT re-running succeeded siblings (HARD-02). The
// wrap is on the branch's INNER action here, NOT the branch node's RetryCount (that stays 0, so node.Execute never
// double-wraps), and it sits BELOW the deterministic child-ID journal (driveBranch) → exactly-once PERSISTENCE
// intact. The branch node still runs the wrapped action directly; a sibling FailFast cancels an in-backoff retry
// via the branch sub-context (RetryableAction's mid-backoff select on ctx.Done), so FailFast latency is bounded.
func branchDAGFromAction(branchAction Action, retry *branchRetryPolicy) fanOutBranch {
	if retry != nil {
		branchAction = retry.applyTo(branchAction)
	}
	return func(_ int, item interface{}) *DAG {
		b := NewWorkflowBuilder()
		b.AddStartNode("branch").WithAction(ActionFunc(func(ctx context.Context, d *WorkflowData) error {
			d.setReserved(FanOutItemKey, item) // the per-branch item (reserved key; setReserved bypasses the AUD-018 seal), read by branchAction via data.Get(FanOutItemKey)
			return branchAction.Execute(ctx, d)
		}))
		dag, err := b.Build()
		if err != nil {
			// A single-node builder with a valid action never fails Build; a nil action is guarded at AddFanOut.
			panic(fmt.Sprintf("fan-out branch DAG build: %v", err))
		}
		return dag
	}
}

// AddFanOut declares a fan-out node: at run time `expander` resolves N items, and `branchAction` runs ONCE per
// item (in the node's own MaxConcurrency-bounded pool, discovery order). Mirrors AddSubWorkflow. Each branch
// reads its item with data.Get(FanOutItemKey). Declare the typed per-branch result keying with .WithResults; cap
// the width with .WithMaxWidth (default DefaultFanOutMaxWidth). Requires a Checkpointer store (expansion-once
// needs durable N) — a non-Checkpointer store fails with ErrFanOutRequiresCheckpointer at run time.
//
// ITEM TYPING: the item a branch reads via FanOutItemKey is JSON-DECODED with UseNumber() — the expansion is
// journaled as a JSON string so it survives a crash store-uniformly. A JSON NUMBER arrives as json.Number
// (INT64-FAITHFUL, full range — call .Int64() or .Float64() for the concrete type); a string as string, an object
// as map[string]interface{}. UseNumber is load-bearing: a DEFAULT decode into interface{} yields float64 and
// CORRUPTS an int64 item above 2^53 (a large ID / nanos timestamp), the [[first-ci-run-saga]] fidelity bug on the
// item axis. (The branch's RESULT — what it Sets under the WithResults branchResultKey — is keyed
// TYPED/value_long-faithful separately.)
//
// CRASH-RESUME IS AT-LEAST-ONCE EXECUTION + EXACTLY-ONCE PERSISTENCE. Do NOT build a durability claim on
// "a branch that already completed is skipped on resume": the terminal-fast-path in driveBranch is an
// OPTIMIZATION, not the contract. What actually holds the line is that a re-driven branch resolves to the
// SAME child ID (deterministic f(parentID, nodeName, index)) and so re-uses the SAME journal, and
// child.Execute's own resume-idempotency then skips that child's already-terminal nodes (the skip itself is
// parallel_execution.go:88, and dag.go:618 for the cross-level sweep; prose at subworkflow.go:274-278) — so
// bypassing the fast path costs a load, not a re-execution.
//
// The re-invocation that DOES happen is a branch IN FLIGHT at the crash: its node is not yet terminal, so
// on resume branchAction runs again for that item, having possibly already half-run. Only the PERSISTED
// per-branch result is exactly-once. Make a side-effecting branchAction idempotent (the executor passes it
// a stable IdempotencyKey handle). Same contract as the expander (F-PG-13) and any node (F-PG-11).
func (b *WorkflowBuilder) AddFanOut(name string, expander FanOutExpander, branchAction Action) *NodeBuilder {
	node := b.AddNode(name)
	if expander == nil || branchAction == nil {
		node.actionErr = fmt.Errorf("%w: AddFanOut %q requires a non-nil expander and branchAction", ErrValidation, name)
		return node
	}
	node.action = &fanOutAction{
		nodeName:    name,
		expander:    expander,
		branchInner: branchAction, // retained so WithBranchRetries can rebuild the factory with a retry policy
		branch:      branchDAGFromAction(branchAction, nil),
	}
	return node
}

// WithResults declares the fan-out node's typed per-branch result keying: each branch's `branchResultKey` DATA
// key (a scalar the branchAction Sets) is written into parent data under `parentBaseKey[i]` in discovery order,
// TYPED (value_long-faithful — an int64 reloads as an int64 on all three stores), plus a count key
// `parentBaseKey.__count__` = N. Mirrors WithResult(parentKey, childDataKey). Optional: without it the branches
// run for effect only (no node[i] keys). A declared key colliding with an existing parent key → loud
// ErrFanOutResultKeyCollision at run time. Only valid on an AddFanOut node.
func (n *NodeBuilder) WithResults(parentBaseKey, branchResultKey string) *NodeBuilder {
	a, ok := n.action.(*fanOutAction)
	if !ok {
		n.actionErr = fmt.Errorf("%w: WithResults is only valid on an AddFanOut node", ErrValidation)
		return n
	}
	a.resultKey = parentBaseKey
	a.resultFrom = branchResultKey
	return n
}

// WithMaxWidth overrides the fan-out node's branch-count ceiling (default DefaultFanOutMaxWidth). A resolved N
// exceeding it → loud ErrFanOutMaxWidth (never a park/silent truncation). A non-positive value restores the
// default. Only valid on an AddFanOut node.
func (n *NodeBuilder) WithMaxWidth(maxWidth int) *NodeBuilder {
	a, ok := n.action.(*fanOutAction)
	if !ok {
		n.actionErr = fmt.Errorf("%w: WithMaxWidth is only valid on an AddFanOut node", ErrValidation)
		return n
	}
	a.maxWidth = maxWidth
	return n
}

// WithCollectPartial opts the fan-out node into the CollectPartial fan-in policy (FANOUT-05). Default (unset) is
// FailFast: the first branch failure fails the fan node and cancels in-flight/un-started siblings. Under
// CollectPartial: ALL N branches run to completion (no sibling cancellation), and the fan node COMPLETES (not
// Failed) even with k failures, exposing a {succeeded, failed} partition in the result namespace:
//   - baseKey.__count__  = N
//   - baseKey.__failed__ = the list of failed branch indices (store-uniform JSON string)
//   - baseKey[i]         = the typed result for a SUCCEEDED branch i (ABSENT for a failed i)
//
// A consumer reads __failed__ to see which failed, and inspects a failed branch's own child journal (by its
// deterministic ID) for the error. A partial failure does NOT fail the fan node, so under CollectPartial the
// parent proceeds and a parent-level M12 WithCompensation rollback is NOT triggered by a partial branch failure
// (containment (b) — the node Completes → no ExecutionError → no rollback). Only valid on an AddFanOut node.
//
// THE PARTITION IS NOT A RE-EXECUTION LEDGER. __failed__/__count__/baseKey[i] describe the FINAL outcome per
// index, not how many times a branch ran: execution across a crash is AT-LEAST-ONCE, persistence exactly-once
// (see AddFanOut — the terminal-fast-path that skips a completed branch on resume is an optimization, not the
// guarantee). A branch in flight at a crash re-runs on resume, so a succeeded baseKey[i] does NOT mean its
// action ran exactly once. Count re-executions, if you need to, in the branch's own idempotent effect.
func (n *NodeBuilder) WithCollectPartial() *NodeBuilder {
	a, ok := n.action.(*fanOutAction)
	if !ok {
		n.actionErr = fmt.Errorf("%w: WithCollectPartial is only valid on an AddFanOut node", ErrValidation)
		return n
	}
	a.collectPartial = true
	return n
}

// WithBranchRetries opts the fan-out node into per-branch retry (HARD-02 / F-PG-10). A failed branch re-drives up
// to `count` extra attempts (total ≤ count+1) with a BOUNDED backoff (capped exponential + jitter by default),
// WITHOUT re-expanding the fan-out and WITHOUT re-running succeeded siblings — the re-drive reuses the same
// deterministic child-ID, so retry and crash-resume share the no-replay path and the result persists exactly-once.
// `delay` is the base backoff. Optional `opts` layer the RetryableAction knobs onto the branch wrapper — e.g.
// `func(r *workflow.RetryableAction){ r.WithRetryIf(isTransient) }` to mark permanent errors non-retryable (a
// non-retryable error → exactly 1 attempt), or WithMaxDelay/WithJitter/WithBackoff to tune the bound.
//
// count ≤ 0 clears the policy (back to no-retry, the default). Interplay with the fan-in policy: under FailFast a
// branch's retries exhaust BEFORE its terminal error reaches the pool's sibling-cancel; a concurrent sibling
// FailFast cancels this branch's in-flight backoff within the window (the branch sub-context is cancelled). Under
// WithCollectPartial each failing branch retries before it lands in the __failed__ partition. Only valid on an
// AddFanOut node.
func (n *NodeBuilder) WithBranchRetries(count int, delay time.Duration, opts ...func(*RetryableAction)) *NodeBuilder {
	a, ok := n.action.(*fanOutAction)
	if !ok {
		n.actionErr = fmt.Errorf("%w: WithBranchRetries is only valid on an AddFanOut node", ErrValidation)
		return n
	}
	if count <= 0 {
		a.branchRetry = nil
		a.branch = branchDAGFromAction(a.branchInner, nil)
		return n
	}
	var configure func(*RetryableAction)
	if len(opts) > 0 {
		configure = func(r *RetryableAction) {
			for _, opt := range opts {
				opt(r)
			}
		}
	}
	a.branchRetry = &branchRetryPolicy{count: count, delay: delay, configure: configure}
	a.branch = branchDAGFromAction(a.branchInner, a.branchRetry)
	return n
}

// fanOutAction is the production fan-out node. It generalizes subWorkflowAction 1→N: N deterministic child runs
// under (parentID, nodeName, index)-derived IDs, driven by the node's OWN bounded pool under a cancellable
// sub-context, aggregated in discovery order. The expander result is journaled (expansion-once) before any branch.
type fanOutAction struct {
	nodeName       string
	expander       FanOutExpander
	branchInner    Action // the raw user branch action, retained so WithBranchRetries can rebuild `branch` with a retry policy
	branch         fanOutBranch
	resultFrom     string             // the branch child DATA key each branch's result is read from (typed, value_long-faithful)
	resultKey      string             // the parent BASE key; per-branch results land TYPED under resultKey[i] + a count key
	maxWidth       int                // per-node width cap (≤0 → DefaultFanOutMaxWidth); exceed → ErrFanOutMaxWidth
	collectPartial bool               // FANOUT-05: true → all branches run, node Completes with a {succeeded, failed} partition; false (default) → FailFast
	branchRetry    *branchRetryPolicy // HARD-02/F-PG-10: per-branch retry policy; nil (default) → no retry, branch drive byte-identical to pre-ph112
}

// branchRetryPolicy is the per-branch fan-out retry configuration (HARD-02 / F-PG-10). When set (via
// WithBranchRetries), each branch's inner action is wrapped in a RetryableAction inside branchDAGFromAction, so a
// failed branch re-drives up to `count` times WITHOUT re-expanding the fan-out and WITHOUT re-running succeeded
// siblings. Retry sits BELOW the deterministic child-ID journal → exactly-once PERSISTENCE is untouched; it
// MULTIPLIES at-least-once EXECUTION (the ph111 contract). A nil policy = no retry.
type branchRetryPolicy struct {
	count     int                    // max retries (extra attempts beyond the first); total attempts ≤ count+1
	delay     time.Duration          // base backoff delay
	configure func(*RetryableAction) // optional caller hook to layer WithMaxDelay/WithJitter/WithRetryIf/WithBackoff
}

// applyTo wraps a branch action in a bounded RetryableAction. The default policy is BOUNDED (capped exp backoff +
// jitter), never a tight loop; a caller `configure` hook may override the cap/jitter/classifier. Called only when
// the policy is non-nil.
func (p *branchRetryPolicy) applyTo(action Action) Action {
	r := NewRetryableAction(action, p.count, p.delay).
		WithMaxDelay(defaultBranchRetryMaxDelay).
		WithJitter(defaultBranchRetryJitter)
	if p.configure != nil {
		p.configure(r)
	}
	return r
}

// Default bounded-backoff knobs for a per-branch retry policy (overridable via WithBranchRetries opts). A cap +
// jitter is what makes the default policy a bounded storm rather than a tight loop (CONTEXT {Q}2).
const (
	defaultBranchRetryMaxDelay = 30 * time.Second
	defaultBranchRetryJitter   = 0.2
)

// fanOutItemsKey is the reserved parent-data key the expansion-once journal lives in. Namespaced by node name
// (internal "__fanout_items__:" prefix) so it cannot collide with a user data key OR two fan-out nodes in one
// workflow. The value is a JSON STRING (not a raw []interface{}), because a slice does NOT round-trip
// store-uniformly — InMemory/JSONFile reload it as []interface{} but SQLite reloads a complex value as a JSON
// string ([[first-ci-run-saga]] store-fidelity). A string round-trips faithfully on ALL three stores, so
// self-encoding the items is the store-uniform expansion-once journal.
func fanOutItemsKey(nodeName string) string { return "__fanout_items__:" + nodeName }

// fanOutJournal is the durable expansion record: the ordered items (JSON-encoded) + their count. Count is
// redundant with len(items) but is a torn-write corruption guard (a partial store write is caught rather than
// silently fanning out a wrong width).
type fanOutJournal struct {
	N     int               `json:"n"`
	Items []json.RawMessage `json:"items"` // each item's JSON, preserved in discovery order
}

// FanOutChildID derives the deterministic per-branch child ID from (parentID, nodeName, index) via ONE hash
// (F3 — drops the spike's brittle base[len("sub:"):] prefix-slice).
//
//	digest = SHA-256( uint64-LE(len(parentID)) || parentID ||
//	                  uint64-LE(len(nodeName)) || nodeName || uint64-LE(index) )
//	id     = "fan:" + hex(digest)
//
// The 8-byte little-endian length prefixes on parentID and nodeName make every
// (parentID, nodeName, index) split unambiguous (("ab","c",0) and ("a","bc",0) never collide), and the index is
// folded into the SAME hash — resume-stable (same index → same ID) and collision-safe vs a 2-field "sub:" child
// (distinct "fan:" prefix + the index in the digest). This construction is a STABLE CONTRACT — downstream
// systems may recompute it, so it must not change across versions without a deliberate, documented break. The
// same guarantee IdempotencyKey carries.
//
// Exported because WithCollectPartial's own contract tells a consumer to inspect a failed branch's child
// journal by its deterministic ID — an instruction that was not followable while this was unexported.
// Recompute it here rather than reimplementing the framing: the length prefixes are a collision guard, not
// incidental.
func FanOutChildID(parentID, nodeName string, index int) string {
	h := sha256.New()
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(len(parentID)))
	h.Write(buf[:])
	h.Write([]byte(parentID))
	binary.LittleEndian.PutUint64(buf[:], uint64(len(nodeName)))
	h.Write(buf[:])
	h.Write([]byte(nodeName))
	binary.LittleEndian.PutUint64(buf[:], uint64(index)) //nolint:gosec // index is a non-negative branch index bounded by DefaultFanOutMaxWidth
	h.Write(buf[:])
	return "fan:" + hex.EncodeToString(h.Sum(nil))
}

// Execute runs the fan-out: F4 checkpointer gate → expansion-once → the node's bounded, cancellable N-branch pool
// → discovery-order aggregate. FailFast: the first branch failure cancels the branch sub-context; in-flight
// siblings observe ctx.Done() and no un-started branch launches.
// engineTrusted marks fanOutAction as engine machinery: it orchestrates branches as
// isolated CHILD workflows (driveBranch → child.Execute; the consumer branch action is
// sealed inside that child execution) and writes the fan-out result/count/journal keys
// into parentData. It runs unsealed so those engine-journal writes succeed; it never runs
// consumer code on parentData, so trusting it does not widen the mediation surface.
// (M24 DEC-M24-MEDIATION.)
func (a *fanOutAction) engineTrusted() {}

func (a *fanOutAction) Execute(ctx context.Context, parentData *WorkflowData) error {
	store := parentStoreFrom(ctx)
	if store == nil {
		return ErrSubWorkflowRequiresStore // the branches are child runs; they need the parent's store.
	}
	// (F4) HARD-FAIL on a non-Checkpointer store BEFORE any expander/branch work — expansion-once has no durable
	// {N} without it, so a silent degrade would re-run the expander on resume (breaking no-replay). Loud + early.
	if checkpointFrom(ctx) == nil {
		return fmt.Errorf("%w: node %q", ErrFanOutRequiresCheckpointer, a.nodeName)
	}
	parentID := parentData.GetWorkflowID()

	// (1) EXPANSION-ONCE. The reserved-key READ precedes the expander CALL: if the journal already carries the
	// items, this is a resume → read them, never re-expand. Else run the expander ONCE, journal {N + items}, and
	// FLUSH durably BEFORE branch 1 (the level-barrier flush at dag.go is too late — a crash between "expander
	// returned" and "branch 1" would lose N). Atomic-before-branch-1.
	items, err := a.resolveExpansion(ctx, parentData)
	if err != nil {
		return err
	}

	n := len(items)

	// WIDTH CAP (the unbounded-N DoS guard): enforced AFTER the expander resolved N but BEFORE any branch or child
	// ID is created — a discover() returning millions of items fails loud + cheap here. Loud typed error, never a
	// park or a silent truncation. Note: the expansion is ALREADY journaled at this point, so on a re-drive of an
	// over-wide schedule the cap fails again deterministically (the journal doesn't shrink) — the intended loud
	// permanent refusal, not a transient.
	maxN := a.maxWidth
	if maxN <= 0 {
		maxN = DefaultFanOutMaxWidth
	}
	if n > maxN {
		return fmt.Errorf("%w: fan-out %q resolved %d branches > cap %d", ErrFanOutMaxWidth, a.nodeName, n, maxN)
	}

	// COLLISION GUARD (base + count keys): refuse if the base key or the count key pre-exists — these are ALWAYS
	// written by this node, so any pre-existing value is a foreign namespace clash (loud, never last-writer-wins;
	// mirrors ErrSubWorkflowResultKeyCollision). Checked BEFORE any branch runs so a doomed fan-out fails early. The
	// per-INDEX keys are checked at WRITE time (below) with a DeepEqual allowance, because only there is the value
	// known — an idempotent resume that re-writes the SAME baseKey[i] value must be allowed (this is inert in ph106
	// where a terminal node is skipped on resume, but ph107 CollectPartial writes partials while non-terminal, so
	// the value-aware check is the forward-correct mirror of the sub-workflow guard — review F1).
	if a.resultKey != "" {
		if err := a.checkBaseKeyCollisions(parentData, n); err != nil {
			return err
		}
	}

	// N=0: nothing to fan out. Write the count (0) so a downstream consumer reads an empty aggregate; under
	// CollectPartial also write an empty __failed__ so the partition is definite. No indexed keys, no branch,
	// single Execute, no hang.
	if n == 0 {
		if a.resultKey != "" {
			parentData.Set(fanOutResultCountKey(a.resultKey), 0)
			if a.collectPartial {
				parentData.Set(fanOutResultFailedKey(a.resultKey), "[]")
			}
		}
		return nil
	}

	// (2) THE NODE'S OWN BOUNDED, CANCELLABLE POOL. Cap = maxConcurrencyFrom(ctx) (the withMaxConcurrency seam;
	// coerce ≤0 → DefaultMaxConcurrency). The level semaphore (parallel_execution.go:82) lives one level up and
	// does not reach inside a node, so the node owns its own bound. branchCtx mirrors executeNodesInLevel's
	// levelCtx (parallel_execution.go:64-66) ONE LEVEL DOWN: a FailFast branch failure cancels it → in-flight
	// siblings observe ctx.Done() and no un-started branch launches.
	capN := maxConcurrencyFrom(ctx)
	if capN <= 0 {
		capN = DefaultMaxConcurrency
	}
	branchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// BOUNDED WORKER-POOL (ph110 / F-PG-08): min(n,capN) worker goroutines pull branch indices from a work channel,
	// so peak goroutines == min(n,cap) — NOT n. (The prior "spawn n goroutines each blocking on a capN-sized sem"
	// had peak==n: a 100k-item fan-out spawned ~100k live goroutines / ~70GB, an OOM cliff on the headline feature.)
	// The observable contract is byte-identical: same FailFast immediate-cancel timing, same un-started skip via
	// branchCtx, same discovery-order results[idx], same CollectPartial "all run, no cancel", driveBranch unchanged.
	results := make([]interface{}, n)
	errs := make([]error, n)

	numWorkers := capN
	if numWorkers > n {
		numWorkers = n
	}
	work := make(chan int, numWorkers) // small buffer; the feeder blocks so it can observe FailFast cancel promptly.

	// FEEDER: hands indices 0..n-1 to the workers in order. On FailFast cancel (branchCtx.Err()!=nil) it stops
	// feeding AND sets errs[idx]=branchCtx.Err() for EVERY un-fed index — the exact equivalent of the old pre-launch
	// skip (":312"). This is load-bearing: an un-fed index left nil/nil would be read by the fan-in aggregate as a
	// SUCCESS keying results[idx]==nil (a silent wrong-result). Invariant: every index ends with a real result+nil
	// err, or a non-nil err — never nil/nil for an un-processed index.
	go func() {
		defer close(work)
		for i := 0; i < n; i++ {
			if branchCtx.Err() != nil {
				for j := i; j < n; j++ {
					errs[j] = branchCtx.Err()
				}
				return
			}
			select {
			case work <- i:
			case <-branchCtx.Done():
				for j := i; j < n; j++ {
					errs[j] = branchCtx.Err()
				}
				return
			}
		}
	}()

	// WORKERS: min(n,cap) goroutines, each pulls indices until work is drained. A fed-but-not-yet-started index whose
	// branchCtx already cancelled records the cancellation error and skips driveBranch (mirrors the old in-goroutine
	// <-branchCtx.Done() arm). Each processed index writes results[idx]+errs[idx]; a FailFast failure cancel()s.
	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range work {
				if branchCtx.Err() != nil {
					errs[idx] = branchCtx.Err()
					continue
				}
				res, berr := a.driveBranch(branchCtx, store, parentID, idx, itemForBranch(items[idx]))
				results[idx] = res
				errs[idx] = berr
				if berr != nil && !a.collectPartial {
					cancel() // FailFast (default): first failure cancels in-flight + un-started siblings.
					// CollectPartial: do NOT cancel — every branch runs to completion.
				}
			}
		}()
	}
	wg.Wait()

	// EXTERNAL CANCELLATION (both policies, review F1): if the PARENT ctx was cancelled/timed out, propagate that
	// error — do NOT complete. Under CollectPartial an external cancel would otherwise bucket the interrupted
	// branches into __failed__ (they were NOT failures — they never got to run/finish) and durably persist a
	// POISONED partition on a Completed node, which a resume then skips (abandoning + mis-classifying them). An
	// external cancel is not a partition outcome; surface it so the node stays non-terminal and the run reports the
	// cancellation. (branchCtx cancellation from FailFast's own cancel() is distinct — that path already returned
	// under !collectPartial above; here ctx is the PARENT ctx, cancelled only externally.)
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("fan-out %q cancelled: %w", a.nodeName, err)
	}

	// (3) FAN-IN POLICY. FailFast (default): the first branch error fails the whole node — surface the ROOT-CAUSE
	// failure, preferring the first NON-cancellation error over a cancelled sibling (a cancelled sibling carries
	// context.Canceled, the fail-fast SIDE EFFECT not the cause — returning it would mask the real failure, review
	// note #2). CollectPartial: skip this return entirely — a failed branch does NOT fail the node; failures go
	// into the __failed__ partition (4) and the node Completes.
	if !a.collectPartial {
		firstErrIdx := -1
		for i := 0; i < n; i++ {
			if errs[i] == nil {
				continue
			}
			if firstErrIdx == -1 {
				firstErrIdx = i // remember the first error of any kind (the all-cancelled fallback)
			}
			if !errors.Is(errs[i], context.Canceled) {
				return fmt.Errorf("fan-out %q branch %d: %w", a.nodeName, i, errs[i]) // the real root cause
			}
		}
		if firstErrIdx != -1 {
			return fmt.Errorf("fan-out %q branch %d: %w", a.nodeName, firstErrIdx, errs[firstErrIdx])
		}
	}

	// (4) RESULT-KEYING + partition. A SUCCEEDED branch writes its result TYPED under resultKey[i] (value_long-
	// faithful scalar, NOT a []interface{} aggregate — the SQLite JSON-string trap). A FAILED branch (CollectPartial
	// only — under FailFast we already returned) writes NO resultKey[i] (absent, not a poisoned value) and its index
	// joins the __failed__ list. Plus the count key. The per-index collision check is VALUE-AWARE (DeepEqual, review
	// F1): a pre-existing resultKey[i] EQUAL to what we'd write is this node's own idempotent re-apply on resume
	// (allowed — the partial-resume case); a DIFFERENT foreign value is a loud collision.
	if a.resultKey != "" {
		var failed []int
		for i := 0; i < n; i++ {
			if errs[i] != nil {
				failed = append(failed, i) // CollectPartial: a failed branch → __failed__, no result key.
				continue
			}
			key := fanOutResultIndexKey(a.resultKey, i)
			// 116-AF2, the DeepEqual class. reflect.DeepEqual RECURSES, and dies at ~922
			// bytes of goroutine stack per level — sooner than json.Marshal's ~743, so it
			// is the tighter class and the same bound covers both. results[i] is a branch
			// result: host code's return value, of arbitrary depth.
			//
			// 🔴 116-GC-F1, AND IT WAS MINE. An earlier version of this comment said
			// results[i] "has been nowhere else" and contrasted it against sub-workflow's
			// store.Load. That is BACKWARDS, and it was a FIFTH instance of the very class
			// the rest of this comment corrects — introduced by the commit that fixed the
			// other four, and caught by an independent reviewer rather than by me.
			//
			// driveBranch returns a.readBranchResult(...) on BOTH of its return paths, and
			// each reads a *WorkflowData that came from store.Load(childID) — the terminal
			// fast path and the post-drive reload alike. Its own doc comment says it uses
			// "the SAME per-child idempotency as subWorkflowAction". So a branch result HAS
			// met the store, exactly as a sub-workflow result has; the two sites are
			// structurally the same, not opposites.
			//
			// It is the OTHER operand that may never meet an encoder: `existing` comes from
			// parentData.Get, and WorkflowData.Get/Set are in-memory map operations with no
			// store involvement. On a fresh single-process run `existing` is the raw Go
			// value a prior node Set, on EVERY backend; only on a RESUME was it rebuilt
			// from the store.
			//
			// 🔴 116-AF9: "BOUNDING ONE SIDE IS SUFFICIENT" STOOD HERE AND IS FALSE.
			// The proof offered for it — deepValueEqual descends only where both values
			// have the corresponding element, so its depth is bounded by the shallower of
			// the two — is about STRUCTURAL depth and holds only for ACYCLIC values. For a
			// cyclic pair both structural depths are infinite and min() says nothing; what
			// terminates deepValueEqual is its memo, and that memo matches on a repeated
			// PAIR. That is why checkDeepEqualPairDepth takes BOTH values, and why it is
			// called with both below. The stale proof is deleted rather than qualified: a
			// reader who half-believes it will "tidy" the call back down to one side.
			if existing, present := parentData.Get(key); present {
				// Guard INSIDE the present branch: that is when DeepEqual actually runs.
				// 🔴 116-AF6-R2, ACCEPTED RESIDUAL (medium). RE-WORDED, not
				// re-dispositioned: an independent seat was commissioned to attack the
				// original text and refuted it in three directions. Corrected here because
				// this comment is the only copy of the finding that ships.
				//
				// The error-substitution class 116-AF1 named is NARROWED, not closed. Any
				// pair checkDeepEqualPairDepth refuses returns ErrValidation from here,
				// because the depth guard runs BEFORE this function's own comparison and a
				// refusal pre-empts it.
				//
				// WIDER than first recorded — it substitutes for SUCCESS, not just for a
				// sentinel. For a deeply EQUAL pair the contract here is nil, the
				// idempotent re-apply. MEASURED: two distinct-but-equal 5-node rings —
				// reflect.DeepEqual answers true in 79.9 us without difficulty — are
				// refused. It fails a run that would have succeeded.
				//
				// NO CYCLE IS NEEDED; "notably two same-type cyclic values" understated
				// the class. An ACYCLIC struct{Val int; Next *N} chain reaches it. MEASURED
				// boundary, monotone, with the guard NAMED at each step:
				//
				//	links   walk frames (2n+1)   outcome
				//	16,383  32,767              ACCEPT -> falls through to the DeepEqual
				//	                            below -> ErrFanOutResultKeyCollision (correct)
				//	16,384  32,769              REFUSE -> ErrValidation, checkDeepEqualPairDepth
				//
				// Note the arithmetic, because this project keeps tripping on it: a link
				// costs a POINTER frame and a STRUCT frame, so n links cost 2n+1 walk
				// frames, and the accept edge is 16,383 — NOT "16,384 x 2 = 32,768", which
				// is the depth-vs-frames slip 116-AF5 was filed for.
				//
				// NARROWER than first recorded — exposure is InMemoryStore-ONLY, and the
				// reason runs through results[i], NOT through `existing` (116-GC-F5: the
				// operands were attributed the wrong way round here too).
				//
				// results[i] is the store-derived operand. On JSONFile / FlatBuffers /
				// SQLite it comes back from store.Load FLATTENED — map[string]interface{}
				// or string — while `existing` is whatever the parent map holds, so the
				// pair TYPE-MISMATCHES, deepEqualSettlesWithoutRecursing accepts it, and
				// the correct collision sentinel comes back. On a RESUME both operands are
				// store-derived, both are flattened and therefore shallow, and the
				// acyclic-side accept above fires instead. Either way a marshalling backend
				// cannot reach this bound.
				//
				// Nor can an encoder-VISIBLE value get there through one: checkJSONDepth
				// caps a document at 10,000 NESTING LEVELS, which is about 20,000 walk
				// frames, and this guard does not refuse until past 32,768. Both quoted in
				// FRAMES on purpose — comparing "10,000 levels" against "16,384 links"
				// would be the AF5 unit slip, two paragraphs after the one warning about it.
				//
				// InMemoryStore is a supported public backend, so the residual is REAL —
				// its blast radius is one backend.
				//
				// EXPOSURE IS AT Execute, NOT AT Save — confirmed independently. This check
				// runs on in-memory results during the run, so "a cyclic value could never
				// persist" is TRUE and about the WRONG AXIS. It is also false in its own
				// right; see 116-AF9 in value_depth_deepequal.go.
				//
				// The remedy — a bounded lockstep probe to decide cheap-disqualification
				// during recursion — was DECLINED by the architect: ~60 lines of equality
				// re-implementation inside a bound, on the guard already carrying the most
				// mirroring complexity, to convert a safe refusal into a less safe accept.
				if derr := checkDeepEqualPairDepth(existing, results[i], fmt.Sprintf("fan-out %q branch %d result", a.nodeName, i)); derr != nil {
					return derr
				}
				if !reflect.DeepEqual(existing, results[i]) {
					return fmt.Errorf("%w: indexed key %q (node %q)", ErrFanOutResultKeyCollision, key, a.nodeName)
				}
			}
			parentData.Set(key, results[i])
		}
		parentData.Set(fanOutResultCountKey(a.resultKey), n)
		if a.collectPartial {
			// Store-uniform __failed__ encoding: the index list is a slice in the flat scalar namespace, so
			// JSON-encode it to a string (the expansion-journal pattern) — round-trips on all 3 stores. Always
			// written under CollectPartial (an empty [] when all succeeded), so a consumer reads a definite partition.
			// Value-aware (F1): a partial-resume re-writes the SAME __failed__ string → allowed; a foreign value → loud.
			if failed == nil {
				failed = []int{} // marshal to "[]" not "null"
			}
			enc, err := json.Marshal(failed)
			if err != nil {
				return fmt.Errorf("fan-out %q encode failed-list: %w", a.nodeName, err)
			}
			fkey := fanOutResultFailedKey(a.resultKey)
			// NO checkValueDepth here, and it is an ARGUMENT rather than an omission — this
			// site was enumerated with the two above as a DeepEqual crash vector and it is
			// not one. The second argument is always a `string`, and reflect.DeepEqual
			// compares TYPES first ("values of distinct types are never deeply equal"), so
			// a deep `existing` returns false immediately without descending; and if
			// `existing` is also a string it is a string comparison, which does not recurse
			// either. MEASURED in a child process at depth 650,000:
			// DeepEqual(deepValue, "a string") exits 0, while DeepEqual(deep, deep) dies.
			//
			// If that second argument ever stops being a string, this argument breaks and
			// the site needs the guard. Adding one TODAY would be worse than useless: it
			// would make the next reader's census count a vector that does not exist.
			if existing, present := parentData.Get(fkey); present && !reflect.DeepEqual(existing, string(enc)) {
				return fmt.Errorf("%w: failed-list key %q (node %q)", ErrFanOutResultKeyCollision, fkey, a.nodeName)
			}
			parentData.Set(fkey, string(enc))
		}
	}
	return nil
}

// checkBaseKeyCollisions refuses if the BASE key already holds any value (always written by this node → any
// pre-existing value is a foreign namespace clash), and if the COUNT key holds a value ≠ N (value-aware, review
// F1: a CollectPartial partial-resume re-writes the same count = N, which must be allowed, not a false collision;
// a DIFFERENT count is foreign). The per-INDEX keys and the __failed__ list are checked value-aware at write time
// (see (4)) because only there are those values known. Mirrors the sub-workflow guard discipline.
func (a *fanOutAction) checkBaseKeyCollisions(parentData *WorkflowData, n int) error {
	if _, present := parentData.Get(a.resultKey); present {
		return fmt.Errorf("%w: base key %q (node %q)", ErrFanOutResultKeyCollision, a.resultKey, a.nodeName)
	}
	if existing, present := parentData.Get(fanOutResultCountKey(a.resultKey)); present && coerceCountInt(existing) != n {
		return fmt.Errorf("%w: count key for %q (node %q)", ErrFanOutResultKeyCollision, a.resultKey, a.nodeName)
	}
	return nil
}

// coerceCountInt normalizes a store-reloaded count (int/int64/json.Number) to an int for the value-aware guard. A
// non-integer (a foreign string) returns a sentinel that never equals n → treated as a collision.
func coerceCountInt(v interface{}) int {
	switch c := v.(type) {
	case int:
		return c
	case int64:
		return int(c)
	case json.Number:
		if iv, err := c.Int64(); err == nil {
			return int(iv)
		}
	}
	return -1 // never equals a real n (n ≥ 0) → foreign value → collision
}

// resolveExpansion returns the discovery-ordered items (each as raw JSON), reading the durable journal on a resume
// (expander NOT called) or running the expander ONCE + journaling + flushing on the first drive. The journal is a
// JSON STRING so it round-trips store-uniformly (see fanOutItemsKey).
func (a *fanOutAction) resolveExpansion(ctx context.Context, parentData *WorkflowData) ([]json.RawMessage, error) {
	if raw, ok := parentData.Get(fanOutItemsKey(a.nodeName)); ok {
		// Resume path: the expansion is durable. Decode the JSON-string journal (the READ precedes any expander
		// CALL — that read-before-call IS expansion-once).
		s, isStr := raw.(string)
		if !isStr {
			return nil, fmt.Errorf("%w: fan-out %q journal is not a string (got %T)", ErrValidation, a.nodeName, raw)
		}
		var j fanOutJournal
		if err := json.Unmarshal([]byte(s), &j); err != nil {
			return nil, fmt.Errorf("%w: fan-out %q journal malformed: %w", ErrValidation, a.nodeName, err)
		}
		// Corruption guard: the journaled count MUST agree with the items length — a mismatch means a torn write
		// / tamper; refuse rather than fan out a wrong width.
		if j.N != len(j.Items) {
			return nil, fmt.Errorf("%w: fan-out %q journaled count %d ≠ items length %d", ErrValidation, a.nodeName, j.N, len(j.Items))
		}
		return j.Items, nil
	}

	// First drive: run the expander ONCE, encode {N + items} as a JSON string, journal it, flush durably BEFORE
	// branch 1. checkpointFrom is non-nil (the F4 gate in Execute guaranteed it).
	resolved, err := a.expander(ctx, parentData)
	if err != nil {
		return nil, fmt.Errorf("fan-out %q expander: %w", a.nodeName, err)
	}
	items := make([]json.RawMessage, len(resolved))
	for i, it := range resolved {
		// AF2: the CRASH axis, BEFORE the marshal. The expander is host code returning
		// arbitrary values, so this is where a fan-out gets one.
		if derr := checkValueDepth(it, fmt.Sprintf("fan-out %q item %d", a.nodeName, i)); derr != nil {
			return nil, derr
		}
		enc, merr := json.Marshal(it)
		if merr != nil {
			return nil, fmt.Errorf("%w: fan-out %q item %d not JSON-encodable: %w", ErrValidation, a.nodeName, i, merr)
		}
		// DEPTH CAP AT THE WRITE, because this journal is read back by a decoder that
		// has one and the snapshot guard structurally cannot see it.
		//
		// The journal is stored as a JSON *string* (Set below), so in the snapshot its
		// nesting lives inside a string literal — and jsonNestingDepth correctly skips
		// string contents. That is the guard working, not failing. But resolveExpansion
		// reads it back with json.Unmarshal, which IS capped at maxJSONNestingDepth by
		// the stdlib scanner. Without this check the pair is: WRITE ACCEPTS, RESUME
		// REFUSES, permanently — a wedged run rather than a rejected input. Refusing
		// here turns a lost workflow into a loud validation error at the moment the bad
		// value is produced.
		if derr := checkJSONDepth(enc, fmt.Sprintf("fan-out %q item %d", a.nodeName, i)); derr != nil {
			return nil, derr
		}
		items[i] = enc
	}
	// NO checkValueDepth here, and the reason is an ARGUMENT rather than an oversight —
	// stated because a reader pairing marshal sites against pre-marshal walks will
	// otherwise find a gap. items is []json.RawMessage, and RawMessage implements
	// json.Marshaler, so the walk would stop at every element without descending: it can
	// only ever return nil. Each item was walked individually at its own marshal above,
	// while it was still a live Go value, which is the only point at which it is walkable.
	// If Items ever stops being []json.RawMessage that argument breaks and this site needs
	// its own walk.
	journal, err := json.Marshal(fanOutJournal{N: len(items), Items: items})
	if err != nil {
		return nil, fmt.Errorf("fan-out %q journal encode: %w", a.nodeName, err)
	}
	// The whole journal too, not only the items: the envelope adds levels, and the
	// decoder's cap applies to the document it actually parses.
	if derr := checkJSONDepth(journal, fmt.Sprintf("fan-out %q journal", a.nodeName)); derr != nil {
		return nil, derr
	}
	parentData.Set(fanOutItemsKey(a.nodeName), string(journal))
	cp := checkpointFrom(ctx)
	if err := cp(parentData); err != nil {
		return nil, fmt.Errorf("fan-out %q expansion checkpoint: %w", a.nodeName, err)
	}
	return items, nil
}

// driveBranch runs branch idx as a child workflow under its deterministic ID, using the SAME per-child
// idempotency as subWorkflowAction (subworkflow.go:285-310): if the child is already durably complete, read its
// result WITHOUT re-executing (crash-after-branch-k idempotency, N-wide, from the DETERMINISTIC ID); else drive
// it. Returns the branch's declared result value (resultFrom key) for the aggregate.
func (a *fanOutAction) driveBranch(ctx context.Context, store WorkflowStore, parentID string, idx int, item interface{}) (interface{}, error) {
	childID := FanOutChildID(parentID, a.nodeName, idx)

	// Terminal-fast-path (optimization, not the guarantee): an already-complete branch child is a no-op on resume.
	if existing, err := store.Load(childID); err == nil && existing != nil {
		if childUnambiguouslyComplete(existing) {
			return a.readBranchResult(existing)
		}
		// non-terminal / Failed → fall through and (re)drive; the child's own resume-idempotency handles it.
	} else if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("fan-out %q branch %d: load child %q: %w", a.nodeName, idx, childID, err)
	}

	branchDAG := a.branch(idx, item)
	child := &Workflow{dag: branchDAG, WorkflowID: childID, Store: store}
	if err := child.Execute(withDriveID(ctx, childID)); err != nil {
		return nil, err // includes ctx.Canceled when a sibling fail-fasts this branch mid-run.
	}
	final, err := store.Load(childID)
	if err != nil {
		return nil, fmt.Errorf("fan-out %q branch %d: reload child %q: %w", a.nodeName, idx, childID, err)
	}
	return a.readBranchResult(final)
}

// itemForBranch decodes a journaled item (raw JSON) back to a Go value for the branch factory, using UseNumber()
// so a JSON number arrives as json.Number — INT64-FAITHFUL (a large int64 item, e.g. an ID or a nanos timestamp,
// survives full range; a DEFAULT decode into interface{} yields float64 and CORRUPTS above 2^53, the
// [[first-ci-run-saga]] fidelity bug on the item axis). A branch reads its item as json.Number and calls
// .Int64()/.Float64() for its concrete type. A decode failure yields the raw JSON bytes as a string.
func itemForBranch(raw json.RawMessage) interface{} {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return string(raw)
	}
	return v
}

// readBranchResult reads the branch's declared result key from the child data (the aggregate element). No
// resultFrom → nil element (the branch ran for effect only).
func (a *fanOutAction) readBranchResult(childData *WorkflowData) (interface{}, error) {
	if a.resultFrom == "" {
		return nil, nil //nolint:nilnil // a nil element is the valid "branch ran for effect only" result, not an error
	}
	v, ok := childData.Get(a.resultFrom)
	if !ok {
		return nil, fmt.Errorf("%w: fan-out %q branch result key %q not present in child data", ErrValidation, a.nodeName, a.resultFrom)
	}
	return v, nil
}
