package workflow

// M20 ph98 — concurrency caps (CAP-01..05, DEC-M20-D1/D3/D6). Bound "at most K of type X running at once"
// (per-type) plus an optional global cap G, enforced atomically at claim time as BACKPRESSURE: a candidate
// that would exceed a cap is SKIPPED (left claimable-later), never rejected — an at-cap item is a no-op skip,
// never a failed workflow (CAP-04). The referent is RUNNING slots only: `state='claimed' AND parked IS NULL`
// (DEC-P98-PARKED-COLUMN) — a parked sub-workflow child is claimed-but-not-a-slot, so K parked parents each
// awaiting a child of a capped type never deadlock the cap.
//
// The cap is IMMUTABLE store config (set once via WithCaps at NewSQLiteStore), so ClaimNext reads it inside its
// BEGIN IMMEDIATE txn with no new lock (the fenced-derived-state trap avoided by construction — the arbiter is
// the row-COUNT-in-txn, this map is just static config). "Lowering a cap" (DEC-M20-D7 no-kill) is reopening the
// store/pool with a lower value on the same DB: in-flight `claimed∧¬parked` rows finish; the row-COUNT admits
// nothing new until the count drops below the new cap. No runtime mutation, no reconciler, no counter to desync.

// Caps is the concurrency-cap configuration for a SQLiteStore's ClaimNext (M20 ph98). The zero value is
// UNBOUNDED (no per-type caps, no global cap) — ClaimNext's gate is a no-op and the M17 hot path is
// byte-behavior-unchanged (CAP-05). Copy-safe (a map + an int); the store snapshots it at construction.
type Caps struct {
	// PerType[typ] = the max number of type-`typ` items that may be RUNNING (claimed∧¬parked) at once.
	// A type ABSENT from the map (or mapped to <= 0) is UNBOUNDED for that type. An untyped run (type="")
	// is never governed by a per-type cap (DEC-M20-D6) — only by Global.
	PerType map[string]int
	// Global = the max number of items of ANY type that may be RUNNING (claimed∧¬parked) at once. <= 0 =
	// no global cap. A claim is admitted iff count(type) < PerType[type] (if capped) AND count(*) < Global
	// (if capped) — both gates AND (DEC-M20-D6).
	Global int
}

// capForType returns (limit, capped) for a dispatch type: capped=false ⇒ this type is unbounded (no per-type
// gate). type="" is never per-type-capped (governed by Global only). A <= 0 limit is treated as uncapped.
func (c Caps) capForType(typ string) (limit int, capped bool) {
	if typ == "" || c.PerType == nil {
		return 0, false
	}
	limit, ok := c.PerType[typ]
	if !ok || limit <= 0 {
		return 0, false
	}
	return limit, true
}

// globalCap returns (limit, capped) for the global gate: capped=false ⇒ no global cap.
func (c Caps) globalCap() (limit int, capped bool) {
	if c.Global <= 0 {
		return 0, false
	}
	return c.Global, true
}

// isUnbounded reports whether NO cap is configured (no per-type entries and no global). When true, ClaimNext's
// cap gate is skipped entirely → the M17 claim path is byte-behavior-unchanged (CAP-05 additive moat).
func (c Caps) isUnbounded() bool {
	return len(c.PerType) == 0 && c.Global <= 0
}

// WithCaps sets the concurrency caps for a SQLiteStore (M20 ph98, CAP-01..05). Unset (or a zero-value Caps)
// leaves dispatch UNBOUNDED — ClaimNext's gate is a no-op and the M17 behavior is unchanged. The store copies
// the caps at construction (immutable thereafter): to change a cap, reopen the store/pool with a new value on
// the same DB file (in-flight runs finish, no kill — DEC-M20-D7). The gate counts RUNNING slots only
// (claimed∧¬parked), so a parked sub-workflow child never consumes a slot.
func WithCaps(caps Caps) SQLiteOption {
	return func(d *sqliteDurability) {
		// Defensive copy of the per-type map so a caller mutating their map after construction can't
		// retro-change the store's immutable caps (the immutability the no-lock in-txn read relies on).
		if len(caps.PerType) > 0 {
			cp := make(map[string]int, len(caps.PerType))
			for k, v := range caps.PerType {
				cp[k] = v
			}
			caps.PerType = cp
		}
		d.caps = caps
	}
}
