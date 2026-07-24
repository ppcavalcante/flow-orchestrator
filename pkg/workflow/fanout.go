// M21 ph105 — Branch-execution engine. The PRODUCTION fan-out mechanism, promoted from the ph104 spike
// (846c2e2, build-tagged) after the crux was proven. A fan-out is a SINGLE ordinary DAG node whose Execute:
//   (1) resolves N via an expander ONCE and journals {N + per-item keys} durably BEFORE branch 1 (expansion-once,
//       via the checkpointFrom(ctx) reserved-key flush — the moat's no-replay leg);
//   (2) drives N branches in the node's OWN MaxConcurrency-bounded pool (read from ctx via the withMaxConcurrency
//       seam), under a cancellable sub-context so a FailFast failure cancels in-flight siblings;
//   (3) aggregates node[i] in DISCOVERY order (index-addressed, assembled after the pool drains).
// Crash-after-branch-k idempotency comes from the DETERMINISTIC child ID + child.Execute resume-idempotency
// (subworkflow.go:285-310) — NOT the terminal-fast-path (which is an optimization; the 104 correction).
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

// fanOutExpander resolves the runtime fan-out width. Returns the ordered discovery list of per-branch inputs;
// len(items) == N. Runs EXACTLY ONCE across a crash+resume (its result is journaled).
type fanOutExpander func(ctx context.Context, parentData *WorkflowData) ([]interface{}, error)

// fanOutBranch builds the single-node child DAG that processes item i. A factory (not a prebuilt DAG) so each
// branch gets a distinct action closure without sharing action state across branches.
type fanOutBranch func(index int, item interface{}) *DAG

// branchDAGFromAction wraps a user branchAction into the per-branch single-node child DAG factory. The wrapper
// node injects the item under FanOutItemKey into the branch child's data BEFORE the user action runs, so the
// action reads its item with data.Get(FanOutItemKey) (the non-generic item-via-WorkflowData contract). The
// result the action Sets under resultFrom is read back by driveBranch for the typed node[i] keying.
func branchDAGFromAction(branchAction Action) fanOutBranch {
	return func(_ int, item interface{}) *DAG {
		b := NewWorkflowBuilder()
		b.AddStartNode("branch").WithAction(ActionFunc(func(ctx context.Context, d *WorkflowData) error {
			d.Set(FanOutItemKey, item) // the per-branch item, read by branchAction via data.Get(FanOutItemKey)
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
func (b *WorkflowBuilder) AddFanOut(name string, expander fanOutExpander, branchAction Action) *NodeBuilder {
	node := b.AddNode(name)
	if expander == nil || branchAction == nil {
		node.actionErr = fmt.Errorf("%w: AddFanOut %q requires a non-nil expander and branchAction", ErrValidation, name)
		return node
	}
	node.action = &fanOutAction{
		nodeName: name,
		expander: expander,
		branch:   branchDAGFromAction(branchAction),
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
func (n *NodeBuilder) WithCollectPartial() *NodeBuilder {
	a, ok := n.action.(*fanOutAction)
	if !ok {
		n.actionErr = fmt.Errorf("%w: WithCollectPartial is only valid on an AddFanOut node", ErrValidation)
		return n
	}
	a.collectPartial = true
	return n
}

// fanOutAction is the production fan-out node. It generalizes subWorkflowAction 1→N: N deterministic child runs
// under (parentID, nodeName, index)-derived IDs, driven by the node's OWN bounded pool under a cancellable
// sub-context, aggregated in discovery order. The expander result is journaled (expansion-once) before any branch.
type fanOutAction struct {
	nodeName       string
	expander       fanOutExpander
	branch         fanOutBranch
	resultFrom     string // the branch child DATA key each branch's result is read from (typed, value_long-faithful)
	resultKey      string // the parent BASE key; per-branch results land TYPED under resultKey[i] + a count key
	maxWidth       int    // per-node width cap (≤0 → DefaultFanOutMaxWidth); exceed → ErrFanOutMaxWidth
	collectPartial bool   // FANOUT-05: true → all branches run, node Completes with a {succeeded, failed} partition; false (default) → FailFast
}

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

// subFanOutChildID derives the deterministic per-branch child ID from (parentID, nodeName, index) via ONE hash
// (F3 — drops the spike's brittle base[len("sub:"):] prefix-slice). The 8-byte LE length prefixes on parentID and
// nodeName make every (parentID, nodeName, index) split unambiguous (("ab","c",0) and ("a","bc",0) never
// collide), and the index is folded into the SAME hash — resume-stable (same index → same ID) and collision-safe
// vs a 2-field "sub:" child (distinct "fan:" prefix + the index in the digest).
func subFanOutChildID(parentID, nodeName string, index int) string {
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

	sem := make(chan struct{}, capN)
	results := make([]interface{}, n)
	errs := make([]error, n)
	var wg sync.WaitGroup

	for i := 0; i < n; i++ {
		// FailFast: once a sibling has failed (branchCtx cancelled), do NOT launch un-started branches. Record a
		// cancellation error for the skipped index so the aggregate loop below fails the node deterministically.
		if branchCtx.Err() != nil {
			errs[i] = branchCtx.Err()
			continue
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-branchCtx.Done():
				errs[idx] = branchCtx.Err()
				return
			}
			res, berr := a.driveBranch(branchCtx, store, parentID, idx, itemForBranch(items[idx]))
			results[idx] = res
			errs[idx] = berr
			if berr != nil && !a.collectPartial {
				cancel() // FailFast (default): first failure cancels in-flight + un-started siblings.
				// CollectPartial: do NOT cancel — every branch runs to completion.
			}
		}(i)
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
			if existing, present := parentData.Get(key); present && !reflect.DeepEqual(existing, results[i]) {
				return fmt.Errorf("%w: indexed key %q (node %q)", ErrFanOutResultKeyCollision, key, a.nodeName)
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
		enc, merr := json.Marshal(it)
		if merr != nil {
			return nil, fmt.Errorf("%w: fan-out %q item %d not JSON-encodable: %w", ErrValidation, a.nodeName, i, merr)
		}
		items[i] = enc
	}
	journal, err := json.Marshal(fanOutJournal{N: len(items), Items: items})
	if err != nil {
		return nil, fmt.Errorf("fan-out %q journal encode: %w", a.nodeName, err)
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
	childID := subFanOutChildID(parentID, a.nodeName, idx)

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
	child := &Workflow{DAG: branchDAG, WorkflowID: childID, Store: store}
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
