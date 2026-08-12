package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// NodeBuilder provides a fluent API for configuring workflow nodes.
// It is part of the builder pattern for creating workflows.
type NodeBuilder struct {
	name            string
	action          Action
	actionErr       error // deferred build error from a builder method (e.g. WithResult/WithInput misuse); surfaced by Build
	dependencies    []string
	retryCount      int
	timeout         time.Duration
	continueOnError bool
	compensation    Action // M12 saga: optional compensating action (WithCompensation)
	workflow        *WorkflowBuilder
}

// WorkflowBuilder provides a fluent API for creating workflow definitions.
// It simplifies the process of defining workflows with dependencies between nodes.
type WorkflowBuilder struct {
	nodes           []*NodeBuilder
	startNodes      []string
	workflowID      string
	store           WorkflowStore
	executionConfig *ExecutionConfig     // nil => DAG uses DefaultConfig()
	tracerProvider  trace.TracerProvider // nil => tracing off
	clock           Clock                // nil => system clock (M10 durable timers)
	choiceEdges     []choiceEdge         // ChoiceNode branch edges, folded in Build (M11)
	mergeEdges      []mergeEdge          // MergeNode join edges, folded in Build (M11 ph42)
	boundaries      []boundaryDecl       // declared (D,V,S) boundaries, folded in Build (M23 VB-01)
	budget          DefinitionBudget     // optional size caps enforced at Build (AUD-068); zero value = no limits
}

// NewWorkflowBuilder creates a new workflow builder.
func NewWorkflowBuilder() *WorkflowBuilder {
	return &WorkflowBuilder{
		nodes:      make([]*NodeBuilder, 0),
		startNodes: make([]string, 0),
		workflowID: fmt.Sprintf("workflow-%d", time.Now().UnixNano()),
	}
}

// WithWorkflowID sets the workflow ID.
// Returns the builder for method chaining.
func (b *WorkflowBuilder) WithWorkflowID(id string) *WorkflowBuilder {
	b.workflowID = id
	return b
}

// WithStore sets the workflow store for persisting workflow state.
// Returns the builder for method chaining.
//
// The store engages only via FromBuilder (which returns a store-backed *Workflow).
// A bare Build() returns a *DAG that cannot carry a store, so since M14/REM-04
// Build() REJECTS a store-configured builder rather than silently running non-durable.
func (b *WorkflowBuilder) WithStore(store WorkflowStore) *WorkflowBuilder {
	b.store = store
	return b
}

// WithExecutionConfig sets the execution configuration (e.g. per-level
// concurrency) applied to the DAG produced by Build.
// Returns the builder for method chaining.
func (b *WorkflowBuilder) WithExecutionConfig(config ExecutionConfig) *WorkflowBuilder {
	b.executionConfig = &config
	return b
}

// DefinitionBudget bounds the SIZE of a workflow definition, enforced once at
// Build (AUD-068). Before this, a definition could grow without limit — a
// runaway fan-out static expansion, a generated graph, or a config bug could
// build an arbitrarily large DAG that only reveals its cost at execution. A
// budget lets a host declare an explicit ceiling and get a typed ErrValidation
// at Build instead.
//
// Each field is an INDEPENDENT cap; a zero (or negative) field means "no limit
// on that axis". The zero value therefore imposes NO limits, so the budget is
// fully opt-in and backward-compatible — a builder with no WithDefinitionBudget
// behaves exactly as before.
type DefinitionBudget struct {
	MaxNodes int // reject if the graph has more than this many nodes (0 = unlimited)
	MaxEdges int // reject if the graph has more than this many dependency edges, generated choice/merge edges included (0 = unlimited)
	MaxWidth int // reject if any level's static width (count of concurrently-runnable nodes) exceeds this (0 = unlimited)
}

// WithDefinitionBudget sets an explicit size ceiling on the definition, checked
// at Build (AUD-068). A graph exceeding any set cap is rejected with a typed
// ErrValidation naming the axis, the actual count, and the ceiling. The zero
// value of any field disables that axis, so passing a zero-value budget is a
// no-op. Returns the builder for method chaining.
func (b *WorkflowBuilder) WithDefinitionBudget(budget DefinitionBudget) *WorkflowBuilder {
	b.budget = budget
	return b
}

// WithTracerProvider sets the OpenTelemetry trace provider used to emit a span
// per executed node (with a parent span per workflow run) on the DAG produced
// by Build. Passing nil disables tracing (the default). API-only: the host
// owns the SDK/exporter (DEC-CHUNK5).
// Returns the builder for method chaining.
func (b *WorkflowBuilder) WithTracerProvider(tp trace.TracerProvider) *WorkflowBuilder {
	b.tracerProvider = tp
	return b
}

// WithClock sets the clock used for durable timers on the workflow produced by
// FromBuilder (M10). Passing nil keeps the default system clock. Tests inject a
// FakeClock to drive durable-time scenarios deterministically.
// Returns the builder for method chaining.
func (b *WorkflowBuilder) WithClock(c Clock) *WorkflowBuilder {
	b.clock = c
	return b
}

// AddTimer adds a declared durable TimerNode (M10 chunk 2): when reached it sleeps
// until clock.Now()+d (an absolute due-time frozen at the first encounter and
// persisted in the checkpoint), parking the run (Waiting) so the process can exit;
// on resume or a host Tick once the due-time has passed it fires and the run
// converges. The timer is durable DATA, not a live time.Timer — it survives
// crash/suspend and an overdue timer fires immediately on resume. Returns a
// NodeBuilder for dependency wiring (DependsOn). The timer action is set directly,
// so do NOT also call WithAction on the returned builder (that would replace the
// timer); retry/timeout are not meaningful on a timer (a park bypasses both).
func (b *WorkflowBuilder) AddTimer(name string, d time.Duration) *NodeBuilder {
	node := b.AddNode(name)
	node.action = &timerAction{nodeName: name, duration: d}
	return node
}

// AddWaitForSignal adds a declared WaitForSignalNode (M10 ph37): when reached it
// parks the run (Waiting) until a signal named signalName is delivered to the
// workflow's durable mailbox (via DeliverSignal / DeliverAndResume), then applies
// the payload idempotently and converges. Requires a Store implementing
// SignalStore. Returns a NodeBuilder for dependency wiring; the action is set
// directly, so do NOT also call WithAction (that would replace it) — retry/timeout
// are not meaningful on a park.
func (b *WorkflowBuilder) AddWaitForSignal(name, signalName string) *NodeBuilder {
	node := b.AddNode(name)
	node.action = &waitForSignalAction{nodeName: name, signalName: signalName}
	return node
}

// AddWaitForSignalTimeout adds a declared first-of(signal, timer) node (M22 ph113):
// when reached it parks the run (Waiting) until EITHER the named signal is delivered to
// the durable mailbox OR the timeout deadline passes — exactly one wins. The deadline is
// an ABSOLUTE instant frozen at first encounter, so it is durable-remaining across a
// crash/restart (not reset). On a same-encounter tie (signal present AND deadline
// passed) the SIGNAL wins. The timeout arm sets a disposition key (timedOutKey(name))
// so a downstream M11 ChoiceNode can branch signal-vs-timeout into separate subgraphs.
// Requires a Store implementing SignalStore (else ErrWaitRequiresSignalStore at run
// time). Returns a NodeBuilder for dependency wiring; the action is set directly, so do
// NOT also call WithAction (that would replace it and hide the park). This is a wholly
// separate mechanism from the non-durable WithTimeout (an exec-deadline on an ordinary
// node) — a park bypasses WithTimeout entirely.
func (b *WorkflowBuilder) AddWaitForSignalTimeout(name, signalName string, timeout time.Duration) *NodeBuilder {
	node := b.AddNode(name)
	node.action = &waitForSignalOrTimeoutAction{nodeName: name, signalName: signalName, duration: timeout}
	return node
}

// requireBuiltChild is the M23 SEAL-06 admission check for a caller-supplied child DAG
// (R-04). AddSubWorkflow and AddSubWorkflowParked both take a *DAG straight from the
// consumer into the sanctioned builder API, and before this they checked only nil and
// (inline) suspendability — the child's PROVENANCE was assumed rather than checked.
//
// It costs nothing and it converts an argument into a check. The nil arm keeps the exact
// wording the inline path already used, because a test pins that string and, more to the
// point, the two conditions want the same remedy from the caller: build the child.
func requireBuiltChild(child *DAG) error {
	if child == nil {
		return fmt.Errorf("%w: sub-workflow child DAG is nil", ErrValidation)
	}
	if !child.built {
		return fmt.Errorf("%w: sub-workflow child DAG %q", ErrDAGNotBuilt, child.name)
	}
	return nil
}

// AddSubWorkflow adds a declared sub-workflow node (M19 ph91): when reached it spawns and
// awaits the definition-value child DAG IN-PROCESS under a deterministic child ID
// (f(parentID, name)) with the child's own journal — parent and child are DISTINCT
// workflows (one-writer preserved). Child success writes the child's result (see
// WithResult) into parent data; child failure fails this node (INV-01 fail-fast). The
// spawn is idempotent (a re-drive after the child completed does not re-run it). Requires
// the run to have a Store (else the node returns ErrSubWorkflowRequiresStore).
//
// The child's whole spawn-closure is scanned AT BUILD for any suspendable node (an inline
// child BLOCKS the parent, so it can never park): a suspendable node anywhere in the
// closure fails Build with ErrSubWorkflowSuspendableChild — INLINE is the only path that
// refuses one. Route such a child to AddSubWorkflowParked (the host runs it; needs a
// SignalStore) or AddSubWorkflowQueued (the engine dispatches it; needs a multi-process
// *SQLiteStore + Pool + Registry) — choose on the STORE, not on capability. The action is
// set directly, so do NOT also call WithAction.
// Returns a NodeBuilder for dependency wiring and result declaration (WithResult).
func (b *WorkflowBuilder) AddSubWorkflow(name string, child *DAG) *NodeBuilder {
	node := b.AddNode(name)
	node.action = &subWorkflowAction{nodeName: name, child: child}
	// M23 SEAL-06 (R-04) — the child is CALLER-SUPPLIED and enters through the sanctioned
	// builder, so assert its provenance HERE rather than arguing that it must be fine.
	// Refusing at the parent's build() is the earliest possible failure and the clearest
	// message; the token check at the child's own drive is the backstop, not the diagnosis.
	if err := requireBuiltChild(child); err != nil {
		node.actionErr = err
	} else if err := scanChildInlineSafe(child); err != nil {
		// Scan the child's spawn-closure NOW; a suspendable node (direct or transitive) is a
		// build error surfaced through actionErr (Build reports it — builder.go Build()).
		node.actionErr = err
	}
	return node
}

// WithResult declares that this sub-workflow node's result is the child's DATA KEY
// childDataKey, written into parent data under parentKey on child success (M19 ph91). The
// child must Set(childDataKey, ...) its result; a data key (not a node output) is read
// because data keys carry the store's typed columns, so a SCALAR result (int64 via value_long,
// plus string/bool/float) round-trips type-faithfully on all three stores (an int64 reloads
// as an int64), whereas node outputs reload as strings on FB/SQLite. A COMPLEX result
// (map/slice/nil) is NOT backend-uniform — it reloads typed on InMemory but as a JSON string
// on FB/SQLite (the same pre-existing store-wide property that governs every complex data
// value); declare a scalar result key when backend-uniformity matters. A collision with a
// pre-existing parent key (foreign value) is a loud ErrSubWorkflowResultKeyCollision at run
// time, not last-writer-wins. Only valid on a node created by AddSubWorkflow; a no-op (with a
// deferred error) otherwise.
func (n *NodeBuilder) WithResult(parentKey, childDataKey string) *NodeBuilder {
	switch a := n.action.(type) {
	case *subWorkflowAction:
		a.resultKey = parentKey
		a.resultFrom = childDataKey
	case *parkedSubWorkflowAction:
		a.resultKey = parentKey
		a.resultFrom = childDataKey
	case *queueSubWorkflowAction:
		a.resultKey = parentKey
		a.resultFrom = childDataKey
	default:
		n.actionErr = fmt.Errorf("%w: WithResult is only valid on an AddSubWorkflow/AddSubWorkflowParked/AddSubWorkflowQueued node", ErrValidation)
	}
	return n
}

// WithInput sets the seeded KV input for a QUEUE-dispatched sub-workflow child (M19 ph94): the map is
// JSON-encoded into the work_queue row's input, and RunNext's seedInput sets each key as a child data
// key on the fresh run (so the child's first nodes read it). Only valid on an AddSubWorkflowQueued node.
// A nil/empty map is a no-op (no seed).
//
// NO CHILD READS PARENT DATA, on any path. Every child runs under its own WorkflowID with its own journal
// (AddSubWorkflow: "parent and child are DISTINCT workflows"); an INLINE child's Execute builds a FRESH
// WorkflowData for the child ID and loads only that child's own persisted state. Parent and child
// WorkflowData are DISJOINT: nothing is copied in. The QUEUE path is the only one with an input
// mechanism, and this method is it.
//
// WithResult moves exactly ONE value INTO PARENT DATA, on every path: a single (parentKey, childDataKey)
// pair, NOT additive — a second call OVERWRITES the first. A child that Sets three data keys still lands
// one in the parent. That bounds the PARENT-DATA channel only; it does not bound what the CALLER can see
// (for parked the host owns the child's run and its store, so it can read the rest back — see
// AddSubWorkflowParked).
//
// To parameterize an INLINE child, CAPTURE THE VALUES IN ITS ACTIONS' CLOSURES AT DAG-CONSTRUCTION TIME —
// build the child *DAG from a Go function taking the parameters, and let its actions close over them:
//
//	func reviewChild(applicant string) (*DAG, error) {
//		cb := NewWorkflowBuilder()
//		cb.AddStartNode("review").WithAction(ActionFunc(func(_ context.Context, d *WorkflowData) error {
//			d.Set("verdict", "reviewed:"+applicant) // `applicant` is CAPTURED, not read from parent data
//			return nil
//		}))
//		return cb.Build()
//	}
//
//	child, err := reviewChild("acme")
//	if err != nil {
//		return err
//	}
//	pb.AddSubWorkflow("review", child).WithResult("verdict", "verdict")
//
// The cost is that an INLINE child *DAG is a VALUE, not a template: a different parameterization needs a
// different child DAG, and an out-capture is bound to that ONE build-time DAG, shared by every run. Where
// one child definition must serve many runtime-varying inputs on the inline path, that is what the QUEUE
// path (AddSubWorkflowQueued + WithInput) is for — it takes a child TYPE and seeds the data keys per run.
//
// PARKED PARAMETERIZES DIFFERENTLY and does not need this method: AddSubWorkflowParked never runs the
// child, so the HOST builds a per-run child (under SubWorkflowChildID) and ONE parent definition serves
// many runtime inputs. Do NOT read "parked is the lighter path" from that — parked requires a SignalStore
// where an inline child runs on a bare WorkflowStore. See AddSubWorkflowParked and
// docs/guides/sub-workflows.md for the full inline-vs-parked divergence map.
func (n *NodeBuilder) WithInput(kv map[string]any) *NodeBuilder {
	sub, ok := n.action.(*queueSubWorkflowAction)
	if !ok {
		n.actionErr = fmt.Errorf("%w: WithInput is only valid on an AddSubWorkflowQueued node", ErrValidation)
		return n
	}
	if len(kv) == 0 {
		return n
	}
	// AF2: the CRASH axis, BEFORE the marshal — the depth cap below measures bytes the
	// encoder only produces if it survived the value. Build time is where this belongs
	// for the same reason the byte cap is: the value is refused before it is ever durable.
	if derr := checkValueDepth(kv, "sub-workflow input"); derr != nil {
		n.actionErr = derr
		return n
	}
	b, err := json.Marshal(kv)
	if err != nil {
		n.actionErr = fmt.Errorf("%w: cannot encode sub-workflow input: %w", ErrValidation, err)
		return n
	}
	// Depth cap at the write, same reason as every other marshal site: this input is
	// carried into the work_queue and read back by a decoder that HAS a cap. Refusing
	// here is a build-time validation error; accepting it produces a durable row whose
	// reader will refuse it, which is a wedge rather than a rejection. Build time is the
	// earliest and cheapest place this can be caught.
	if derr := checkJSONDepth(b, "sub-workflow input"); derr != nil {
		n.actionErr = derr
		return n
	}
	sub.input = b
	return n
}

// AddSubWorkflowParked adds a declared PARKED sub-workflow-await node (M19 ph92): when reached
// it PARKS the run (Waiting) while the child — run OUT-OF-BAND under its deterministic ID
// f(parentID, name) — is not yet terminal; a durable completion signal delivered to the
// workflow's mailbox (SubWorkflowCompletionSignal) + a host DeliverAndResume wakes it; on wake
// it reads the child's declared result DATA key (see WithResult — the uniform ph91 contract, NOT
// the signal payload) and converges, or fails this node if the child terminalized failed
// (INV-01, coe-aware). Requires a Store implementing SignalStore (else ErrWaitRequiresSignalStore).
//
// This is the PARKED counterpart to AddSubWorkflow (which BLOCKS inline). The action is set directly
// (marker visible), so do NOT also call WithAction. The ROUTING between inline and parked is ph94; ph92
// provides the parked mechanism. A nil child fails at Build (it would otherwise kill the host process
// when the verdict is rendered).
//
// THE child YOU PASS IS NEVER EXECUTED — it is a VERDICT CLASSIFIER, not a template. The HOST runs the
// child, under SubWorkflowChildID(parentID, name); this node only classifies the host's FINISHED run from
// the journal. The classifier must declare, as ContinueOnError, EVERY node name the host's run may leave
// in status Failed and expect tolerated. NOTHING ELSE about it is read — not its edges, not its actions
// (a classifier node's action is never invoked), not its node count, not extra nodes the host never ran,
// not nodes that succeed. A Compensated/CompensationFailed node is ALWAYS a failure and the classifier is
// not consulted. A one-node stub naming the single coe-failable node correctly classifies a larger run.
//
// The failure mode is a FALSE FAILURE, and it is silent: a node the host TOLERATED whose name is ABSENT
// from the classifier — or PRESENT but not marked ContinueOnError — makes this node fail a run the host
// considered successful.
//
// PARAMETERIZE PER RUN: because the host runs the child, it builds a fresh child DAG per run with per-run
// closure captures, and ONE parent definition serves many runtime inputs (the inline path cannot — its
// child is one build-time value shared by every run). The host can also reach the child's FULL result
// set — it owns the run and the store, so store.Load(childID) or its own closure capture gets the rest
// (Workflow.Execute returns only an error; the run's WorkflowData is not exposed). So WithResult's
// single-value limit bounds only what reaches PARENT data.
//
// THE HOST MUST RUN THE CHILD ON THE SAME STORE AS THE PARENT — this node reads the child journal through
// the PARENT's store, so a child run on a different store is invisible: this node reads ErrNotFound and
// returns ErrSuspended, so the parent RE-PARKS ON EVERY WAKE — no error, no timeout, and no number of
// re-drives converges it.
//
// DIVERGENCES FROM THE INLINE PATH (AddSubWorkflow), none of them symmetric:
//   - SUSPENDABLE CHILD: inline REFUSES at Build (ErrSubWorkflowSuspendableChild); parked ACCEPTS one —
//     the host may park AND resume the child, then wake this parent.
//   - STORE: parked REQUIRES a SignalStore; inline runs on a bare WorkflowStore. Parked is NOT uniformly
//     the lighter path (it IS lighter than the queue path, which needs *SQLiteStore + Pool + Registry).
//   - GUARDS: the nesting-depth ceiling, the ancestor-cycle guard and the build-time closure scan are
//     INLINE-ONLY. The parked action never drives, so depth and cycles are the HOST's responsibility.
//   - FAILURE FIDELITY (the sharpest): inline propagates the child's ACTUAL error value, so errors.Is
//     reaches the child's sentinel. Parked RECONSTRUCTS the verdict from node statuses — the error value
//     is LOST and only the offending node NAME survives. A consumer classifying child failures with
//     errors.Is/errors.As gets a true positive on inline and a SILENT FALSE NEGATIVE on parked.
func (b *WorkflowBuilder) AddSubWorkflowParked(name string, child *DAG) *NodeBuilder {
	node := b.AddNode(name)
	node.action = &parkedSubWorkflowAction{nodeName: name, child: child}
	// A nil child is LATENT-THEN-FATAL: Build would accept it, an all-success host run
	// converges (childRunFailed only reads dag.Nodes on a Failed node), and the first
	// child run that leaves any node Failed derefs the nil ON A LEVEL WORKER GOROUTINE
	// — where a caller's recover() cannot reach it, so the HOST PROCESS dies. Refuse at
	// Build, in the same words the inline sibling uses (scanChildInlineSafe). Parked does
	// NOT get the inline suspendable-scan: a suspendable child is legitimate here.
	// (F-PARK-03.)
	// M23 SEAL-06 (R-04) folds the nil check in: requireBuiltChild refuses nil with the
	// same wording, and additionally refuses a non-nil child that never passed build().
	// That second half matters more on THIS path than on the inline one, because the
	// parked child is never executed — it is read to render the run's VERDICT, so an
	// unvalidated graph here turns a failed child into a reported success.
	if err := requireBuiltChild(child); err != nil {
		node.actionErr = err
	}
	return node
}

// AddSubWorkflowQueued adds a QUEUE-dispatched sub-workflow node (M19 ph94): when reached it ENQUEUES
// a child of TYPE childType to the M17 work_queue (carrying this parent's mailbox address in the
// trusted control columns) and PARKS (Waiting); a pool worker claims + runs the child; on child-terminal
// a completion signal wakes this parent, which reads the child's result DATA key (WithResult) + renders
// the coe-aware verdict. This is the queue counterpart to AddSubWorkflow (inline, ph91): the opt-in for a
// TYPE-REF child, and ONE of the two onward routes for a SUSPENDABLE child — the INLINE path refuses one
// at Build (ErrSubWorkflowSuspendableChild), but AddSubWorkflowParked ACCEPTS one (the host runs it, and
// may park AND resume it, then wake this parent). Choose between the two on the STORE, not on capability:
// parked needs only a SignalStore. (Parked is not uniformly the lighter choice though — it requires a
// SignalStore where an INLINE child runs on a bare WorkflowStore.) This path structurally
// requires a multi-process *SQLiteStore + a worker Pool + a Registry (the type→DAG map, injected at
// Execute — the DAG carries only the type STRING, keeping the workflow pure DATA). The action is set
// directly (marker visible), so do NOT also call WithAction. Returns a NodeBuilder for dependency wiring
// + WithResult. WithInput sets the child's seeded KV input.
func (b *WorkflowBuilder) AddSubWorkflowQueued(name, childType string) *NodeBuilder {
	node := b.AddNode(name)
	node.action = &queueSubWorkflowAction{nodeName: name, childType: childType}
	if childType == "" {
		node.actionErr = fmt.Errorf("%w: AddSubWorkflowQueued requires a non-empty child type", ErrValidation)
	}
	return node
}

// AddApproval adds a declared approval node (M19 ph90): when reached it parks the
// run (Waiting) until an approve/reject decision (an ApprovalDecision payload) is
// delivered to the workflow's durable mailbox under the SIGNAL NAME EQUAL TO THE NODE
// NAME, then acts: approve → apply the decision (persisted to the journal for audit)
// and converge; reject → fail fast with an *ApprovalRejectedError (INV-01, no
// downstream runs). Requires a Store implementing SignalStore (else the node returns
// ErrWaitRequiresSignalStore — a loud failure, never a forever-park). A host builds
// the decision Signal with ApproveSignal / RejectSignal (which derive the same name).
// Returns a NodeBuilder for dependency wiring; the action is set directly, so do NOT
// also call WithAction (that would replace it) — retry/timeout are not meaningful on a
// park.
//
// AUTHORIZATION SCOPE (AUD-069 / S-02): AddApproval is an ORCHESTRATION primitive — a
// durable decision gate — NOT an authentication or authorization protocol. The engine
// does NOT verify WHO approved: the ApprovalDecision.Approver is a HOST-ASSERTED string
// carried only for the audit trail, with no freshness, principal-identity, request
// correlation/nonce, policy-version, or cryptographic/host-authenticated provenance. Any
// decision delivered to the mailbox under this node's name is accepted as-is (a
// misdirected or forged one included). Authenticating the approver, correlating the
// request to a specific pending approval, and preventing replay are the HOST's
// responsibility: deliver an already-authorized decision. (Host-endorsement provenance
// is planned but not yet provided — do not rely on the engine for it.)
//
// FOR A BOUNDED DECISION, SEE AddWaitForSignalTimeout: an approval park has no deadline and
// waits indefinitely. AddWaitForSignalTimeout arms an ABSOLUTE due instant at FIRST encounter
// and never re-arms it (signal_timeout.go:95-110), so the deadline is durable-remaining across
// a restart and a crash-looping run cannot hold the park open indefinitely; its timeout arm
// sets timedOutKey(name) (signal_timeout.go:40) so a downstream M11 ChoiceNode can branch into
// an explicit timeout subgraph. The cost of switching is this node's decision vocabulary: the
// ApproveSignal/RejectSignal constructors and the *ApprovalRejectedError classification are
// specific to AddApproval — a timeout-bounded wait carries a plain signal payload and a
// disposition key instead.
func (b *WorkflowBuilder) AddApproval(name string) *NodeBuilder {
	node := b.AddNode(name)
	node.action = &approvalAction{nodeName: name, signalName: name}
	if name == "" {
		// An approval node's decision signal name is derived 1:1 from its node name
		// (so ApproveSignal/RejectSignal cannot drift). A bare "" name is a silent
		// footgun — the node builds but can never be satisfied (no host can target
		// the empty decision signal). Fail loud + actionable at Build instead.
		node.actionErr = fmt.Errorf("%w: AddApproval requires a non-empty name (it IS the decision signal name — deliver the decision with ApproveSignal(name)/RejectSignal(name))", ErrValidation)
	}
	return node
}

// AddWaitForCondition adds a declared WaitForConditionNode (M10 ph37, "await"):
// when reached it parks the run while predicate(data) is false, re-evaluating on
// each wake (a host re-drive), and converges when it flips. Returns a NodeBuilder
// for dependency wiring; the action is set directly, so do NOT also call WithAction.
func (b *WorkflowBuilder) AddWaitForCondition(name string, predicate func(*WorkflowData) bool) *NodeBuilder {
	node := b.AddNode(name)
	node.action = &waitForConditionAction{predicate: predicate}
	return node
}

// AddNode adds a regular node to the workflow and returns a NodeBuilder for
// further configuration.
func (b *WorkflowBuilder) AddNode(name string) *NodeBuilder {
	node := &NodeBuilder{
		name:         name,
		dependencies: make([]string, 0),
		workflow:     b,
	}
	b.nodes = append(b.nodes, node)
	return node
}

// AddStartNode adds a starting node (no dependencies) to the workflow and
// returns a NodeBuilder for further configuration.
func (b *WorkflowBuilder) AddStartNode(name string) *NodeBuilder {
	node := b.AddNode(name)
	b.startNodes = append(b.startNodes, name)
	return node
}

// WithAction sets the action for the node.
// The action can be an Action interface or a function with the signature
// func(ctx context.Context, data *WorkflowData) error.
// Returns the builder for method chaining.
func (n *NodeBuilder) WithAction(action Action) *NodeBuilder {
	// AUD-041: typed. The action IS an Action — the compiler rejects a mistyped value
	// at the call site instead of the old interface{}+runtime-`default` rejection at
	// Build (which could only report "unsupported action type: %T" after the fact). To
	// supply a bare function with the Action signature, use WithActionFunc. A nil Action
	// falls through to Build's "no action defined" guard, unchanged.
	n.action = action
	return n
}

// WithActionFunc sets the node's action from a bare function with the Action signature —
// the ergonomic form for an inline closure, equivalent to WithAction(ActionFunc(fn)).
// (AUD-041: the typed counterpart to WithAction, mirroring http.Handler / http.HandlerFunc.)
func (n *NodeBuilder) WithActionFunc(fn func(ctx context.Context, data *WorkflowData) error) *NodeBuilder {
	n.action = ActionFunc(fn)
	return n
}

// DependsOn specifies dependencies for this node by name.
// Returns the builder for method chaining.
func (n *NodeBuilder) DependsOn(deps ...string) *NodeBuilder {
	n.dependencies = append(n.dependencies, deps...)
	return n
}

// WithRetries sets the number of retries for the node.
// Returns the builder for method chaining.
func (n *NodeBuilder) WithRetries(count int) *NodeBuilder {
	n.retryCount = count
	return n
}

// WithTimeout sets a timeout for the node execution.
// Returns the builder for method chaining.
func (n *NodeBuilder) WithTimeout(timeout time.Duration) *NodeBuilder {
	n.timeout = timeout
	return n
}

// WithContinueOnError marks the node so that a failure does not fail the
// workflow. The node is recorded as Failed and the rest of the DAG continues;
// dependents may inspect the node's Failed status (via WorkflowData.GetNodeStatus)
// and branch on it. Default (unset) preserves the fail-fast behavior.
// Returns the builder for method chaining.
func (n *NodeBuilder) WithContinueOnError() *NodeBuilder {
	n.continueOnError = true
	return n
}

// WithCompensation sets the compensating action for this node (M12 saga). If the
// workflow fails with a hard error and rolls back, a Completed node's compensation
// is invoked in reverse-topological order under a FRESH context to durably undo its
// effect, and the node is then marked Compensated. Accepts an Action or a
// func(ctx, *WorkflowData) error (the same forms as WithAction). An unsupported
// type is recorded and reported by Build(). A compensation MUST be idempotent — it
// may be re-invoked after a crash mid-rollback (at-least-once); the executor passes
// it a stable IdempotencyKey handle. A node with no compensation is a rollback
// no-op. Returns the builder for method chaining.
func (n *NodeBuilder) WithCompensation(action Action) *NodeBuilder {
	// AUD-041: typed (see WithAction). Use WithCompensationFunc for a bare function.
	n.compensation = action
	return n
}

// WithCompensationFunc sets the node's compensation from a bare function with the Action
// signature, equivalent to WithCompensation(ActionFunc(fn)) (AUD-041).
func (n *NodeBuilder) WithCompensationFunc(fn func(ctx context.Context, data *WorkflowData) error) *NodeBuilder {
	n.compensation = ActionFunc(fn)
	return n
}

// Build creates a DAG from the workflow definition.
// Returns an error if the workflow definition is invalid (e.g., has cycles).
//
// M14 ph62 (REM-04): Build REFUSES when a store was configured via WithStore. A
// bare *DAG does NOT carry a store — DAG.Execute has no persistence — so
// WithStore(s).Build().Execute would run SILENTLY NON-DURABLE, discarding the store
// the caller explicitly set (a silent durability-loss footgun). To build a durable,
// store-backed run, use FromBuilder (returns a *Workflow whose Execute uses the
// store) or construct a *Workflow directly. This guard turns the silent lie into a
// loud, self-documenting error; the store-less Build() path is unchanged.
func (b *WorkflowBuilder) Build() (*DAG, error) {
	if b.store != nil {
		return nil, fmt.Errorf(
			"%w: WithStore configures a durable Workflow, but Build returns a bare DAG that cannot carry a store (Execute would silently run non-durable) — build with FromBuilder(b) to get a store-backed *Workflow instead",
			ErrValidation,
		)
	}
	return b.build()
}

// build is the guard-free DAG construction, used by Build (after the store guard)
// and by FromBuilder (which DOES carry the store forward onto the *Workflow, so the
// store is not lost — the guard would be wrong there). (M14 ph62 REM-04.)
func (b *WorkflowBuilder) build() (*DAG, error) {
	// Create a new DAG with capacity hints based on the number of nodes
	nodeCount := len(b.nodes)
	dag := newDAGWithCapacity(b.workflowID, nodeCount)

	// Apply a custom execution config if one was provided; otherwise the DAG
	// keeps its DefaultConfig().
	if b.executionConfig != nil {
		dag.WithExecutionConfig(*b.executionConfig)
	}

	// Apply a tracer provider if one was set. Done after WithExecutionConfig so
	// it survives a custom config (which would otherwise reset the field) — the
	// builder's WithTracerProvider is the source of truth for tracing here.
	if b.tracerProvider != nil {
		dag.WithTracerProvider(b.tracerProvider)
	}

	// AUD-012 / C-04: fold ChoiceNode branch edges (choice -> target) and MergeNode join
	// edges (merge -> tail) into a LOCAL per-node effective-dependency map — built from
	// CLONES of each NodeBuilder's declared dependencies — never back into the builders.
	// Folded FIRST so the existing count/wire/cycle-check passes treat generated edges like
	// any other edge, and so the When/Otherwise/From wiring is independent of node-declaration
	// order (a target/tail may be declared before or after the call) (M11). The clone is what
	// makes Build PURE: a builder is reusable (registry factories, tests, and the phase-121
	// digest all rebuild the same builder), and the previous in-place append re-added the same
	// generated edges on every Build, drifting topology (and the definition digest) with Build
	// count. effectiveDeps is now the single authority the passes below read from.
	effectiveDeps := make(map[string][]string, nodeCount)
	for _, nb := range b.nodes {
		effectiveDeps[nb.name] = append([]string(nil), nb.dependencies...)
	}
	if len(b.choiceEdges) > 0 || len(b.mergeEdges) > 0 {
		builderByName := make(map[string]*NodeBuilder, nodeCount)
		for _, nb := range b.nodes {
			builderByName[nb.name] = nb
		}
		for _, e := range b.choiceEdges {
			if _, ok := builderByName[e.target]; !ok {
				return nil, fmt.Errorf("choice %q routes to unknown branch target %q", e.choice, e.target)
			}
			effectiveDeps[e.target] = append(effectiveDeps[e.target], e.choice)
		}
		for _, e := range b.mergeEdges {
			if _, ok := builderByName[e.merge]; !ok {
				return nil, fmt.Errorf("merge %q not found while wiring its tails", e.merge)
			}
			if _, ok := builderByName[e.tail]; !ok {
				return nil, fmt.Errorf("merge %q joins unknown branch tail %q", e.merge, e.tail)
			}
			effectiveDeps[e.merge] = append(effectiveDeps[e.merge], e.tail)
		}
	}

	// Boundary verifier/sink nodes are validated by validateBoundaries below with
	// role-specific reasons (boundaryOpaqueReason covers BOTH the nil-run and the
	// *DAG grounds and names the role). Those supersede the generic per-node action
	// checks here, so a node named as a V/S is skipped in the AUD-001/AUD-011 checks
	// and refused — with the better message — by the boundary layer instead.
	boundaryRoleNode := make(map[string]bool, 2*len(b.boundaries))
	for _, d := range b.boundaries {
		boundaryRoleNode[d.verifier] = true
		boundaryRoleNode[d.sink] = true
	}

	// Map to track node dependency counts for capacity hints
	nodeDependencyCounts := make(map[string]int, nodeCount)

	// First pass: count dependencies per node (from the effective, generated-edge-folded map)
	for _, builder := range b.nodes {
		for _, depName := range effectiveDeps[builder.name] {
			nodeDependencyCounts[depName]++
		}
	}

	// Create real nodes from builders
	for _, builder := range b.nodes {
		if builder.actionErr != nil {
			return nil, fmt.Errorf("node %s has invalid action: %w", builder.name, builder.actionErr)
		}
		if builder.action == nil {
			return nil, fmt.Errorf("node %s has no action defined", builder.name)
		}
		if !boundaryRoleNode[builder.name] {
			// AUD-001 / F118-ENG-06: reject nil / typed-nil / nil-operand built-in
			// actions at Build. Their Execute dereferences a nil operand inside an
			// executor worker goroutine, where no caller's recover() can reach it, so
			// the panic takes the HOST PROCESS down rather than failing the node. The
			// boundary clause already refused these for a boundary's verifier/sink; an
			// ORDINARY node was still exposed. Rejecting here makes the invalid state
			// unrepresentable instead of a run-time crash. (Structural completes-on-
			// clock/structure kinds are NOT touched — they are valid ordinary nodes.)
			if why, unsafe := actionRunSafetyReason(builder.action); unsafe {
				return nil, fmt.Errorf("%w: node %s has an action that cannot run: %s", ErrValidation, builder.name, why)
			}
			// AUD-011 / F118-ENG-01: reject a compiled *DAG smuggled in via WithAction
			// — it bypasses AddSubWorkflow's depth/cycle/suspendable-child guards.
			if err := rejectCompiledDAGAction(builder.name, builder.action); err != nil {
				return nil, err
			}
		}

		// F118-ENG-01 (compensation slot): a compiled *DAG smuggled in via
		// WithCompensation runs under (*DAG).Execute during rollback exactly as an
		// action would, bypassing the same AddSubWorkflow depth/cycle/suspendable-child
		// guards. Checked for every node (not gated on boundary role): compensation is
		// not a boundary concept, and a boundary node may still declare one.
		if builder.compensation != nil {
			if err := rejectCompiledDAGCompensation(builder.name, builder.compensation); err != nil {
				return nil, err
			}
		}

		// Use capacity hints for dependencies (effective = declared + folded generated edges)
		depCapacity := len(effectiveDeps[builder.name])
		node := newNodeWithCapacity(builder.name, builder.action, depCapacity)

		// M23 SEAL-01: the WithRetries/WithTimeout/WithContinueOnError mutators are
		// deleted — they were exported, so they configured a node AFTER build() had
		// validated it. build() owns construction, so it writes the fields directly.

		// CLAMPED, and this is a correctness fix, not tidying (blocker 117-F1).
		// WithRetries does no validation, and the old `if retryCount > 0` guard around
		// the mutator call was silently absorbing negative input. Removing it let -1
		// reach the node, where saga_rollback.go's compensation loop is
		// `for attempt := 0; attempt <= n.retryCount` — UNGUARDED. With -1 the body
		// never runs, lastErr stays nil, that reads as success, and the node is stamped
		// Compensated with markCompensated recorded: the durable journal says an effect
		// was undone that was never touched.
		//
		// Clamped HERE rather than at each reader: this is the only site that writes a
		// CALLER-SUPPLIED value into Node.retryCount, so it is the one boundary where an
		// out-of-range value can enter. Making the invalid state unrepresentable is this
		// phase's thesis; tolerating it at the readers is what produced the blocker, and
		// the reader tally below is the argument for that — count it, do not estimate it.
		//
		// 117-F4: an earlier version of this sentence read "four readers, three of which
		// happen to guard" and contradicted the enumeration twelve lines down IN THE SAME
		// BLOCK — and the A2 edit that fixed a self-contradicting comment left it standing
		// inside the block it was fixing. MEASURED, all six non-test read sites:
		//   (*Node).execute           `if n.retryCount > 0`     — a real guard
		//   (*Node).execute           the read inside that guard
		//   nodeSpanAttributes        `if n.retryCount > 0`     — a real guard
		//   nodeSpanAttributes        the read inside that guard
		//   runCompensationWithRetry  `attempt <= n.retryCount` — THE BLOCKER, its loop
		//     condition, unguarded
		//   runCompensationWithRetry  `attempt < n.retryCount`  — the backoff check in the
		//     SAME function, and NOT a guard: it is merely unreachable on a negative,
		//     because the loop condition above never admits a body. Reading it
		//     as protection is the error that makes the surface look twice as defended as
		//     it is — an accident of control flow is not an invariant.
		// So exactly TWO of six reads carry a real `> 0` guard. The clamp is what the
		// other four rely on.
		//
		// THE WRITER SETS ARE COMPILER-DERIVED — each field renamed, the error list read,
		// never grepped, because grep-over-a-category is the instrument that produced
		// 117-F1. TWO fields had to be renamed, and an earlier version of this comment
		// renamed only the first while asserting a property of the second:
		//   NodeBuilder.retryCount — 2 non-test sites: WithRetries (sole writer),
		//     build() (sole reader). Plus 2 test reads.
		//   Node.retryCount — 3 non-test WRITES, not one: this line, and the
		//     `retryCount: 0` literals in NewNode and NewNodeWithCapacity. Its non-test
		//     READS are enumerated once, above; deliberately not re-tallied here, because
		//     a second tally of the same set in the same block is precisely how 117-F4
		//     happened.
		// So "the sole writer" was false as literally written; the two constructor writes
		// are zero literals and cannot be out of range, which is the property that
		// actually holds and the one stated above.
		//
		// WithBranchRetries (fanout.go) does NOT reach either field — CHECKED, not
		// assumed, since it was the reviewer's open question. It guards its own count
		// with `if count <= 0 { branchRetry = nil; … }`, so its consumer
		// (branchRetryPolicy.count, renamed: 2 non-test sites, both under that guard)
		// only ever sees >= 1. No second clamp is owed on that path.
		node.retryCount = max(0, builder.retryCount)
		node.timeout = builder.timeout
		node.continueOnError = builder.continueOnError
		node.compensation = builder.compensation // M12 saga: nil when no WithCompensation

		// Add node to DAG
		if err := dag.addNode(node); err != nil {
			return nil, fmt.Errorf("failed to add node %s: %w", builder.name, err)
		}
	}

	// Add dependencies (from the effective map, so generated choice/merge edges are wired)
	for _, builder := range b.nodes {
		nodeDeps := effectiveDeps[builder.name]
		if len(nodeDeps) == 0 {
			continue
		}

		node, exists := dag.GetNode(builder.name)
		if !exists {
			return nil, fmt.Errorf("node %s not found", builder.name)
		}

		deps := make([]*Node, 0, len(nodeDeps))

		// Collect all dependencies first
		for _, depName := range nodeDeps {
			depNode, exists := dag.GetNode(depName)
			if !exists {
				return nil, fmt.Errorf("dependency %s for node %s not found",
					depName, builder.name)
			}
			deps = append(deps, depNode)
		}

		// M23 SEAL-01: AddDependencies is deleted (it mutated the edge set post-build).
		node.dependsOn = append(node.dependsOn, deps...)
	}

	// Validate the DAG
	if err := dag.Validate(); err != nil {
		return nil, fmt.Errorf("invalid workflow: %w", err)
	}

	// Strict reconvergence validation (M11 ph42, D-P42-STRICT): only structured,
	// single-Choice, local OR-joins are expressible. Runs after the cycle-check
	// so a rejected graph never reaches the executor, and so the runtime OR-join
	// semantics can rely on "a merge reconverges exactly one Choice".
	if err := validateReconvergence(dag); err != nil {
		return nil, fmt.Errorf("invalid workflow: %w", err)
	}

	// M23 SEAL-06 — the builder token, and it MUST REMAIN THE LAST STATEMENT HERE.
	// validateReconvergence above does not merely check: it APPENDS the DEC-M11-DEPMODEL
	// merge<-choice edges as its final act. Stamping any earlier — in particular right
	// after dag.Validate() — would certify a graph that then gained edges, which is a
	// token that means something subtly other than what it says.
	//
	// This is also the ONLY assignment to dag.built in non-test code, and that is the
	// property the whole mechanism rests on: stamped anywhere else, the token would
	// certify "a DAG was constructed" rather than "build() validated it", the mechanism
	// would be void, and every test would still pass. Guarded by
	// TestSealed_BuiltIsStampedOnlyInBuild.
	//
	// M23 VB-01 — the token now certifies a SECOND thing: that every declared boundary
	// holds over this graph. The seat is here for the same reason the stamp is: a
	// dominance predicate evaluated before validateReconvergence would be evaluated
	// against a graph that then gains the merge<-choice edges.
	// slices.Clone, not the slice itself (118-F5): the builder is reusable and its
	// slice header outlives this call, so an aliased dag.boundaries could be mutated
	// after validation by a further WithBoundary on the same builder -- validated set,
	// changed content, stamped graph. Cloning makes the run-constancy class true by
	// construction rather than by nobody having tried it.
	dag.boundaries = slices.Clone(b.boundaries)
	dag.hasBoundaries = len(b.boundaries) > 0
	if err := validateBoundaries(dag, dag.boundaries); err != nil {
		return nil, fmt.Errorf("invalid workflow: %w", err)
	}

	// Definition budget (AUD-068): reject an over-budget graph with a typed
	// ErrValidation. Checked here, after the graph is fully wired and validated so
	// GetLevels() is well-defined (acyclic), and before the built token is stamped
	// so an over-budget DAG is never certified. edgeCount is the effective edge set
	// (declared + folded choice/merge edges), the same set the wiring above used.
	if b.budget.MaxNodes > 0 || b.budget.MaxEdges > 0 || b.budget.MaxWidth > 0 {
		edgeCount := 0
		for _, deps := range effectiveDeps {
			edgeCount += len(deps)
		}
		if err := validateDefinitionBudget(nodeCount, edgeCount, dag.GetLevels(), b.budget); err != nil {
			return nil, err
		}
	}

	dag.built = true
	return dag, nil
}

// validateDefinitionBudget rejects a definition that exceeds any set cap in the
// budget (AUD-068). A zero/negative cap disables that axis. Errors are typed
// ErrValidation and name the axis, the actual count, and the ceiling so a host
// can see how far over it is.
func validateDefinitionBudget(nodeCount, edgeCount int, levels [][]*Node, budget DefinitionBudget) error {
	if budget.MaxNodes > 0 && nodeCount > budget.MaxNodes {
		return fmt.Errorf("%w: definition has %d nodes, exceeds the %d-node budget", ErrValidation, nodeCount, budget.MaxNodes)
	}
	if budget.MaxEdges > 0 && edgeCount > budget.MaxEdges {
		return fmt.Errorf("%w: definition has %d dependency edges, exceeds the %d-edge budget", ErrValidation, edgeCount, budget.MaxEdges)
	}
	if budget.MaxWidth > 0 {
		widest, atLevel := 0, 0
		for i, level := range levels {
			if len(level) > widest {
				widest, atLevel = len(level), i
			}
		}
		if widest > budget.MaxWidth {
			return fmt.Errorf("%w: definition level %d has static width %d, exceeds the %d-width budget", ErrValidation, atLevel, widest, budget.MaxWidth)
		}
	}
	return nil
}
