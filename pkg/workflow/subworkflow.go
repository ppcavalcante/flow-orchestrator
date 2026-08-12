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
// child would hang.
//
// TWO paths accept a suspendable child, and the message names both because they are not
// ordered by weight in one direction (F-PARK-04):
//   - AddSubWorkflowParked — the host runs the child itself; needs a SignalStore.
//   - AddSubWorkflowQueued — the engine dispatches it; needs a multi-process *SQLiteStore
//     plus a worker Pool and a Registry.
//
// Naming only the queue path over-steered callers onto the heaviest option. Naming only
// parked would over-steer the other way: parked is lighter than queue, but HEAVIER than
// inline (inline runs on a bare WorkflowStore), so a caller who could have restructured to
// stay inline should not be pushed off it either. State the requirement of each and let
// the caller choose.
var ErrSubWorkflowSuspendableChild = errors.New("inline sub-workflow child contains a suspendable node: inline cannot park, so use AddSubWorkflowParked (host runs the child; requires a SignalStore) or AddSubWorkflowQueued (engine dispatches it; requires a multi-process *SQLiteStore, Pool and Registry)")

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

// SubWorkflowChildID derives the deterministic child WorkflowID from the parent ID and the
// sub-workflow node name.
//
//	digest = SHA-256( uint64-LE(len(parentID)) || parentID || nodeName )
//	id     = "sub:" + hex(digest)
//
// The 8-byte little-endian length prefix on parentID frames the boundary between the two
// fields so the split point is unambiguous: ("ab","c") and ("a","bc") yield distinct IDs
// (a naive concatenation would collide). This construction is a STABLE CONTRACT —
// downstream systems may recompute it, so it must not change across versions without a
// deliberate, documented break. The same guarantee IdempotencyKey carries.
//
// Resume-stable: the same (parent, node) always yields the same child ID, so a re-drive
// finds the same child. Prefixed "sub:" so a child ID is visibly distinct from a
// top-level workflow ID.
//
// Exported because the PARKED sub-workflow pattern is not otherwise reachable: the host
// runs the child itself and must know the WorkflowID to run it under. Recompute it here
// rather than reimplementing the framing — the length prefix is a collision guard, not
// incidental.
func SubWorkflowChildID(parentID, nodeName string) string {
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
	for _, node := range child.nodes {
		// M23 SEAL-09: gate on the node-indexed capability, not the action's dynamic
		// type. Same set, derived once at mint (node.go) — see Node.suspendable.
		if node.suspendable {
			return fmt.Errorf("%w: node %q in child %q", ErrSubWorkflowSuspendableChild, node.name, child.name)
		}
		// Recurse into a nested definition-value sub-workflow child (the TRANSITIVE case).
		if sub, ok := node.action.(*subWorkflowAction); ok {
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
	childID := SubWorkflowChildID(parentData.GetWorkflowID(), a.nodeName)

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
	child := &Workflow{dag: a.child, WorkflowID: childID, Store: store}
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
	//
	// reflect.DeepEqual (not !=) so a non-comparable child result (a slice/map) does not PANIC
	// the comparison. DeepEqual is total over any value TYPE — which is why it beat `!=` —
	// and that is NOT the same as total over any value, which is what this comment used to
	// imply. It is NOT total over DEPTH: DeepEqual recurses, and it dies on a deep value at
	// ~922 bytes of goroutine stack per level. See checkValueDepth.
	//
	// 116-AF2, and this site is the REPRODUCED one: the defect was driven end-to-end from an
	// external module through the public builder API alone, at depth 650,000, and the child
	// exited with `fatal error: stack overflow` under reflect.deepValueEqual.
	//
	// THE CRASH IS ON THE SUCCESS PATH, which is the whole severity. Reaching DeepEqual at
	// all means a value is already present; if the two are EQUAL this function returns nil
	// and the run proceeds. So an ordinary idempotent re-apply of a sub-workflow result — a
	// crash-resume replaying a completed child, the engine's most normal operation — kills
	// the host process. No error path, no adversarial shape.
	//
	// AND NO MARSHAL GUARD REACHES IT ON THE BACKEND THAT REPRODUCES IT. That qualifier is
	// the correction: the three measurements below are all about InMemoryStore, and the
	// sentence that used to end this paragraph — "the value never meets an encoder before it
	// meets DeepEqual" — is store-specific written as general. On this backend Save clones
	// and never marshals; 650,000 is BELOW json.Marshal's own death at ~721,914; and cloneMap
	// is iterative, so the clone costs heap, not stack.
	//
	// On JSONFile / FlatBuffers / SQLite the child result HAS met an encoder — see the
	// 116-AF6-R2 note below, which is exactly why the residual there is InMemoryStore-only.
	//
	// Guarded on the SAME bound as the encoders because DeepEqual is the tighter class —
	// it dies FIRST (~922 B/level vs the encoder's ~743), so a bound sound for it is sound
	// for both.
	//
	// 🔴 116-AF9: "ONE SIDE IS ENOUGH" STOOD HERE AND IS FALSE — and it was stated in the
	// imperative, telling a future reader not to "complete" it with a second check on
	// `existing`. It therefore instructed them to reintroduce the defect this phase fixed.
	// The proof (deepValueEqual descends only where BOTH values have the corresponding
	// element, so its depth is bounded by the SHALLOWER of the two) is about STRUCTURAL
	// depth and holds only for ACYCLIC values. For a cyclic pair both structural depths are
	// infinite and min() says nothing; what terminates deepValueEqual is its memo, and that
	// memo matches on a repeated PAIR. checkDeepEqualPairDepth takes BOTH values, its doc
	// comment carries the correction in full, and the call below passes both.
	if existing, present := parentData.Get(a.resultKey); present {
		// The guard sits INSIDE the present branch because that is exactly when
		// reflect.DeepEqual runs. Running it unconditionally refused values that were
		// never going to be compared.
		// 🔴 116-AF6-R2, ACCEPTED RESIDUAL (medium). RE-WORDED, not re-dispositioned:
		// an independent seat was commissioned to attack the original text and refuted
		// it in three directions. Corrected here because this comment is the only copy
		// of the finding that ships.
		//
		// The error-substitution class 116-AF1 named is NARROWED, not closed. Any pair
		// checkDeepEqualPairDepth refuses returns ErrValidation from here, because the
		// depth guard runs BEFORE this function's own comparison and a refusal
		// pre-empts it.
		//
		// WIDER than first recorded — it substitutes for SUCCESS, not just for a
		// sentinel. For a deeply EQUAL pair the contract here is nil, the idempotent
		// re-apply. MEASURED: two distinct-but-equal 5-node rings — reflect.DeepEqual
		// answers true in 79.9 us without difficulty — are refused. It fails a run that
		// would have succeeded, which for THIS site is the crash-resume replay of a
		// completed child: the engine's most normal operation.
		//
		// NO CYCLE IS NEEDED; "notably two same-type cyclic values" understated the
		// class. An ACYCLIC struct{Val int; Next *N} chain reaches it. MEASURED
		// boundary, monotone, with the guard NAMED at each step:
		//
		//	links   walk frames (2n+1)   outcome
		//	16,383  32,767              ACCEPT -> falls through to the DeepEqual below
		//	                            -> ErrSubWorkflowResultKeyCollision (correct)
		//	16,384  32,769              REFUSE -> ErrValidation, checkDeepEqualPairDepth
		//
		// A link costs a POINTER frame and a STRUCT frame, so n links cost 2n+1 walk
		// frames and the accept edge is 16,383 — NOT "16,384 x 2 = 32,768", which is the
		// depth-vs-frames slip 116-AF5 was filed for.
		//
		// NARROWER than first recorded — exposure is InMemoryStore-ONLY, and the reason
		// runs through `result`, NOT through `existing`. 116-GC-F5: this comment had the
		// operands the wrong way round, and the fan-out site had the same inversion.
		//
		// `result` is the STORE-DERIVED operand: applyResult's childData arrives from
		// store.Load(childID) on both call paths, so on JSONFile / FlatBuffers / SQLite
		// `result` lands FLATTENED — map[string]interface{} or string. `existing` comes
		// from parentData.Get, and WorkflowData.Get/Set are in-memory map operations with
		// no store involvement: on a fresh single-process run it is the raw Go value a
		// prior node Set, on EVERY backend. So the pair TYPE-MISMATCHES,
		// deepEqualSettlesWithoutRecursing accepts it, and the correct collision sentinel
		// comes back. On a RESUME both operands are store-derived, both flattened and
		// therefore shallow, and the acyclic-side accept fires instead. Either way a
		// marshalling backend cannot reach this bound.
		//
		// Nor can an encoder-VISIBLE value get there through one: checkJSONDepth caps a
		// document at 10,000 NESTING LEVELS, about 20,000 walk frames, and this guard
		// does not refuse until past 32,768. Both quoted in FRAMES on purpose —
		// comparing "10,000 levels" against "16,384 links" would be the AF5 unit slip,
		// two paragraphs after the one warning about it.
		//
		// InMemoryStore is a supported public backend, so the residual is REAL — its
		// blast radius is one backend.
		//
		// EXPOSURE IS AT Execute, NOT AT Save — confirmed independently. The comparison
		// runs during the run, so "a cyclic value could never persist" is TRUE and about
		// the WRONG AXIS. It is also false in its own right; see 116-AF9 in
		// value_depth_deepequal.go. And "on in-memory branch results", the phrase the
		// original finding used, is wrong at BOTH sites, not just this one: the child
		// result arrives via store.Load(childID) here, and fan-out's driveBranch reads
		// its branch result from store.Load(childID) on both of ITS return paths. The
		// re-worded finding called that phrase accurate for fan-out; it is not
		// (116-GC-F1). The operand that is genuinely in-memory is `existing`, on a
		// non-resume run.
		//
		// The remedy — a bounded lockstep probe to decide cheap-disqualification
		// during recursion — was DECLINED by the architect: ~60 lines of equality
		// re-implementation inside a bound, on the guard already carrying the most
		// mirroring complexity, to convert a safe refusal into a less safe accept.
		if derr := checkDeepEqualPairDepth(existing, result, fmt.Sprintf("sub-workflow %q result key %q", a.nodeName, a.resultKey)); derr != nil {
			return derr
		}
		if !reflect.DeepEqual(existing, result) {
			return fmt.Errorf("%w: key %q (node %q)", ErrSubWorkflowResultKeyCollision, a.resultKey, a.nodeName)
		}
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
func childRunFailed(dag *DAG, childData *WorkflowData) (failed bool, firstFailed string, err error) {
	// M23 SEAL-06 — VERDICT MEDIATION. This is the second of the phase's two checks, and it
	// is NOT redundant with the one in (*DAG).Execute: the ph92 parked path renders a run's
	// verdict by READING a child DAG WITHOUT EVER EXECUTING IT, so no execution-path check
	// can see it. (F-117-ARCH-01.)
	//
	// SCOPED TO THE PARKED PATH, matching the doc comment above. An earlier version of THIS
	// comment contradicted that doc comment three lines up: it claimed one check here also
	// closed the ph94 queue path "because the queue action delegates to the parked action".
	// The delegation is real — queueSubWorkflowAction.Execute hands its factory DAG to a
	// parkedSubWorkflowAction — but it does NOT arrive here. That action ENQUEUES the child
	// before it parks, and the parked action consults the work_queue row FIRST; every arm of
	// that queue-authority switch returns ABOVE the childRunFailed call site. On the queue
	// path the verdict therefore comes from the ROW, and this function is UNREACHABLE.
	// Verified at this head: childRunFailed has exactly ONE non-test call site
	// (subworkflow_parked.go), sited below that switch, and no production statement deletes a
	// work_queue row (every production verb on the table is INSERT/SELECT/UPDATE), so the row
	// is still there on every later wake.
	//
	// THE CHECK STAYS ANYWAY — IT IS A DELIBERATE BACKSTOP, NOT DEAD CODE. Its unreachability
	// rests entirely on that "no production DELETE" fact, which is a property of today's code
	// and not a design invariant. Add a work_queue GC — an entirely reasonable future change —
	// and queueTerminalState starts returning exists=false, the queue path falls through to
	// the journal gate below it, and this function becomes reachable holding a child DAG that
	// came straight from a consumer factory. Do NOT delete this check on the grounds that the
	// queue path cannot reach it: that reasoning expires the day the row stops being permanent.
	//
	// The exposure is not academic. The verdict is rendered by reading continueOnError off
	// this DAG, so an unvalidated graph flips a FAILED CHILD INTO A REPORTED SUCCESS — the
	// phase's own defect shape, on the verdict path instead of the execution path.
	//
	// This also SUBSUMES A LATENT CRASH. dag==nil used to reach `dag.nodes[name]` and
	// panic on a level worker goroutine, where a consumer cannot recover — previously held
	// off only by a guard at Build. It is now a typed error on the same branch.
	//
	// SCOPE, stated so the seal is not overclaimed: this proves the verdict DAG passed
	// build(). It does NOT prove the verdict DAG is the same graph that ran — for a queue
	// child those are two objects from two separate consumer factory() calls in two
	// processes. That gap is F-117-ARCH-05, it is pre-existing, and T6 neither causes nor
	// closes it.
	if dag == nil || !dag.built {
		return false, "", fmt.Errorf("%w: cannot render a sub-workflow verdict", ErrDAGNotBuilt)
	}

	consider := func(name string) {
		if firstFailed == "" || name < firstFailed {
			failed = true
			firstFailed = name
		}
	}
	for name, st := range childData.GetAllNodeStatuses() {
		switch st {
		case Failed:
			if node, ok := dag.nodes[name]; ok && node.continueOnError {
				continue // a coe Failed node is tolerated — not a run failure
			}
			consider(name)
		case Compensated, CompensationFailed:
			consider(name) // a rollback node → the run failed (rollback implies failure)
		}
	}
	return failed, firstFailed, nil
}
