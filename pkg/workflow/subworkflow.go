package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
)

// M19 ph91 — Sub-workflow inline core. A parent node spawns + awaits a child workflow
// IN-PROCESS: it runs the child's DAG.Execute under the child's own deterministic ID +
// its own journal (parent and child are DISTINCT workflows — M16 one-writer preserved).
// This is the zero-infra default of the resolved Option C hybrid (GATE-M19-EXEC-MODEL);
// the parked-await WAKE is ph92, the SQLite mailbox ph93, the queue/type-ref/suspendable
// path ph94. Inline BLOCKS on the child (does not park) — which is exactly why an inline
// child must be non-suspendable (enforced by the build-time closure-scan below).

// ErrSubWorkflowRequiresStore is returned when a sub-workflow node is reached but no
// parent Store is in scope (a bare DAG.Execute with no Workflow.executeLocked injection).
// A child cannot persist its own journal without a store, so this is a loud configuration
// failure — the durability-honesty analog of ErrWaitRequiresSignalStore, never a silent
// in-memory-only spawn.
var ErrSubWorkflowRequiresStore = errors.New("workflow cannot spawn a sub-workflow: no parent store in scope")

// ErrSubWorkflowSuspendableChild is returned at build time when an inline sub-workflow's
// definition-value child (or any transitive descendant) contains a suspendable node. An
// inline child BLOCKS the parent goroutine, so it can never park-and-resume; a suspendable
// child would hang. Such a child must be dispatched via the queue path (ph94) instead.
var ErrSubWorkflowSuspendableChild = errors.New("inline sub-workflow child contains a suspendable node: route it to the queue-dispatch path instead")

// ErrSubWorkflowCycle is returned when a sub-workflow would spawn a child whose
// deterministic ID equals an ID already on the drive stack (an ancestor). The per-WorkflowID
// drive lease is NON-reentrant (lease.go), so re-driving the same ID from within its own
// drive self-deadlocks — this refuses loudly BEFORE the lease is acquired.
var ErrSubWorkflowCycle = errors.New("sub-workflow child ID collides with an ancestor on the drive stack (would self-deadlock)")

// ErrSubWorkflowResultKeyCollision is returned when a sub-workflow's declared result key
// already exists in the parent data (and was not written by a prior spawn of this same
// node). Overwriting would be a silent last-writer-wins; this fails loudly instead.
var ErrSubWorkflowResultKeyCollision = errors.New("sub-workflow declared result key collides with an existing parent data key")

// --- parent-store ctx injection (Task 0) ---
//
// subWorkflowAction.Execute is handed only ctx + the parent's *WorkflowData, never the
// parent's Store. The store is threaded on ctx exactly like the Clock (clock.go) and the
// SignalStore (signal.go): injected once at Workflow.executeLocked (the single point where
// w.Store is in scope), read here to build the child Workflow. Ctx-injection keeps the
// action stateless and means a normal Execute reaches the store through the identical seam.

type parentStoreCtxKey struct{}

// withParentStore returns a child context carrying the parent's Store for a sub-workflow
// spawn. A nil store is not injected (parentStoreFrom then returns nil → the loud guard).
func withParentStore(ctx context.Context, s WorkflowStore) context.Context {
	if s == nil {
		return ctx
	}
	return context.WithValue(ctx, parentStoreCtxKey{}, s)
}

// parentStoreFrom extracts the injected parent Store, returning nil when none was injected
// (a bare DAG.Execute, or a Workflow with no Store).
func parentStoreFrom(ctx context.Context) WorkflowStore {
	if s, ok := ctx.Value(parentStoreCtxKey{}).(WorkflowStore); ok {
		return s
	}
	return nil
}

// --- ancestor drive-stack (Task 2 — the child-ID ≠ ancestor invariant) ---
//
// The set of WorkflowIDs currently being driven on this goroutine's spawn chain. A child
// whose deterministic ID is already in the set would, on child.Execute, re-acquire the
// per-ID (non-reentrant) drive lease held by that ancestor → self-deadlock. We refuse
// BEFORE the lease is acquired. The set is threaded on ctx and grows by exactly the child
// ID for the child's own drive.

// ErrSubWorkflowMaxDepth is returned when a sub-workflow spawn is reached at nesting
// depth >= the ceiling (default 8, override via Workflow.MaxSubWorkflowDepth). The
// COMP-CLOSE unbounded-nesting DoS boundary: loud, never a park, never a silent cap.
var ErrSubWorkflowMaxDepth = fmt.Errorf("%w: sub-workflow nesting depth exceeds the ceiling", ErrValidation)

// defaultMaxSubWorkflowDepth is the nesting ceiling when MaxSubWorkflowDepth is unset (0).
const defaultMaxSubWorkflowDepth = 8

// maxSubWorkflowDepthCap is an ABSOLUTE upper bound on a carried work_queue.depth (F-P95-01). A legit
// row is bounded ≤ the ceiling by the enqueue-time refusal, so a depth beyond this cap can only be a
// forged/bit-rotted row (the corrupt-store-as-input-TCB the defensive read exists to harden against).
// The read fail-safe-refuses such a row rather than letting RunNext seed a giant drive-stack. 1024 is
// ~128× the default ceiling — comfortably above any sane override, far below an allocation-storm size.
const maxSubWorkflowDepthCap = 1024

type maxDepthCtxKey struct{}

// withMaxSubWorkflowDepth returns a ctx carrying the nesting ceiling. A non-positive ceiling
// (0 = unset, or negative) is normalized to the default — the ceiling is ALWAYS a positive
// bound (there is no "unbounded nesting" mode; that would defeat the DoS guard).
func withMaxSubWorkflowDepth(ctx context.Context, ceiling int) context.Context {
	if ceiling <= 0 {
		ceiling = defaultMaxSubWorkflowDepth
	}
	return context.WithValue(ctx, maxDepthCtxKey{}, ceiling)
}

// maxSubWorkflowDepthFrom returns the injected ceiling, or 0 when none is present (the
// "not yet injected" sentinel executeLocked checks to inject exactly once).
func maxSubWorkflowDepthFrom(ctx context.Context) int {
	if c, ok := ctx.Value(maxDepthCtxKey{}).(int); ok {
		return c
	}
	return 0
}

// effectiveMaxDepth is the ceiling a spawn action enforces: the injected value, or the
// default if none was injected (a bare DAG.Execute with no executeLocked injection still
// gets the DoS bound — the guard is never absent).
func effectiveMaxDepth(ctx context.Context) int {
	if c := maxSubWorkflowDepthFrom(ctx); c > 0 {
		return c
	}
	return defaultMaxSubWorkflowDepth
}

// depthExceeded reports whether a spawn at this point would breach the ceiling. The current
// nesting depth is the number of ancestor drives on the stack (ph91's driveStack); a spawn
// at depth d is refused when d >= ceiling (ceiling=8 permits ancestors 0..7, refusing the
// 9th spawn). The queue path seeds the drive-stack size from the carried depth (ph95 Slice B).
func depthExceeded(ctx context.Context) bool {
	return len(driveStackFrom(ctx)) >= effectiveMaxDepth(ctx)
}

// withDepthSeed returns a ctx whose drive stack is pre-populated with n synthetic ancestor
// entries, so a queue child driven by RunNext (a SEPARATE drive from its parent — the parent's
// drive-stack ctx does NOT cross the dispatch) starts at nesting depth n = its carried
// work_queue.depth (M19 ph95, F-P94-04). This keeps depthExceeded's len(driveStack) semantics
// UNIFORM across the inline and queue paths — the queue child's own spawns accumulate on top of
// the seeded base, so a type-ref chain A->B->C... is bounded by the same ceiling as an inline
// chain. The seed IDs are placeholders ("ph95-depth-seed:i") — never real ancestor IDs; a genuine
// childID is a "sub:"-prefixed sha256, structurally distinct, so the cycle guard cannot false-match.
//
// Built in ONE O(n) pass (copy the parent stack once, then add n entries in place) — NOT n calls to
// withDriveID, which each copy the whole map = O(n²) (F-P95-01). n is bounded ≤ maxSubWorkflowDepthCap
// by the caller's defensive read, so even a forged large depth cannot drive an allocation storm.
func withDepthSeed(ctx context.Context, n int) context.Context {
	if n <= 0 {
		return ctx
	}
	prev := driveStackFrom(ctx)
	next := make(map[string]struct{}, len(prev)+n)
	for k := range prev {
		next[k] = struct{}{}
	}
	for i := 0; i < n; i++ {
		next[fmt.Sprintf("ph95-depth-seed:%d", i)] = struct{}{}
	}
	return context.WithValue(ctx, driveStackCtxKey{}, next)
}

type driveStackCtxKey struct{}

// driveStackFrom returns the set of ancestor drive IDs (nil-safe: no stack → empty).
func driveStackFrom(ctx context.Context) map[string]struct{} {
	if s, ok := ctx.Value(driveStackCtxKey{}).(map[string]struct{}); ok {
		return s
	}
	return nil
}

// withDriveID returns a ctx whose drive stack additionally contains id. It copies the set
// (never mutates the parent's), so sibling spawns on the same parent do not see each
// other's IDs — only true ancestors are in scope.
func withDriveID(ctx context.Context, id string) context.Context {
	prev := driveStackFrom(ctx)
	next := make(map[string]struct{}, len(prev)+1)
	for k := range prev {
		next[k] = struct{}{}
	}
	next[id] = struct{}{}
	return context.WithValue(ctx, driveStackCtxKey{}, next)
}

// subWorkflowChildID derives the deterministic child WorkflowID from the parent ID and the
// sub-workflow node name. Reuses the IdempotencyKey framing discipline (an 8-byte LE length
// prefix on parentID so the (parentID, nodeName) split is unambiguous — ("ab","c") and
// ("a","bc") never collide). Resume-stable: the same (parent, node) always yields the same
// child ID, so a re-drive finds the same child. Prefixed "sub:" so a child ID is visibly
// distinct from a top-level workflow ID.
func subWorkflowChildID(parentID, nodeName string) string {
	h := sha256.New()
	var lenPrefix [8]byte
	binary.LittleEndian.PutUint64(lenPrefix[:], uint64(len(parentID)))
	h.Write(lenPrefix[:])
	h.Write([]byte(parentID))
	h.Write([]byte(nodeName))
	return "sub:" + hex.EncodeToString(h.Sum(nil))
}

// --- build-time closure-scan (Task 4 shell; recursion is the load-bearing part) ---
//
// scanChildInlineSafe recursively verifies a definition-value child's whole spawn-closure
// contains NO suspendable node: it type-asserts each node's DIRECT Action against
// suspendableAction (the same shape the executor matches — suspend.go), and recurses into
// any nested sub-workflow node's own child DAG. A suspendable node anywhere in the closure
// → ErrSubWorkflowSuspendableChild. A nested type-ref (queue) sub-workflow is not
// statically knowable (opaque DAGFactory) — ph94 introduces that node type; until then a
// definition-value closure is fully scannable.
//
// A visited-set (keyed by the *DAG pointer identity) makes the scan TOTAL over any
// definition-value graph shape (F91-2): a by-value CYCLE (a child that transitively contains
// itself) terminates as a refusal instead of a stack-overflow, and a DIAMOND (a grandchild
// shared by two references) is scanned ONCE instead of 2^depth times. Both are build-time
// DoS classes on an otherwise-legitimate graph; the visited-set removes them.
func scanChildInlineSafe(child *DAG) error {
	return scanChildInlineSafeVisited(child, make(map[*DAG]struct{}))
}

func scanChildInlineSafeVisited(child *DAG, visited map[*DAG]struct{}) error {
	if child == nil {
		return fmt.Errorf("%w: sub-workflow child DAG is nil", ErrValidation)
	}
	if _, seen := visited[child]; seen {
		return nil // already scanned this DAG (a diamond) or we are inside a cycle → stop, don't recurse
	}
	visited[child] = struct{}{}
	for _, node := range child.Nodes {
		if _, ok := node.Action.(suspendableAction); ok {
			return fmt.Errorf("%w: node %q in child %q", ErrSubWorkflowSuspendableChild, node.Name, child.Name)
		}
		// Recurse into a nested definition-value sub-workflow child (the TRANSITIVE case).
		if sub, ok := node.Action.(*subWorkflowAction); ok {
			if err := scanChildInlineSafeVisited(sub.child, visited); err != nil {
				return err
			}
		}
	}
	return nil
}

// --- the inline spawn action (Task 3) ---

// subWorkflowAction runs a definition-value child workflow in-process under a deterministic
// child ID, awaits its terminal state, and populates the declared result key on success.
// It is NOT suspendable (it blocks on child.Execute) — the closure-scan guarantees the
// child cannot park, so the block always terminates.
type subWorkflowAction struct {
	nodeName   string // the parent node's name (keys the deterministic child ID)
	child      *DAG   // the definition-value child (its own graph, scanned inline-safe at build)
	resultKey  string // the parent data key the child's result is written to (may be empty)
	resultFrom string // the child node name whose output is the result (may be empty → no result)
}

// Execute spawns + awaits the child (idempotent per PIN-2). NOT a suspension: it blocks.
func (a *subWorkflowAction) Execute(ctx context.Context, parentData *WorkflowData) error {
	// Nesting-DoS ceiling (M19 ph95): refuse a spawn at or past the depth ceiling BEFORE any
	// work. Depth = ancestor drives on the stack; loud typed error, never a park/silent cap.
	if depthExceeded(ctx) {
		return fmt.Errorf("%w: node %q at depth %d", ErrSubWorkflowMaxDepth, a.nodeName, len(driveStackFrom(ctx)))
	}
	store := parentStoreFrom(ctx)
	if store == nil {
		return ErrSubWorkflowRequiresStore
	}
	childID := subWorkflowChildID(parentData.GetWorkflowID(), a.nodeName)

	// Ancestor-cycle guard (Task 2): refuse BEFORE child.Execute acquires the non-reentrant
	// per-ID lease. A child ID already on the drive stack is an ancestor → self-deadlock.
	if _, onStack := driveStackFrom(ctx)[childID]; onStack {
		return fmt.Errorf("%w: child ID %q", ErrSubWorkflowCycle, childID)
	}

	// Idempotency is provided by the DETERMINISTIC child ID above + the child's OWN
	// resume-idempotency: re-driving child.Execute on an already-terminal child journal does
	// NOT re-run the child's actions (dag.go leaves terminal nodes as-is), so spawn-count
	// stays 1 across any parent re-drive OR crash-mid-child window — that is the load-bearing
	// idempotent-spawn guarantee (proven by the deterministic-ID seed-break in the tests).
	//
	// This terminal-fast-path is an OPTIMIZATION + the terminal-Failed router, NOT the
	// idempotency guard: if the child is already durably terminal we act on the outcome
	// directly (skip a redundant re-Execute + reload for the Completed case; surface a
	// terminal-Failed child as the parent-node failure without re-driving a run that can only
	// fail again). A non-terminal or absent child falls through to the drive below.
	if existing, err := store.Load(childID); err == nil && existing != nil {
		if childUnambiguouslyComplete(existing) {
			// Fast-path: the child is durably complete with no Failed node → populate the
			// result + return without re-driving (skip a redundant re-Execute + reload).
			return a.applyResult(parentData, existing)
		}
		// else: not-yet-complete OR a Failed node whose coe-vs-fail-fast verdict only
		// child.Execute can render → fall through and drive/resume it (authoritative).
	} else if err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("sub-workflow %q: load child %q: %w", a.nodeName, childID, err) // corrupt/IO — don't spawn over a bad read
	}

	// Drive the child in-process under its own ID + journal + the parent's store. The child
	// ID is pushed on the drive stack so a grandchild that would re-use an ancestor ID is
	// caught by the guard above. Parent ctx cancellation flows into child.Execute for free.
	child := &Workflow{DAG: a.child, WorkflowID: childID, Store: store}
	if err := child.Execute(withDriveID(ctx, childID)); err != nil {
		return err // child failure → non-ErrSuspended → parent node Failed (INV-01 terminal-no-op)
	}

	// Child succeeded: read its final state and populate the declared result key.
	final, err := store.Load(childID)
	if err != nil {
		return fmt.Errorf("sub-workflow %q: reload completed child %q: %w", a.nodeName, childID, err)
	}
	return a.applyResult(parentData, final)
}

// applyResult reads the child's result from its declared DATA KEY (resultFrom) and writes it
// to the declared parent key, with a collision check (Task 4). No resultKey → nothing to do.
//
// ⚠ FIDELITY — read a child DATA key, NOT a node OUTPUT. Data keys carry the store's TYPED
// columns (value_long for int64, plus string/bool/float), so a SCALAR result round-trips
// type-faithfully on all three stores (an int64 reloads AS an int64 on InMemory/FB/SQLite).
// Node OUTPUTS reload as a raw JSON STRING on FB and SQLite (they have no typed column), so
// reading the result from an output would corrupt an int64 child result into a string on 2
// of 3 stores — the data-key read avoids that (verified across all three stores).
//
// SCOPE (F91-1): this type-faithfulness covers the SCALAR types the stores type-column
// (int64/string/bool/float). A COMPLEX result (a map/slice/nil) is NOT store-uniform: it
// reloads typed on InMemory but as a JSON string on FB/SQLite — the SAME pre-existing
// store-wide property that governs every complex data value (workflow_store.go:655 default→
// JSON string), not a sub-workflow-specific behavior. A sub-workflow whose result must be
// backend-uniform should declare a scalar result key; a complex result is subject to the
// store's serialization exactly as any other complex data value is.
func (a *subWorkflowAction) applyResult(parentData *WorkflowData, childData *WorkflowData) error {
	if a.resultKey == "" || a.resultFrom == "" {
		return nil
	}
	result, ok := childData.Get(a.resultFrom)
	if !ok {
		return fmt.Errorf("%w: sub-workflow %q declared result key %q not present in child data", ErrValidation, a.nodeName, a.resultFrom)
	}
	// Collision check: refuse to overwrite a pre-existing parent key. A prior spawn of THIS
	// node writes the SAME key with the SAME value, which is the idempotent re-apply — allow
	// that (equal value), refuse a foreign pre-existing value (last-writer-wins hazard).
	// reflect.DeepEqual (not !=) so a non-comparable child result (a slice/map) does not PANIC
	// the comparison — DeepEqual is total over any value type.
	if existing, present := parentData.Get(a.resultKey); present && !reflect.DeepEqual(existing, result) {
		return fmt.Errorf("%w: key %q (node %q)", ErrSubWorkflowResultKeyCollision, a.resultKey, a.nodeName)
	}
	parentData.Set(a.resultKey, result)
	return nil
}

// childUnambiguouslyComplete reports whether the terminal FAST-PATH may short-circuit the
// spawn: true only when the loaded child is UNAMBIGUOUSLY complete — every node terminal AND
// none Failed. It returns false the moment ANY node is Failed, because a Failed node does NOT
// by itself mean the child failed: a ContinueOnError node fails (status Failed) yet
// DAG.Execute returns SUCCESS (the coe contract, parallel_execution.go). Only child.Execute
// encodes that semantics, so on any Failed node the caller falls through and lets child.Execute
// (a cheap no-op on a terminal child — resume-idempotent) render the authoritative verdict.
// This keeps the non-crash path and the crash-resume fast-path CONSISTENT for a coe child
// (FIND-P91-R1). A child with no persisted nodes, or any non-terminal node, is likewise not
// complete (fall through and resume it).
func childUnambiguouslyComplete(childData *WorkflowData) bool {
	statuses := childData.GetAllNodeStatuses()
	if len(statuses) == 0 {
		return false
	}
	for _, st := range statuses {
		if !isTerminalStatus(st) || st == Failed {
			return false
		}
	}
	return true
}

// childTerminal reports whether a loaded child run is durably terminal — every persisted node
// carries a terminal status and there is at least one node. Unlike childUnambiguouslyComplete
// (which is false on a Failed node so the INLINE path defers to child.Execute), this admits a
// Failed node: the PARKED path (ph92) has no child.Execute to defer to, so it must render the
// verdict itself from (statuses + DAG) via childRunFailed below.
func childTerminal(childData *WorkflowData) bool {
	statuses := childData.GetAllNodeStatuses()
	if len(statuses) == 0 {
		return false
	}
	for _, st := range statuses {
		if !isTerminalStatus(st) {
			return false
		}
	}
	return true
}

// childRunFailed renders the run verdict from a loaded child journal + its DAG. The run failed if
// EITHER of two conditions holds for any node:
//   - status Failed AND the node is NOT ContinueOnError (the coe-aware fail-fast rule — a coe
//     Failed node is tolerated and never a run failure, matching the executor live at
//     parallel_execution.go:206);
//   - status Compensated or CompensationFailed — a saga ROLLBACK occurred (M12), which only
//     happens on a FAILED run. A cancel/deadline-triggered rollback (workflow.go) can leave a
//     child terminalized with {Compensated, CompensationFailed, Completed} and NO Failed node —
//     so a Failed-only check would render a FALSE SUCCESS and silently converge past a
//     CompensationFailed ("effect not undone"). Treating a rollback node as failure keeps the
//     parked verdict IDENTICAL to the inline path (which returns the *SagaError / cancel error).
//     (F-P92-01.)
//
// This is the ONE shared verdict callable for the parked path (DEC-P92-COE-VERDICT-FROM-DAG) —
// a pure read over (DAG, WorkflowData); no re-execution, no write. firstFailed is the
// deterministic (lowest-name) offending node, for a stable error message.
//
// LIMITATION (documented): a pure-CANCEL/deadline outcome that left NO Failed and NO rollback
// node (a run cancelled before any node failed or compensated) is NOT reconstructable from node
// statuses alone — the inline path surfaces the ctx error, the parked path cannot see it. In
// ph92 the child is run out-of-band by the (manual/ph94) producer; a cancelled child that
// terminalized cleanly is out of scope here and is the ph94 producer's responsibility to signal.
func childRunFailed(dag *DAG, childData *WorkflowData) (failed bool, firstFailed string) {
	consider := func(name string) {
		if firstFailed == "" || name < firstFailed {
			failed = true
			firstFailed = name
		}
	}
	for name, st := range childData.GetAllNodeStatuses() {
		switch st {
		case Failed:
			if node, ok := dag.Nodes[name]; ok && node.ContinueOnError {
				continue // a coe Failed node is tolerated — not a run failure
			}
			consider(name)
		case Compensated, CompensationFailed:
			consider(name) // a rollback node → the run failed (rollback implies failure)
		}
	}
	return failed, firstFailed
}
