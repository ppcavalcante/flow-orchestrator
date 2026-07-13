package workflow

// Per-Execute delta capture (M15 ph69) — the O(1)-at-mutator accumulator that makes the
// SQLite fast path genuinely O(N). When a durable run drives forward under an
// IncrementalCheckpointer store, the executor turns capture ON for the duration of the
// Execute; each mutator (SetNodeStatus/Set/SetOutput/SetWait/ClearWait) records its
// touched key O(1) under the w.mu it already holds; the per-level barrier flush drains the
// accumulated ChangeSet and hands it to SaveDeltaCheckpoint.
//
// ZERO-ALLOC WHEN INACTIVE (load-bearing — the mutators are on the NON-durable hot path;
// det-tax must stay EXACT 283/277): capture is a NIL pointer when off. recordDelta is a
// single nil-check + early return — a branch, no allocation. The accumulator is allocated
// once per beginDeltaCapture (durable path only), never on the hot path.
//
// RACE-SAFE: the accumulator lives on the per-Execute *WorkflowData (a call parameter, NOT
// shared across concurrent Executes — that was the M14 ph61 hazard, which was a field on
// the SHARED w.DAG). It is guarded by the SAME w.mu the mutators already take, so recording
// is free of any new lock. (DEC-M15-INCR-IFACE, constraint 2 corrected: on-WorkflowData-
// per-Execute, not on-ctx — the mutators have no ctx.)

// deltaDomain discriminates which decomposed table a touched key belongs to.
type deltaDomain uint8

const (
	deltaNode deltaDomain = iota // nodeStatus + outputs (both keyed by node name)
	deltaData                    // data_kv
	deltaWait                    // waits
)

// deltaCapture accumulates the DISTINCT touched keys per domain since the last flush. Maps
// act as sets so a key touched N times in a level is one entry (the store re-reads its
// current value from d once). Nil-valued maps are lazily created on first record.
type deltaCapture struct {
	nodes map[string]struct{}
	data  map[string]struct{}
	waits map[string]struct{}
}

// ChangeSet is the per-level changed-key set handed to SaveDeltaCheckpoint (M15 ph69,
// DEC-M15-INCR-IFACE). MULTI-DOMAIN: node names, data keys, and wait node-names each get
// their own slice. The store UPSERTs each reported key by RE-READING its current value from
// the WorkflowData; a reported key ABSENT from the WorkflowData means it was deleted
// (a consumed wait / cleared entry) ⇒ the store DELETEs it. Encoding stays inside the store.
type ChangeSet struct {
	Nodes    []string
	DataKeys []string
	WaitKeys []string
}

// IncrementalCheckpointer is the additive optional interface (M15 ph69). A store that
// implements it receives the per-level changed-set → O(Δ) compute+writes = genuine O(N)
// forward drive. Type-asserted like Checkpointer/Syncer; a store that does NOT implement it
// falls back to the full SaveCheckpoint (the ph67 shadow-diff), unchanged. Forward-drive
// only: M12 rollback persists via full Save (cold path), out of ph69 scope.
type IncrementalCheckpointer interface {
	// SaveDeltaCheckpoint persists ONLY the keys in changed (re-read from d), in one
	// crash-atomic transaction. Absence of a reported key in d ⇒ DELETE. The union of a
	// run's delta checkpoints must reconstruct byte-identically to a full SaveCheckpoint.
	SaveDeltaCheckpoint(changed ChangeSet, d *WorkflowData) error
}

// beginDeltaCapture turns capture ON for this WorkflowData (idempotent — re-arming resets
// the accumulator). Called by the executor at Execute start ONLY when the store is an
// IncrementalCheckpointer, so plain-Checkpointer runs never allocate the accumulator.
func (w *WorkflowData) beginDeltaCapture() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.capture = &deltaCapture{}
}

// endDeltaCapture turns capture OFF (Execute end / a non-durable run). Idempotent.
func (w *WorkflowData) endDeltaCapture() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.capture = nil
}

// recordDelta notes a touched key. HOT-PATH-CRITICAL: when capture is off (w.capture ==
// nil) this is a single branch + return, zero alloc. Caller holds w.mu (every mutator does).
func (w *WorkflowData) recordDelta(domain deltaDomain, key string) {
	c := w.capture
	if c == nil {
		return // capture inactive — the non-durable / plain-Checkpointer hot path
	}
	switch domain {
	case deltaNode:
		if c.nodes == nil {
			c.nodes = make(map[string]struct{})
		}
		c.nodes[key] = struct{}{}
	case deltaData:
		if c.data == nil {
			c.data = make(map[string]struct{})
		}
		c.data[key] = struct{}{}
	case deltaWait:
		if c.waits == nil {
			c.waits = make(map[string]struct{})
		}
		c.waits[key] = struct{}{}
	}
}

// drainDeltaCapture returns the accumulated ChangeSet and RESETS the accumulator (the
// per-barrier flush). Returns an empty ChangeSet + false when capture is inactive (the
// caller then uses the full SaveCheckpoint path). Key order is Go map-iteration order
// (unsorted) — fidelity is order-independent (Load reads rows unordered; UPSERT/DELETE
// commute within one txn), so the write order does not matter (ph69-F3).
func (w *WorkflowData) drainDeltaCapture() (ChangeSet, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	c := w.capture
	if c == nil {
		return ChangeSet{}, false
	}
	cs := ChangeSet{
		Nodes:    setKeys(c.nodes),
		DataKeys: setKeys(c.data),
		WaitKeys: setKeys(c.waits),
	}
	// Reset for the next level (keep capture ON — a new empty accumulator).
	w.capture = &deltaCapture{}
	return cs, true
}

// setKeys returns a set's keys as a slice (nil for an empty/nil set — no allocation).
func setKeys(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
