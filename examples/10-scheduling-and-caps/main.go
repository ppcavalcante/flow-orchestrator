// Command 10-scheduling-and-caps shows two governance primitives that make a durable
// engine safe to run unattended: durable SCHEDULES and cross-process concurrency CAPS.
//
// Part A — durable schedule (deterministic clock):
//
//	A workflow arms three durable timers (fire at +1h, +2h, +3h). A timer is durable
//	DATA — an absolute fireAt persisted in the checkpoint, not a live time.Timer — so it
//	survives a crash and fires on resume. A host POLLER loop drives it: it advances an
//	injected FakeClock, asks DueTimers(now) which timers are due, and calls Tick(now) to
//	fire them. Because the clock is injected, the whole "3-hour schedule" runs as an
//	instant, deterministic test — no wall-clock sleeping, no flakiness.
//
//	          Execute (arm+park) ──park──▶ poller: Advance clock ─▶ DueTimers ─▶ Tick
//	          timer+1h ─▶ job-1     ↑                                    │
//	          timer+2h ─▶ job-2     └──────────── resume until converged ┘
//	          timer+3h ─▶ job-3
//
// Part B — cross-process concurrency cap:
//
//	Many "transcode" jobs sit on a shared queue; a fleet of workers drains them. A cap
//	(WithCaps: at most K transcode RUNNING at once) is enforced inside the store's atomic
//	ClaimNext across every worker/process — a claim that would exceed K is refused until a
//	running one finishes. Each job brackets its work window so we can sweep live
//	concurrency and prove the peak never exceeds K, even under a fleet that would otherwise
//	run all of them at once.
//
// Run it:
//
//	go run ./examples/10-scheduling-and-caps
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("10-scheduling-and-caps: %v", err)
	}
}

func run() error {
	if err := runSchedule(); err != nil {
		return fmt.Errorf("schedule: %w", err)
	}
	fmt.Println()
	if err := runCap(); err != nil {
		return fmt.Errorf("cap: %w", err)
	}
	return nil
}

// ─────────────────────────── Part A: durable schedule ───────────────────────────

// Each scheduled job is its OWN durable one-shot schedule: an independent workflow that
// parks on a single timer and fires once, at its own due time. Independent workflows (not
// three timers in one graph) are what makes the fires land staggered over the tick loop —
// a real scheduler dispatches each due job independently, exactly like this.
type scheduledJob struct {
	id    string        // the schedule's durable workflow id
	name  string        // the effect this job records when it fires
	delay time.Duration // fires at arm-time + delay (an absolute, durable fireAt)
}

var scheduledJobs = []scheduledJob{
	{"sched-1", "job-1", 1 * time.Hour},
	{"sched-2", "job-2", 2 * time.Hour},
	{"sched-3", "job-3", 3 * time.Hour},
}

// fireLog is the process-shared, concurrency-safe record of which jobs fired, in order. A
// job node appends its name when it runs; because the engine never re-runs a Completed
// node, each job appears exactly once no matter how many Ticks drive the workflow.
type fireLog struct {
	mu    sync.Mutex
	fired []string
}

func newFireLog() *fireLog { return &fireLog{} }

func (l *fireLog) record(job string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fired = append(l.fired, job)
}

func (l *fireLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.fired...)
}

// newOneShotSchedule builds one job's durable schedule over the store + injected clock:
// a single timer node gating the job's record. It is rebuilt on every poll iteration on
// purpose — each Tick loads the durable timer state fresh from the store, so a rebuilt
// workflow (or a whole new process) resumes exactly where the last left off. That fresh
// load is what "durable schedule" means: the fire survives the process, not a live timer.
func newOneShotSchedule(store workflow.WorkflowStore, clk workflow.Clock, log *fireLog, j scheduledJob) (*workflow.Workflow, error) {
	b := workflow.NewWorkflowBuilder().
		WithWorkflowID(j.id).
		WithStore(store)

	// AddTimer arms an absolute fireAt = clock.Now()+delay, frozen at the first Execute and
	// persisted. Do NOT call WithAction on a timer — the timer action is set for you.
	b.AddTimer("timer", j.delay)
	// The job runs when its timer fires; it records the durable "fire" effect exactly once.
	name := j.name
	b.AddNode("fire").DependsOn("timer").
		WithActionFunc(func(_ context.Context, _ *workflow.WorkflowData) error {
			log.record(name)
			return nil
		})

	wf, err := workflow.FromBuilder(b)
	if err != nil {
		return nil, err
	}
	return wf.WithClock(clk), nil
}

// armSchedules arms each one-shot schedule: the first Execute parks it on its timer,
// persisting an absolute fireAt (ErrSuspended is expected — it means "a node is waiting",
// not a failure). After this returns, the schedules survive a process/handle restart.
func armSchedules(ctx context.Context, store workflow.WorkflowStore, clk workflow.Clock, log *fireLog) error {
	for _, j := range scheduledJobs {
		wf, err := newOneShotSchedule(store, clk, log, j)
		if err != nil {
			return err
		}
		if err := wf.Execute(ctx); err != nil && !errors.Is(err, workflow.ErrSuspended) {
			return fmt.Errorf("arm %s: %w", j.id, err)
		}
	}
	return nil
}

// tickSchedules is the host poller loop over ALREADY-ARMED schedules. It advances the
// FakeClock by `step` up to `maxTicks` times; on each tick, for each schedule, it asks
// DueTimers(now) and calls Tick(now) if the timer is due. Returns the number of fires.
// Because each schedule is independent, each fires at its own due instant, so the fire log
// comes out in due-time order — a genuine schedule over time.
func tickSchedules(ctx context.Context, store workflow.WorkflowStore, clk *workflow.FakeClock, log *fireLog, step time.Duration, maxTicks int) (int, error) {
	fires := 0
	for i := 0; i < maxTicks; i++ {
		clk.Advance(step)
		now := clk.Now()

		for _, j := range scheduledJobs {
			// Rebuild over the durable state (a fresh workflow / a new process would do the same).
			wf, err := newOneShotSchedule(store, clk, log, j)
			if err != nil {
				return fires, err
			}
			due, err := wf.DueTimers(now)
			if err != nil {
				return fires, fmt.Errorf("due-timers %s: %w", j.id, err)
			}
			if len(due) == 0 {
				continue // not due yet (or already fired) — a cheap no-op poll
			}
			fired, err := wf.Tick(ctx, now)
			if err != nil {
				// nil = the schedule converged (fired + job ran); a timer schedule has no
				// other parked node, so ErrSuspended is not expected here — any error is real.
				return fires, fmt.Errorf("tick %s at %s: %w", j.id, now.Format(time.RFC3339), err)
			}
			if fired {
				fires++
			}
		}
	}
	return fires, nil
}

// pollSchedules arms every one-shot schedule and drives them all to completion with a
// deterministic clock.
func pollSchedules(ctx context.Context, store workflow.WorkflowStore, clk *workflow.FakeClock, log *fireLog, step time.Duration, maxTicks int) (int, error) {
	if err := armSchedules(ctx, store, clk, log); err != nil {
		return 0, err
	}
	return tickSchedules(ctx, store, clk, log, step, maxTicks)
}

func runSchedule() error {
	dir, err := os.MkdirTemp("", "schedule")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	store, err := workflow.NewSQLiteStore(filepath.Join(dir, "schedule.db"))
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	// A fixed epoch so the run is byte-identical every time — no wall clock anywhere.
	epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := workflow.NewFakeClock(epoch)
	log := newFireLog()

	// 8 ticks of 30 min = a 4-hour horizon; the +1h/+2h/+3h schedules fire at ticks 2, 4, 6.
	fires, err := pollSchedules(context.Background(), store, clk, log, 30*time.Minute, 8)
	if err != nil {
		return err
	}

	fired := log.snapshot()
	fmt.Printf("durable schedule: %d jobs fired over 8 ticks of a controllable clock (fires=%d)\n", len(fired), fires)
	fmt.Printf("fire order: %v\n", fired)
	if len(fired) != len(scheduledJobs) {
		return fmt.Errorf("expected %d fires, got %d", len(scheduledJobs), len(fired))
	}
	return nil
}

// ─────────────────────────── Part B: concurrency cap ───────────────────────────

const (
	typeTranscode = "transcode"
	keyJobID      = "job_id"
	keyDone       = "done"
)

// Cap-demo sizing. A cap of K transcodes, a fleet of more workers than K trying to run
// everything at once, and a work window long enough that concurrent transcodes actually
// overlap in wall-clock so the peak is observable.
const (
	transcodeCap  = 2                     // at most K=2 transcodes RUNNING at once, fleet-wide
	capWorkers    = 6                     // more workers than the cap → real contention
	capJobs       = 10                    // several jobs per worker
	transcodeWork = 80 * time.Millisecond // the work window the cap bounds; long enough that the K admitted claims reliably overlap
	capIdleGrace  = 500 * time.Millisecond
)

// concurrencyMeter tracks live + peak concurrent transcodes across the fleet, plus a
// per-job run count (to prove exactly-once). A job brackets its work window with enter/exit.
type concurrencyMeter struct {
	mu   sync.Mutex
	live int
	peak int
	runs map[string]int
}

func newConcurrencyMeter() *concurrencyMeter { return &concurrencyMeter{runs: map[string]int{}} }

func (m *concurrencyMeter) enter(jobID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.live++
	if m.live > m.peak {
		m.peak = m.live
	}
	m.runs[jobID]++
}

func (m *concurrencyMeter) exit() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.live--
}

func (m *concurrencyMeter) peakConcurrency() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.peak
}

func (m *concurrencyMeter) runCounts() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]int, len(m.runs))
	for k, v := range m.runs {
		out[k] = v
	}
	return out
}

// buildTranscode is the "transcode" DAGFactory: one node that holds a work window open
// (so concurrent transcodes overlap) bracketed by the meter, then records completion.
func buildTranscode(meter *concurrencyMeter) workflow.DAGFactory {
	return func() (*workflow.DAG, error) {
		b := workflow.NewWorkflowBuilder()
		b.AddStartNode("transcode").WithActionFunc(func(_ context.Context, d *workflow.WorkflowData) error {
			jobID, _ := d.GetString(keyJobID)
			meter.enter(jobID)
			defer meter.exit()
			time.Sleep(transcodeWork) // the window the cap bounds; long enough to overlap
			d.Set(keyDone, true)
			return nil
		})
		return b.Build()
	}
}

// openCapStore opens a worker's OWN handle to the shared DB with the transcode cap wired.
// EVERY worker must open WithCaps so the gate applies on every claim, fleet-wide.
func openCapStore(dbPath string, cap int) (*workflow.SQLiteStore, error) {
	return workflow.NewSQLiteStore(dbPath,
		workflow.WithMultiProcess(),
		workflow.WithCaps(workflow.Caps{PerType: map[string]int{typeTranscode: cap}}))
}

// drainCap runs one worker draining transcode jobs until the queue is idle for capIdleGrace.
func drainCap(ctx context.Context, store *workflow.SQLiteStore, reg *workflow.Registry, ownerID string) error {
	lastWork := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ran, err := workflow.RunNext(ctx, store, reg, ownerID)
		switch {
		case ran:
			lastWork = time.Now()
		default:
			// No progress: EITHER an empty queue, OR the cap refusing admission because K
			// transcodes are already running, OR a transient fault. Back off and retry; quit
			// only after a real idle window so a persistent fault can never spin forever.
			if time.Since(lastWork) > capIdleGrace {
				return nil
			}
			_ = err // ErrNoWork / cap-refusal / transient all handled by backing off
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// runCapFleet enqueues capJobs transcodes and drains them with capWorkers workers (each its
// own store handle, all sharing the cap). Returns the meter's observations.
func runCapFleet(ctx context.Context, dbPath string, meter *concurrencyMeter, nJobs, nWorkers, cap int) error {
	reg := workflow.NewRegistry()
	if err := reg.Register(typeTranscode, buildTranscode(meter)); err != nil {
		return err
	}

	enq, err := openCapStore(dbPath, cap)
	if err != nil {
		return fmt.Errorf("open enqueue store: %w", err)
	}
	for i := 0; i < nJobs; i++ {
		jobID := fmt.Sprintf("clip-%02d", i)
		input, err := json.Marshal(map[string]any{keyJobID: jobID})
		if err != nil {
			return err
		}
		if _, err := enq.Enqueue("transcode:"+jobID, typeTranscode, input); err != nil {
			return fmt.Errorf("enqueue %s: %w", jobID, err)
		}
	}
	if err := enq.Close(); err != nil {
		return fmt.Errorf("close enqueue store: %w", err)
	}

	// A start barrier: every worker opens its store handle FIRST, then all begin draining
	// together on close(start). Without it, one worker's whole transcode window could finish
	// before a sibling even finished opening its handle — the cap would then never be
	// contended, and the peak-reaches-K observation would be a flaky race. The barrier makes
	// the K admitted claims reliably overlap so the cap is genuinely exercised.
	errs := make([]error, nWorkers)
	start := make(chan struct{})
	var ready, wg sync.WaitGroup
	ready.Add(nWorkers)
	wg.Add(nWorkers)
	for i := 0; i < nWorkers; i++ {
		go func(idx int) {
			defer wg.Done()
			store, err := openCapStore(dbPath, cap)
			ready.Done() // signal "handle open" whether or not it succeeded (never deadlock the barrier)
			if err != nil {
				errs[idx] = err
				return
			}
			defer store.Close()
			<-start // all workers released together
			if err := drainCap(ctx, store, reg, fmt.Sprintf("worker-%d", idx)); err != nil && ctx.Err() == nil {
				errs[idx] = err
			}
		}(i)
	}
	ready.Wait() // every handle is open
	close(start) // release the fleet
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func runCap() error {
	dir, err := os.MkdirTemp("", "caps")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	dbPath := filepath.Join(dir, "queue.db")

	meter := newConcurrencyMeter()
	if err := runCapFleet(context.Background(), dbPath, meter, capJobs, capWorkers, transcodeCap); err != nil {
		return err
	}

	peak := meter.peakConcurrency()
	runs := meter.runCounts()
	fmt.Printf("concurrency cap: %d transcodes, %d workers, cap K=%d → peak concurrency observed = %d\n",
		capJobs, capWorkers, transcodeCap, peak)
	if peak > transcodeCap {
		return fmt.Errorf("cap exceeded: peak %d > K=%d", peak, transcodeCap)
	}
	for id, n := range runs {
		if n != 1 {
			return fmt.Errorf("job %s ran %d times, want exactly 1", id, n)
		}
	}
	fmt.Printf("all %d transcodes completed exactly once, peak never exceeded K=%d\n", len(runs), transcodeCap)
	return nil
}
