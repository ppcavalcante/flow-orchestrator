package workflow

// M20 ph100 — durable schedules + the fenced single-txn fire (SCHED-01..06, DEC-P100-*). A schedule fires a run
// of its target workflow type onto the M17 work_queue when its stored next_fire_time passes. Firing is safe
// across many processes: the M16 fencing/lease lineage (reused, NOT re-invented — DEC-P100-FENCE-PER-SCHEDULE)
// gives one-writer-per-schedule, and the whole fire — {claim (fenced), capAdmits stub, enqueue, advance
// next_fire} — runs in ONE BEGIN IMMEDIATE txn so a due slot is enqueued EXACTLY ONCE even under N racing
// pollers (DEC-P100-SINGLE-TXN, the exactly-once-enqueue safety property). No network entry point: the poller is
// an in-process goroutine an embedder opts into (schedule_poller.go).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// schedule kinds — the fire-advance discriminator.
const (
	schedCron     = "cron"
	schedInterval = "interval"
	schedOneshot  = "oneshot"
)

// missed-run policies (DEC-P100-MISSED-RUN, SCHED-04).
const (
	missedSkip    = "skip"    // skip-to-next: advance past all missed slots to the next future slot, fire nothing extra
	missedCatchup = "catchup" // catch-up-once: fire exactly one run for the missed window, then advance to the next future slot
)

// scheduleLeaseKey namespaces a schedule id into the shared M16 leases id-space so a schedule claim
// (claimLocked) can never collide with a workflow lease (DEC-P100-FENCE-PER-SCHEDULE).
func scheduleLeaseKey(id string) string { return "schedule:" + id }

// ErrSchedule is the typed schedule-config error sentinel (bad kind, empty id/type, unparseable spec).
var ErrSchedule = fmt.Errorf("%w: invalid schedule", ErrValidation)

// ScheduleSpec is the caller's declaration of a schedule (SCHED-01). Construct via NewCronSchedule /
// NewIntervalSchedule / NewOneshotSchedule; register with SQLiteStore.CreateSchedule.
type ScheduleSpec struct {
	ID         string        // stable id (idempotent re-register target, SCHED-05)
	kind       string        // schedCron | schedInterval | schedOneshot
	spec       string        // cron: the 5-field spec; interval: period-nanos as text; oneshot: ""
	targetType string        // the work_queue dispatch type the fire enqueues
	interval   time.Duration // interval kind only
	firstFire  time.Time     // the first next_fire_time (oneshot: the fire instant; cron/interval: the anchor)
	missedSkip bool          // true = skip-to-next (default); false = catch-up-once
	haveMissed bool          // whether a missed-policy was explicitly chosen (else default skip)
}

// NewCronSchedule builds a cron schedule firing target type `typ` per the 5-field `spec`. firstFire is
// cron.Next(anchor.UTC()) — the first slot strictly after `anchor`, evaluated in UTC.
//
// UTC IS LOAD-BEARING FOR CONSISTENCY (review ph100-F1): the fire-advance (advanceSchedule) evaluates the cron
// spec in UTC, so the constructor MUST too — else a caller passing time.Now() in a non-UTC zone gets a
// local-calendar FIRST fire and a UTC calendar for every fire after, silently shifting the schedule by the zone
// offset after one fire. Anchoring in UTC here makes the whole fire calendar UTC (DEC-P100-TZ default). A
// per-schedule location is a clean additive follow-up (a tz column threaded to both paths).
func NewCronSchedule(id, typ, spec string, anchor time.Time) (ScheduleSpec, error) {
	if id == "" || typ == "" {
		return ScheduleSpec{}, fmt.Errorf("%w: cron schedule requires a non-empty id and target type", ErrSchedule)
	}
	sched, err := ParseCron(spec)
	if err != nil {
		return ScheduleSpec{}, fmt.Errorf("%w: cron schedule %q: %w", ErrSchedule, id, err)
	}
	first, err := sched.Next(anchor.UTC()) // UTC: consistent with advanceSchedule (ph100-F1).
	if err != nil {
		return ScheduleSpec{}, fmt.Errorf("%w: cron schedule %q has no fire time: %w", ErrSchedule, id, err)
	}
	return ScheduleSpec{ID: id, kind: schedCron, spec: spec, targetType: typ, firstFire: first}, nil
}

// NewIntervalSchedule builds a schedule firing `typ` every `period` starting at `anchor`+period. period must be > 0.
func NewIntervalSchedule(id, typ string, period time.Duration, anchor time.Time) (ScheduleSpec, error) {
	if id == "" || typ == "" {
		return ScheduleSpec{}, fmt.Errorf("%w: interval schedule requires a non-empty id and target type", ErrSchedule)
	}
	if period <= 0 {
		return ScheduleSpec{}, fmt.Errorf("%w: interval schedule %q requires a positive period", ErrSchedule, id)
	}
	return ScheduleSpec{
		ID: id, kind: schedInterval, spec: strconv.FormatInt(int64(period), 10),
		targetType: typ, interval: period, firstFire: anchor.Add(period),
	}, nil
}

// NewOneshotSchedule builds a one-shot schedule firing `typ` once at `fireAt`. It auto-deletes on the fire-commit
// (DEC-M20-D7).
func NewOneshotSchedule(id, typ string, fireAt time.Time) (ScheduleSpec, error) {
	if id == "" || typ == "" {
		return ScheduleSpec{}, fmt.Errorf("%w: one-shot schedule requires a non-empty id and target type", ErrSchedule)
	}
	return ScheduleSpec{ID: id, kind: schedOneshot, spec: "", targetType: typ, firstFire: fireAt}, nil
}

// WithCatchupOnce sets the catch-up-once missed-run policy (persisted as `missed_policy='catchup'`). RESERVED —
// V-M20-01 / DEC-P103-CATCHUP-RESERVED: it is CURRENTLY IDENTICAL to the default skip-to-next. A due schedule
// whose next-fire is far in the past coalesces ALL missed slots into a SINGLE fire on the next poll and jumps to
// the first future slot, regardless of policy (advanceSchedule fires once + advances; the `policy` arg is not yet
// consumed). A distinct per-missed-slot catch-up (fire once per missed slot) is a deferred increment. The flag +
// column round-trip durably today (so a future implementation reads the intent), but the OBSERVABLE behavior does
// not yet differ from the default. Ignored for one-shot (no missed-run window). Use it to record the intent; do
// NOT rely on a distinct catch-up firing yet.
func (s ScheduleSpec) WithCatchupOnce() ScheduleSpec {
	s.missedSkip = false
	s.haveMissed = true
	return s
}

// CreateSchedule durably registers (or idempotently re-registers, SCHED-05) a schedule. A re-register of an
// existing id is a VISIBLE no-op on the fire state (the row is left byte-unchanged — never a silent next_fire
// reset that would double-fire), returning created=false. Requires mp mode (the schedules fire onto the mp
// work_queue via the fenced txn).
func (s *SQLiteStore) CreateSchedule(spec ScheduleSpec) (created bool, err error) {
	if !s.dur.mp {
		return false, fmt.Errorf("%w: CreateSchedule requires a multi-process store", ErrValidation)
	}
	if spec.ID == "" || spec.targetType == "" || spec.kind == "" {
		return false, fmt.Errorf("%w: incomplete schedule spec", ErrSchedule)
	}
	// VALIDATE the schedule id (review ph100-F2): the fire mints a work_queue run id `sched:<id>:<slot>` from
	// spec.ID — every OTHER enqueue path (Enqueue/EnqueueSubWorkflow) runs validateWorkflowID first. A
	// traversal-shaped id (containing `/` or `..`) would mint a run that every validated store op (Save/
	// checkpoint/Claim) then REJECTS with ErrValidation → the run can never persist, is re-claimed + re-failed
	// forever (and re-opens the path-traversal surface if the id reaches a file-backed store). Refuse it here.
	if verr := validateWorkflowID(spec.ID); verr != nil {
		return false, fmt.Errorf("%w: schedule id %q: %w", ErrSchedule, spec.ID, verr)
	}
	policy := missedSkip
	if spec.haveMissed && !spec.missedSkip {
		policy = missedCatchup
	}
	ctx := context.Background()
	s.mu.Lock()
	defer s.mu.Unlock()
	now := unixNanoNow()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO schedules(id, kind, spec, target_type, next_fire_time, missed_policy, paused, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,0,?,?)
		 ON CONFLICT(id) DO NOTHING`,
		spec.ID, spec.kind, spec.spec, spec.targetType, spec.firstFire.UnixNano(), policy, now, now)
	if err != nil {
		return false, classifyTxErr("create schedule", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("%w: create schedule rows: %w", ErrIO, err)
	}
	return n == 1, nil // 1 = a new schedule; 0 = the id already existed (idempotent re-register, byte-unchanged).
}

// PauseSchedule sets paused=1 (a paused schedule never fires; SCHED-03). Idempotent; a missing id is a 0-row no-op.
func (s *SQLiteStore) PauseSchedule(id string) (bool, error) { return s.setSchedulePaused(id, true) }

// ResumeSchedule clears paused (fires again on the next due slot). Idempotent.
func (s *SQLiteStore) ResumeSchedule(id string) (bool, error) { return s.setSchedulePaused(id, false) }

func (s *SQLiteStore) setSchedulePaused(id string, paused bool) (bool, error) {
	if !s.dur.mp {
		return false, fmt.Errorf("%w: schedule pause/resume requires a multi-process store", ErrValidation)
	}
	pv := 0
	if paused {
		pv = 1
	}
	ctx := context.Background()
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx,
		`UPDATE schedules SET paused=?, updated_at=? WHERE id=?`, pv, unixNanoNow(), id)
	if err != nil {
		return false, classifyTxErr("pause schedule", err)
	}
	n, _ := res.RowsAffected() //nolint:errcheck // modernc always returns it
	return n == 1, nil
}

// DeleteSchedule removes a schedule (stops all fires) AND releases its lease row so the id is immediately
// re-registerable (else the lease would block a same-id re-create from firing for up to the TTL). Idempotent; a
// missing id is a 0-row no-op. The schedule + lease deletes ride ONE BEGIN IMMEDIATE txn (atomic — review
// ph100-F4: the prior code deleted only the schedule row while the comment claimed a lease release it never did).
func (s *SQLiteStore) DeleteSchedule(id string) (bool, error) {
	if !s.dur.mp {
		return false, fmt.Errorf("%w: DeleteSchedule requires a multi-process store", ErrValidation)
	}
	ctx := context.Background()
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, classifyTxErr("delete schedule begin", err)
	}
	committed := false
	defer func() {
		if !committed {
			tx.Rollback() //nolint:errcheck,gosec // best-effort; the real error is already returned
		}
	}()
	res, err := tx.ExecContext(ctx, `DELETE FROM schedules WHERE id=?`, id)
	if err != nil {
		return false, classifyTxErr("delete schedule", err)
	}
	if _, lerr := tx.ExecContext(ctx, `DELETE FROM leases WHERE workflow_id=?`, scheduleLeaseKey(id)); lerr != nil {
		return false, classifyTxErr("delete schedule lease release", lerr)
	}
	n, _ := res.RowsAffected() //nolint:errcheck // modernc always returns it
	if cerr := tx.Commit(); cerr != nil {
		return false, classifyTxErr("delete schedule commit", cerr)
	}
	committed = true
	return n == 1, nil
}

// enqueueRunLocked is the tx-scoped enqueue used INSIDE the fire txn (G3): it inserts a pending work_queue row on
// the passed `tx` (NOT a fresh auto-commit txn like the public Enqueue), so the enqueue is atomic with the
// schedule claim + next-fire advance. Idempotent by workflow_id (ON CONFLICT DO NOTHING) like Enqueue.
func (s *SQLiteStore) enqueueRunLocked(ctx context.Context, tx *sql.Tx, workflowID, typ string, input []byte) (inserted bool, err error) {
	now := unixNanoNow()
	res, err := tx.ExecContext(ctx,
		`INSERT INTO work_queue(workflow_id, type, input, enqueued_at, state, attempts, updated_at)
		 VALUES (?,?,?,?, 'pending', 0, ?)
		 ON CONFLICT(workflow_id) DO NOTHING`,
		workflowID, typ, input, now, now)
	if err != nil {
		return false, classifyTxErr("schedule enqueue run", err)
	}
	n, _ := res.RowsAffected() //nolint:errcheck // modernc always returns it
	return n == 1, nil         // false = the run id already existed (ON CONFLICT no-op) → NOT a fresh enqueue (ph100-MINOR-1).
}

// capAdmits is the SCHED×CAP backlog-admission gate (M20 ph101, DEC-M20-D2 / DEC-P101-BACKLOG-CEILING): the
// fire-txn asks whether a scheduled run of `typ` may be enqueued under the concurrency caps. A false → the fire
// is a MISSED slot (fireDueLocked advances next_fire WITHOUT enqueuing = skip-to-next), so the (running+pending)
// population of `typ` is BOUNDED to cap(typ) — a schedule firing faster than workers drain can never grow the
// backlog unbounded (the barred catch-up-all footgun).
//
// THE DELIBERATE ASYMMETRY vs ph98 ClaimNext (DEC-P101-BACKLOG-CEILING): this counts running (claimed∧¬parked)
// PLUS pending, whereas ClaimNext's cap counts running-only. A schedule fires on wall-clock regardless of drain
// rate; if a fire-at-running-cap merely enqueued a pending item (ClaimNext semantics, where a pending item
// WAITS), the pending backlog would grow without bound. Counting pending too — and DROPPING the fire at the
// ceiling — bounds the total population. Same immutable WithCaps config value (DEC-P101-REUSE-CAP-CONFIG), a
// different referent SET, by design.
//
// The COUNTs run INSIDE the passed fire txn `tx` (DEC-M20-D3, crash-consistent, no fenced counter — the
// row-COUNT is the arbiter, [[engineer-fencing-extends-to-derived-state]]). Both caps AND (DEC-M20-D6): admitted
// iff under the per-type cap AND under the global cap. An untyped run (typ=="") is governed by the global cap
// only (capForType("") is uncapped). NO `workflow_id<>?` self-exclude (DEC-P101-NO-SELF-EXCLUDE): the scheduled
// run does not yet exist in work_queue, so the EXISTING population < ceiling means admitting one keeps it ≤
// ceiling. Unset caps ⇒ the isUnbounded() fast path ⇒ always-true ⇒ ph100 behavior byte-unchanged (CAP-05).
func (s *SQLiteStore) capAdmits(ctx context.Context, tx *sql.Tx, typ string) (bool, error) {
	if s.caps.isUnbounded() {
		return true, nil // no caps configured → ph100 semantics, byte-unchanged.
	}
	// Per-type gate: count running (claimed∧¬parked) + pending of this type.
	if limit, capped := s.caps.capForType(typ); capped {
		var pop int
		if qerr := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM work_queue WHERE type=? AND ((state='claimed' AND parked IS NULL) OR state='pending')`,
			typ).Scan(&pop); qerr != nil {
			return false, classifyTxErr("capadmits count (type)", qerr)
		}
		if pop >= limit {
			return false, nil // at the per-type backlog ceiling → this fire is a missed slot.
		}
	}
	// Global gate: count running + pending across ALL types.
	if limit, capped := s.caps.globalCap(); capped {
		var pop int
		if qerr := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM work_queue WHERE ((state='claimed' AND parked IS NULL) OR state='pending')`).
			Scan(&pop); qerr != nil {
			return false, classifyTxErr("capadmits count (global)", qerr)
		}
		if pop >= limit {
			return false, nil // at the global backlog ceiling → missed slot.
		}
	}
	return true, nil // under both gates → admit the fire.
}

// fireInput is the run-id + input a fire enqueues. The scheduled run's WorkflowID is deterministic per fire slot
// (schedule id + the fired next_fire_time) so a re-fire of the SAME slot (a crash-retry) enqueues the SAME id →
// the work_queue ON CONFLICT dedupes it → exactly-once-enqueue even if the advance didn't commit.
func scheduledRunID(scheduleID string, slot int64) string {
	return "sched:" + scheduleID + ":" + strconv.FormatInt(slot, 10)
}

// fireDueLocked runs the fenced single-txn fire for one due schedule row (DEC-P100-SINGLE-TXN). It is called by
// the poller under s.mu already held is NOT assumed — it takes s.mu itself. Returns fired=true iff this call
// enqueued a run (a lost claim / not-due / at-cap / paused → fired=false, no error). The WHOLE operation —
// claim the schedule lease (fenced), capAdmits, enqueue the run, advance next_fire (or delete a one-shot) — is
// ONE BEGIN IMMEDIATE txn: N racing pollers serialize on the write lock and exactly one enqueues the slot.
func (s *SQLiteStore) fireDueLocked(ctx context.Context, id, ownerID string) (fired bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil) // IMMEDIATE (mp DSN) → the write lock spans the whole fire.
	if err != nil {
		return false, classifyTxErr("fire begin", err)
	}
	committed := false
	defer func() {
		if !committed {
			tx.Rollback() //nolint:errcheck,gosec // best-effort; the real error is already returned
		}
	}()

	// Re-read the row INSIDE the txn (a sibling poller may have fired + advanced it since the poller's scan).
	var (
		kind, spec, targetType, policy string
		nextFire                       int64
		paused                         int
	)
	scanErr := tx.QueryRowContext(ctx,
		`SELECT kind, spec, target_type, next_fire_time, missed_policy, paused FROM schedules WHERE id=?`, id).
		Scan(&kind, &spec, &targetType, &nextFire, &policy, &paused)
	switch {
	case errors.Is(scanErr, sql.ErrNoRows):
		return false, nil // deleted since the scan → nothing to fire.
	case scanErr != nil:
		return false, classifyTxErr("fire read", scanErr)
	}

	now := s.nowNanos() // the INJECTED clock (DEC-P100-CLOCK-SEAM) — a FakeClock drives due-ness.
	if paused == 1 || nextFire > now {
		return false, nil // paused, or not yet due (a sibling already advanced it past now) → no fire.
	}

	// FENCE: claim the schedule's lease (one-writer-per-schedule). A live foreign lease → ErrClaimLost → skip
	// (another poller owns this fire); a lapsed/absent lease → we take it (fresh token). Reuses the M16 primitive.
	if _, claimErr := s.claimLocked(ctx, tx, scheduleLeaseKey(id), ownerID); claimErr != nil {
		if errors.Is(claimErr, ErrClaimLost) {
			return false, nil // a live poller owns this schedule's fire → back off.
		}
		return false, claimErr
	}

	// CAP (ph100 stub always-true; ph101 fills it): at-cap → treat as a missed slot, advance without enqueue.
	admit, capErr := s.capAdmits(ctx, tx, targetType)
	if capErr != nil {
		return false, capErr
	}

	// Compute the advance + decide whether THIS fire enqueues (missed-run policy, DEC-P100-MISSED-RUN).
	firedSlot := nextFire
	newNext, doEnqueue, computeErr := advanceSchedule(kind, spec, policy, nextFire, now)
	if computeErr != nil {
		// A CORRUPT SPEC (review ph100-MINOR-2): advanceSchedule can only fail on a spec that no longer parses —
		// impossible via the validating constructors, so this means direct DB tamper / bit-rot. Rolling back and
		// re-firing every tick would spin the poller silently forever (the row stays due). Instead PAUSE the row
		// (fail-safe: a corrupt schedule stops firing, is operator-visible as paused) + COMMIT so the pause
		// persists, then surface the error. This is the same fail-safe-quarantine posture as ClaimNext skipping a
		// corrupt-depth row (F-P95-03) — one bad row never wedges the poller.
		if _, perr := tx.ExecContext(ctx, `UPDATE schedules SET paused=1, updated_at=? WHERE id=?`, unixNanoNow(), id); perr != nil {
			return false, classifyTxErr("fire corrupt-spec pause", perr)
		}
		if cerr := tx.Commit(); cerr != nil {
			return false, classifyTxErr("fire corrupt-spec commit", cerr)
		}
		committed = true
		return false, computeErr
	}
	if admit && doEnqueue {
		runID := scheduledRunID(id, firedSlot)
		inserted, eerr := s.enqueueRunLocked(ctx, tx, runID, targetType, nil)
		if eerr != nil {
			return false, eerr
		}
		fired = inserted // report fired ONLY on a fresh enqueue (ph100-MINOR-1: an ON CONFLICT dedup is not a fire).
	}
	// MISSED FIRE (M20 ph103, O-P101): a DUE slot (doEnqueue) the cap BLOCKED (!admit). Operator visibility that a
	// schedule is starved by a saturated cap — for BOTH recurring skip-to-next AND one-shot RETAIN (both reach
	// here with doEnqueue=true, admit=false). NOT counted on not-due/paused (returned at the paused/nextFire>now
	// gate), lost-claim (ErrClaimLost return), or corrupt-spec (the computeErr pause+return) — all exit BEFORE this
	// point. The increment is DEFERRED to AFTER the commit (review ph103-note): like its sibling
	// incReclaimAfterDeath (post-commit), a rolled-back fire txn must not over-count the miss.
	missed := doEnqueue && !admit

	// Advance the durable next_fire (or delete a one-shot) IN THE SAME TXN as the enqueue → atomic move.
	switch {
	case kind == schedOneshot && admit:
		// A one-shot that FIRED (admitted) is gone forever → delete the row + release its lease atomically
		// (review ph100-F4: else the leases row leaks). The id can be cleanly re-registered.
		if _, derr := tx.ExecContext(ctx, `DELETE FROM schedules WHERE id=?`, id); derr != nil {
			return false, classifyTxErr("fire oneshot delete", derr)
		}
		if _, lerr := tx.ExecContext(ctx, `DELETE FROM leases WHERE workflow_id=?`, scheduleLeaseKey(id)); lerr != nil {
			return false, classifyTxErr("fire oneshot lease release", lerr)
		}
	case kind == schedOneshot: // !admit — cap-blocked
		// RETAIN a cap-blocked one-shot (DEC-P101-ONESHOT-AT-CAP-RETAIN, architect-ruled): a one-shot is an
		// explicit run-ONCE instruction; DROPPING it on a TRANSIENT cap silently loses it (data-loss-shaped).
		// Unlike a recurring schedule, a one-shot has no "next slot" to skip to — D2's skip-to-next assumes one.
		// So leave next_fire UNCHANGED (still ≤ now → still due) → it retries next poller tick and fires exactly
		// once when the cap drains. Retaining costs nothing D2 guards: it is a SINGLE durable schedules row, NOT a
		// growing work_queue backlog, and the cap is still respected (it does not fire while full). No delete, no
		// advance — the row simply stays due.
	default: // recurring (cron/interval): advance to the next slot whether or not this fire enqueued (D2 skip-to-next).
		if _, uerr := tx.ExecContext(ctx,
			`UPDATE schedules SET next_fire_time=?, updated_at=? WHERE id=?`,
			newNext, unixNanoNow(), id); uerr != nil {
			return false, classifyTxErr("fire advance", uerr)
		}
	}

	if cerr := tx.Commit(); cerr != nil {
		return false, classifyTxErr("fire commit", cerr)
	}
	committed = true
	// POST-COMMIT metric increment (review ph103-note): count the cap-blocked miss ONLY after the txn durably
	// commits — a rolled-back fire must not over-count (uniform with incReclaimAfterDeath, which increments
	// post-commit at ClaimNext). Opt-in/nil-safe via the s.metrics guard + the nil-receiver-safe helper.
	if missed && s.metrics != nil {
		s.metrics.incMissedFire()
	}
	return fired, nil
}

// advanceSchedule computes (newNextFire, doEnqueue) for a fire at `nextFire` observed at `now`, honoring the
// missed-run policy. For cron/interval it advances PAST every slot ≤ now to the first future slot; doEnqueue is
// true iff at least one slot was due. skip-to-next fires ONE run for the whole missed window (the current slot)
// then jumps ahead — matching standard "coalesce missed slots into one". catch-up-once likewise fires one, then
// advances to the next future slot (both fire exactly once per poll; the difference is only observable when we
// later add fire-per-missed-slot, deferred). A one-shot returns doEnqueue=true and is deleted by the caller.
func advanceSchedule(kind, spec, policy string, nextFire, now int64) (newNext int64, doEnqueue bool, err error) {
	switch kind {
	case schedOneshot:
		return 0, true, nil // fire once; caller deletes the row.
	case schedInterval:
		period, perr := strconv.ParseInt(spec, 10, 64)
		if perr != nil || period <= 0 {
			return 0, false, fmt.Errorf("%w: corrupt interval spec %q", ErrSchedule, spec)
		}
		// CLOSED-FORM advance past every elapsed period to the first future slot (review ph100-F3): the old
		// `for next<=now { next+=period }` loop ran (now-nextFire)/period iterations INSIDE the write txn — a
		// legal 1ns period one hour behind = ~3.6e12 iterations, stalling the store (s.mu + the SQLite write
		// lock) for minutes and every sibling process into SQLITE_BUSY. One division instead:
		//   k = (now - nextFire)/period + 1   (the number of periods to jump past now)
		//   next = nextFire + k*period
		// with an overflow guard (k*period or the sum wrapping int64 → a huge-period corrupt row).
		next := nextFire
		if now >= nextFire {
			k := (now-nextFire)/period + 1
			if k <= 0 || period > (int64(^uint64(0)>>1)-nextFire)/k { // k*period + nextFire would overflow int64
				return 0, false, fmt.Errorf("%w: interval advance overflows int64 (period %d too large)", ErrSchedule, period)
			}
			next = nextFire + k*period
		}
		_ = policy // skip vs catchup are indistinguishable at one-fire-per-poll granularity (see doc).
		return next, true, nil
	case schedCron:
		sched, cerr := ParseCron(spec)
		if cerr != nil {
			return 0, false, fmt.Errorf("%w: corrupt cron spec %q: %w", ErrSchedule, spec, cerr)
		}
		// Advance to the first cron slot strictly after `now` (coalesces all missed slots into this one fire).
		// Evaluated in UTC (DEC-P100-TZ default; ph100 stores no per-schedule location — a location column is a
		// clean additive follow-up. The ph99 DST-correctness lives in cron.Next; UTC has no transitions so a UTC
		// cron schedule is unaffected). ponytail: UTC-only, add a tz column when a non-UTC cron schedule is asked for.
		next, nerr := sched.Next(time.Unix(0, now).UTC())
		if nerr != nil {
			return 0, false, fmt.Errorf("%w: cron advance: %w", ErrSchedule, nerr)
		}
		_ = policy
		return next.UnixNano(), true, nil
	default:
		return 0, false, fmt.Errorf("%w: unknown schedule kind %q", ErrSchedule, kind)
	}
}
