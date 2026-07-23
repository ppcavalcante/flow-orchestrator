package workflow

// M18 ph86 Slice 2 — the OTel bridge for dispatch metrics (OBS-MET state gauges + event counters through
// OTel). Mirrors the M14 metrics/otel.go pattern (API-only — the OTel metric API, never the SDK; the host
// owns the SDK/exporter/endpoint) but lives in the workflow package because it reads the store's
// Observability read-model (ph85) + DispatchMetrics (Slice 1), which are workflow-package types.
//
// Two instrument families, split by provenance (DEC-M18-METRICS):
//   - STATE GAUGES (store-derived, live): an observable callback reads ph85's QueueCounts()/InFlight() each
//     collection cycle → per-state queue depth + in-flight/lease-live gauges. No new state; the gauge IS
//     the read-model call at that instant (BITE 3).
//   - EVENT COUNTERS (in-process): observable counters reading the DispatchMetrics atomics (reclaim/
//     fence-reject/superseded/retry/dead-letter).
//
// BEST-EFFORT ISOLATION: NewOTelDispatchBridge returns an ERROR (never panics) on a nil provider or a
// failed instrument/callback creation. The host logs it; the in-process atomics STILL increment + are
// queryable via the DispatchMetrics getters; the DISPATCH PATH IS NEVER AFFECTED. Metrics are opt-in
// telemetry, not a dispatch dependency.

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const dispatchInstrumentationScope = "github.com/ppcavalcante/flow-orchestrator/dispatch"

// stateAttrKey — the single bounded attribute on the queue-depth gauge: state=<pending|claimed|…>. A
// closed set (the wqStates), never a workflow-controlled string (the cardinality contract).
const stateAttrKey = "state"

// OTelDispatchBridge registers the dispatch-metrics observable instruments with a host MeterProvider. Its
// callback reads the ph85 read-model (state gauges) + the DispatchMetrics atomics (event counters) each
// collection cycle. Shutdown unregisters. Constructed via NewOTelDispatchBridge.
type OTelDispatchBridge struct {
	reg metric.Registration
}

// NewOTelDispatchBridge wires the dispatch-metrics observable instruments. `store` supplies the live state
// gauges (its Observability read-model); `m` supplies the event-counter atomics; `mp` is the host's
// MeterProvider. Returns an error (NEVER panics) on a nil provider or any instrument/callback failure —
// the caller treats a non-nil error as "OTel telemetry unavailable", NOT a dispatch failure (the atomics
// still work). A nil `m` disables the event counters (state gauges still register); a nil `store` disables
// the state gauges. mp must be non-nil (that is the host's contract for wanting OTel at all).
func NewOTelDispatchBridge(store Observability, m *DispatchMetrics, mp metric.MeterProvider) (*OTelDispatchBridge, error) {
	if mp == nil {
		return nil, fmt.Errorf("%w: NewOTelDispatchBridge requires a non-nil metric.MeterProvider", ErrValidation)
	}
	meter := mp.Meter(dispatchInstrumentationScope)

	depthGauge, err := meter.Int64ObservableGauge(
		"flow_orchestrator.dispatch.queue.depth",
		metric.WithUnit("{workflow}"),
		metric.WithDescription("Work-queue depth by state (pending/claimed/done/failed/cancelled), read live from the store."),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: create queue.depth gauge: %w", ErrIO, err)
	}
	inflightGauge, err := meter.Int64ObservableGauge(
		"flow_orchestrator.dispatch.inflight",
		metric.WithUnit("{workflow}"),
		metric.WithDescription("In-flight (claimed) workflow count, read live from the store."),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: create inflight gauge: %w", ErrIO, err)
	}
	reclaimCtr, err := meter.Int64ObservableCounter(
		"flow_orchestrator.dispatch.reclaim_after_death",
		metric.WithUnit("{event}"),
		metric.WithDescription("Cumulative reclaim-after-death events (in-process, reset on restart)."),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: create reclaim counter: %w", ErrIO, err)
	}
	fenceCtr, err := meter.Int64ObservableCounter(
		"flow_orchestrator.dispatch.fence_rejections",
		metric.WithUnit("{event}"),
		metric.WithDescription("Cumulative fence-rejection events (superseded terminal flips rejected)."),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: create fence counter: %w", ErrIO, err)
	}
	// review ph86-F1 (HIGH): the bridge documents surfacing ALL five event counters — it must not silently
	// export a subset. supersededAborts/retriesAttempted/deadLetters are the operational signals a host
	// alerts on (dead-letters especially = poison/budget-exhausted workflows); export them too.
	supersededCtr, err := meter.Int64ObservableCounter(
		"flow_orchestrator.dispatch.superseded_aborts",
		metric.WithUnit("{event}"),
		metric.WithDescription("Cumulative superseded-abort events (a fenced drive aborted without terminalizing)."),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: create superseded counter: %w", ErrIO, err)
	}
	retryCtr, err := meter.Int64ObservableCounter(
		"flow_orchestrator.dispatch.retries_attempted",
		metric.WithUnit("{event}"),
		metric.WithDescription("Cumulative retry-attempted events (a transient-fault drive requeued)."),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: create retry counter: %w", ErrIO, err)
	}
	deadLetterCtr, err := meter.Int64ObservableCounter(
		"flow_orchestrator.dispatch.dead_letters",
		metric.WithUnit("{event}"),
		metric.WithDescription("Cumulative dead-letter events (a drive terminalized failed: poison or budget-exhausted)."),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: create dead-letter counter: %w", ErrIO, err)
	}
	missedFireCtr, err := meter.Int64ObservableCounter(
		"flow_orchestrator.dispatch.missed_fires",
		metric.WithUnit("{event}"),
		metric.WithDescription("Cumulative missed-fire events (M20 ph103: a due scheduled fire blocked by a saturated cap)."),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: create missed-fire counter: %w", ErrIO, err)
	}

	reg, err := meter.RegisterCallback(
		func(_ context.Context, o metric.Observer) error {
			// STATE GAUGES — read the ph85 read-model LIVE (the gauge IS the read-model value, BITE 3).
			if store != nil {
				if counts, cerr := store.QueueCounts(""); cerr == nil {
					for state, n := range counts {
						o.ObserveInt64(depthGauge, int64(n), metric.WithAttributes(attribute.String(stateAttrKey, state)))
					}
				}
				if inflight, ierr := store.InFlight(); ierr == nil {
					o.ObserveInt64(inflightGauge, int64(len(inflight)))
				}
			}
			// EVENT COUNTERS — read the in-process atomics (ALL six, review ph86-F1 + M20 ph103 missed-fires).
			if m != nil {
				o.ObserveInt64(reclaimCtr, m.ReclaimAfterDeath())
				o.ObserveInt64(fenceCtr, m.FenceRejections())
				o.ObserveInt64(supersededCtr, m.SupersededAborts())
				o.ObserveInt64(retryCtr, m.RetriesAttempted())
				o.ObserveInt64(deadLetterCtr, m.DeadLetters())
				o.ObserveInt64(missedFireCtr, m.MissedFires())
			}
			return nil
		},
		depthGauge, inflightGauge, reclaimCtr, fenceCtr, supersededCtr, retryCtr, deadLetterCtr, missedFireCtr,
	)
	if err != nil {
		return nil, fmt.Errorf("%w: register dispatch OTel callback: %w", ErrIO, err)
	}
	return &OTelDispatchBridge{reg: reg}, nil
}

// Shutdown unregisters the bridge's callback. nil-safe + idempotent.
func (b *OTelDispatchBridge) Shutdown(_ context.Context) error {
	if b == nil || b.reg == nil {
		return nil
	}
	return b.reg.Unregister()
}
