package workflow

// M18 ph86 — DISPATCH metrics (OBS-MET): optional in-process event counters over the M17 dispatch layer
// (work_queue/leases). A NEW surface, distinct from the M14 per-workflow-run metrics (metrics.Config/
// MetricsCollector/OTelBridge — attached via Workflow.MetricsConfig, byte-unchanged here). Two parts split
// by data provenance (DEC-M18-METRICS, corrected):
//   - Event counters (this file): transient, per-process, reset-on-restart — reclaim-after-death,
//     fence-rejection, superseded-abort, retries-attempted, dead-letters. Atomic counters incremented at
//     the dispatch/store call sites. These are EVENTS, not durable columns.
//   - State gauges (workflow_store_sqlite_metrics_otel.go, Slice 2): store-derived, read LIVE from the
//     ph85 read-model (QueueCounts/InFlight) — no new durable state.
//
// ATTACHMENT (OBS-MOAT, nil = zero-cost): DispatchMetrics is an OPTIONAL pointer field on SQLiteStore
// (WithDispatchMetrics). When unset (nil), every increment site is a single `if s.metrics != nil` nil
// check — no counter, no allocation, no branch cost beyond the check (BITE 1). Opt-in only; the default
// dispatch path is byte-unchanged.

import "sync/atomic"

// DispatchMetrics holds the in-process dispatch event counters (OBS-MET-01/02 + the reclaim/dead-letter
// part of OBS-MET-03). All counters are atomic.Int64 (lock-free increment on the dispatch path). Reset on
// process restart is CORRECT — these are events, not durably-counted state. Construct with
// NewDispatchMetrics(); attach with WithDispatchMetrics(); read with the getters (for tests + the OTel
// observable-counter callbacks in Slice 2).
type DispatchMetrics struct {
	reclaimAfterDeath atomic.Int64 // a lapsed-claimed row was re-claimed by a live worker (ClaimNext reclaim branch)
	fenceRejections   atomic.Int64 // a superseded worker's terminal flip was rejected (flipTerminalFenced 0-row)
	supersededAborts  atomic.Int64 // a fenced drive aborted without terminalizing (disposeExecErr ErrFencedOut)
	retriesAttempted  atomic.Int64 // a transient-fault drive was requeued for retry (MarkForRetry)
	deadLetters       atomic.Int64 // a drive was dead-lettered (terminal MarkFailed on budget-exhausted/poison)
	missedFires       atomic.Int64 // M20 ph103: a DUE scheduled fire was blocked by a saturated concurrency cap
	// (fireDueLocked's !admit path — recurring skip-to-next AND one-shot RETAIN). Operator visibility that a
	// schedule is starved by its cap (the companion to DEC-P101-ONESHOT-AT-CAP-RETAIN).
}

// NewDispatchMetrics returns a zeroed DispatchMetrics ready to attach + read.
func NewDispatchMetrics() *DispatchMetrics { return &DispatchMetrics{} }

// --- getters (snapshot the current counter values; used by tests + the OTel counter callbacks) ---

// ReclaimAfterDeath returns the reclaim-after-death count.
func (m *DispatchMetrics) ReclaimAfterDeath() int64 { return m.reclaimAfterDeath.Load() }

// FenceRejections returns the fence-rejection count (superseded terminal flips rejected).
func (m *DispatchMetrics) FenceRejections() int64 { return m.fenceRejections.Load() }

// SupersededAborts returns the superseded-abort count (fenced drives that aborted).
func (m *DispatchMetrics) SupersededAborts() int64 { return m.supersededAborts.Load() }

// RetriesAttempted returns the retries-attempted count (requeued transient-fault drives).
func (m *DispatchMetrics) RetriesAttempted() int64 { return m.retriesAttempted.Load() }

// DeadLetters returns the dead-letter count.
func (m *DispatchMetrics) DeadLetters() int64 { return m.deadLetters.Load() }

// MissedFires returns the missed-fire count (M20 ph103): due scheduled fires blocked by a saturated cap.
func (m *DispatchMetrics) MissedFires() int64 { return m.missedFires.Load() }

// --- increment helpers (nil-safe: a nil *DispatchMetrics is a no-op, so call sites need no nil check
// beyond the store's own `if s.metrics != nil` gate — but these ARE nil-safe as a defense-in-depth for a
// nil receiver, keeping the call sites uniform). ---

func (m *DispatchMetrics) incReclaimAfterDeath() {
	if m != nil {
		m.reclaimAfterDeath.Add(1)
	}
}

func (m *DispatchMetrics) incFenceRejections() {
	if m != nil {
		m.fenceRejections.Add(1)
	}
}

func (m *DispatchMetrics) incSupersededAborts() {
	if m != nil {
		m.supersededAborts.Add(1)
	}
}

func (m *DispatchMetrics) incRetriesAttempted() {
	if m != nil {
		m.retriesAttempted.Add(1)
	}
}

func (m *DispatchMetrics) incDeadLetters() {
	if m != nil {
		m.deadLetters.Add(1)
	}
}

func (m *DispatchMetrics) incMissedFire() {
	if m != nil {
		m.missedFires.Add(1)
	}
}

// WithDispatchMetrics attaches an optional DispatchMetrics to the store (OBS-MET, opt-in). nil metrics
// (the default, when this option is not passed) means ZERO-COST: the dispatch path does no metrics work
// beyond a single nil check at each site. Compose with WithMultiProcess.
func WithDispatchMetrics(m *DispatchMetrics) SQLiteOption {
	return func(d *sqliteDurability) { d.dispatchMetrics = m }
}
