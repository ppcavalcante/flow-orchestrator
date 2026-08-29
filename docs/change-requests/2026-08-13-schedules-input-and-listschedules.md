# Response — flow-orchestrator change request (2026-08-13)

**Re:** `openai-workflow/docs/flow-orchestrator-change-request.md`
**Both requests: ACCEPTED and implemented.** Landed on `main` (unreleased; will ship in the next
`v0.23.0-alpha`). Your verified references were accurate; thank you for the precision.

| # | Request | Status | Commit |
|---|---|---|---|
| 1 | `schedules.input` — a schedule carries its fire's input | **Done** | `ce6cd76` |
| 2 | `ListSchedules` read API on `Observability` | **Done** | `3a4c51f` |

## What shipped (matches your proposal)

**Request 1.** `ScheduleSpec.WithInput([]byte)`; a nullable `schedules.input` column with the idempotent
`ALTER TABLE ... ADD COLUMN` migration; validation **at `CreateSchedule`** that a non-nil input is a JSON
object (a malformed payload is refused there with `ErrSchedule`, never left to fail at every future fire);
the fire re-reads `input` inside its single IMMEDIATE txn and passes it to `enqueueRunLocked`, copied
verbatim into `work_queue.input`.

- **Run provenance is intentional and documented.** As you asked: because the bytes are copied *at each
  fire*, a fired run durably records the parameters it ran with; re-registering with different input does
  not rewrite history. This is stated in `WithInput`'s godoc.
- **`seedInput` fidelity note:** not a defect (agreed). We added the one-line discoverability pointer you
  suggested, on `WithInput`'s godoc — a JSON number seeds as `float64` (not `GetInt`-readable), so prefer
  scalar strings a node decodes itself.

**Request 2.** `Observability.ListSchedules() ([]ScheduleInfo, error)` — one atomic
`SELECT ... ORDER BY next_fire_time`, mp-gated like the rest of the interface, empty store → empty non-nil
slice, paused schedules included. `ScheduleInfo` carries the fields you specified (incl. `Input`).

## Decisions on your open questions

- **`UpdateScheduleInput`: not added.** Delete-then-create is the model for now (as you said you're fine
  with). Keeping the frozen surface minimal; it's a clean additive follow-up if a real need appears.
- **Schedules in `QueueSnapshot`: not added** (you don't need it).

## One heads-up (affects your CR's citations)

`ScheduleSpec.WithCatchupOnce()` — which your CR cited as the builder-style precedent — was **removed**
just before this work (`b383781`, AUD-067). It was a `RESERVED:` no-op that recorded a `'catchup'` policy
the engine never acted on, so it was cut rather than frozen into 1.0. Consequences for you: `WithInput`
is now the sole `ScheduleSpec` builder (the pattern is unchanged), and `ScheduleInfo.MissedPolicy` is
always `"skip"` today (the column is a reserved slot for a future additive catch-up policy). Nothing you
asked for depends on the removed method.

## How to consume before the tag

The features are on `main` now. To use them before `v0.23.0-alpha` ships:

```
go get github.com/ppcavalcante/flow-orchestrator@3a4c51f
```

(or wait for the `v0.23.0-alpha` tag). No public signatures changed; `WithInput`/`ListSchedules`/
`ScheduleInfo` are new. A new binary migrates an old DB on open; an old binary reading a new DB is fine
(explicit column-list SELECTs never see `input`) — it would just fire schedules without their parameters,
a downgrade hazard worth a release-notes line, not a crash.

Each request landed with the exact test set your CR listed (7 for input, 6 for list), plus the full
`pkg/workflow` suite green.
