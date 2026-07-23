package workflow

// M20 ph99 — a hand-rolled, dependency-free 5-field cron parser + next-fire computation (SCHED-01,
// DEC-M20-CRON). Pure computation: no store, no dispatch, no I/O, no wall-clock read (next() takes an explicit
// reference time so ph100's poller supplies "now" through its Clock seam). The zero-new-dependency moat holds by
// construction — this file uses only the stdlib.
//
// Grammar (DEC-P99-FIELDS): five space-separated fields — minute(0-59) hour(0-23) day-of-month(1-31)
// month(1-12) day-of-week(0-6, 0=Sun). Operators per field: `*` (any), `a-b` (range), `*/n` (step over the whole
// range), `a-b/n` (step over a sub-range), and comma lists `x,y,z` of any of those. NOT supported (out per
// DEC-M20-CRON): `L`, `W`, `#`, `@named`, seconds, and 7=Sun (only 0=Sun).
//
// DST + timezone (DEC-P99-TZ), fixed by the ph99 T1 spike (cron_dst_test.go): next() evaluates in the reference
// time's *time.Location and reconstructs each candidate via time.Date(calendar-fields) — so a spring-forward
// skipped wall hour never fires (its wall minutes don't exist) and a fall-back doubled wall hour fires exactly
// ONCE (the .After(after) guard + calendar reconstruction dedupe the repeated hour). See that test for the proof.

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// fieldSpec is one parsed cron field: a bitset of the allowed values (bit i set ⇔ value i matches) plus whether
// the field was RESTRICTED (anything other than `*`). `restricted` is load-bearing only for dom/dow (the OR-rule,
// DEC-P99-DOM-DOW-OR); for the other fields it is informational.
type fieldSpec struct {
	bits       uint64 // bit i set ⇔ value i is allowed (values fit in [0,63] for every cron field)
	restricted bool   // true unless the field was `*` (or `*/1`, which is equivalent to `*` → NOT restricted)
}

func (f fieldSpec) matches(v int) bool {
	if v < 0 || v > 63 {
		return false
	}
	return f.bits&(1<<uint(v)) != 0
}

// Schedule is a parsed 5-field cron spec. Immutable after Parse; next() is a pure function of (Schedule, time).
type Schedule struct {
	spec   string    // the original spec (for String()/errors)
	minute fieldSpec // 0-59
	hour   fieldSpec // 0-23
	dom    fieldSpec // 1-31
	month  fieldSpec // 1-12
	dow    fieldSpec // 0-6, 0=Sun
}

// String returns the original spec string.
func (s *Schedule) String() string { return s.spec }

// ErrCronSpec is the typed parse-error sentinel (a malformed spec never panics — DEC-P99 robustness). Callers
// distinguish it with errors.Is(err, ErrCronSpec).
var ErrCronSpec = fmt.Errorf("%w: invalid cron spec", ErrValidation)

// ErrCronNoFire is returned by Next when no matching instant exists within the bounded search horizon — an
// impossible spec (e.g. "0 0 30 2 *", Feb 30) yields this instead of hanging (DEC-P99-BOUNDED-SEARCH).
var ErrCronNoFire = fmt.Errorf("%w: cron spec has no fire time within the search horizon", ErrValidation)

// cronField describes a field's bounds for parsing.
type cronField struct {
	name string
	min  int
	max  int
}

var cronFields = [5]cronField{
	{"minute", 0, 59},
	{"hour", 0, 23},
	{"day-of-month", 1, 31},
	{"month", 1, 12},
	{"day-of-week", 0, 6},
}

// ParseCron parses a standard 5-field cron spec into a Schedule (DEC-P99-FIELDS). It NEVER panics; any malformed
// input returns a typed error wrapping ErrCronSpec. Fields are space-separated (runs of whitespace collapse).
func ParseCron(spec string) (*Schedule, error) {
	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return nil, fmt.Errorf("%w: expected 5 fields, got %d in %q", ErrCronSpec, len(fields), spec)
	}
	parsed := make([]fieldSpec, 5)
	for i, raw := range fields {
		fs, err := parseField(raw, cronFields[i])
		if err != nil {
			return nil, err
		}
		parsed[i] = fs
	}
	return &Schedule{
		spec:   spec,
		minute: parsed[0],
		hour:   parsed[1],
		dom:    parsed[2],
		month:  parsed[3],
		dow:    parsed[4],
	}, nil
}

// parseField parses one field (a comma list of `*`, `a`, `a-b`, `*/n`, `a-b/n`) into a bitset. A field is
// RESTRICTED unless it is exactly `*` (or `*/1`). Any out-of-range / malformed token → typed ErrCronSpec.
func parseField(raw string, cf cronField) (fieldSpec, error) {
	if raw == "" {
		return fieldSpec{}, fmt.Errorf("%w: empty %s field", ErrCronSpec, cf.name)
	}
	var fs fieldSpec
	restricted := false
	for _, term := range strings.Split(raw, ",") {
		if term == "" {
			return fieldSpec{}, fmt.Errorf("%w: empty term in %s field %q", ErrCronSpec, cf.name, raw)
		}
		lo, hi, step, termRestricted, err := parseTerm(term, cf)
		if err != nil {
			return fieldSpec{}, err
		}
		if termRestricted {
			restricted = true
		}
		for v := lo; v <= hi; v += step {
			fs.bits |= 1 << uint(v)
		}
	}
	fs.restricted = restricted
	return fs, nil
}

// parseTerm parses a single comma-term into (lo, hi, step, restricted). `*`→full range step 1 (not restricted);
// `*/n`→full range step n (restricted iff n>1); `a`→[a,a] step 1 (restricted); `a-b`→[a,b] step 1 (restricted);
// `a-b/n`→[a,b] step n (restricted).
func parseTerm(term string, cf cronField) (lo, hi, step int, restricted bool, err error) {
	rangePart := term
	step = 1
	hasStep := false
	if slash := strings.IndexByte(term, '/'); slash >= 0 {
		hasStep = true
		rangePart = term[:slash]
		stepStr := term[slash+1:]
		step, err = strconv.Atoi(stepStr)
		if err != nil || step <= 0 {
			return 0, 0, 0, false, fmt.Errorf("%w: bad step %q in %s field", ErrCronSpec, stepStr, cf.name)
		}
	}

	switch {
	case rangePart == "*":
		lo, hi = cf.min, cf.max
		// `*` is unrestricted; `*/n` (n>1) narrows the set → restricted.
		restricted = step > 1
	case strings.IndexByte(rangePart, '-') >= 0:
		dash := strings.IndexByte(rangePart, '-')
		lo, err = parseIntInRange(rangePart[:dash], cf)
		if err != nil {
			return 0, 0, 0, false, err
		}
		hi, err = parseIntInRange(rangePart[dash+1:], cf)
		if err != nil {
			return 0, 0, 0, false, err
		}
		if lo > hi {
			return 0, 0, 0, false, fmt.Errorf("%w: range %q is descending in %s field", ErrCronSpec, rangePart, cf.name)
		}
		restricted = true
	default:
		// A bare single value `a`. A `/step` on a bare value (`a/n`) is OUT OF GRAMMAR (DEC-P99-FIELDS allows
		// `*/n` and `a-b/n`, not `a/n`) — REJECT it rather than silently treating it as {a} (review ph99-F2:
		// standard cron reads `5/10` as `5-max/10`; accepting it as {5} is a silent-wrong schedule). A bare
		// value without a step is the ordinary fixed-value case.
		if hasStep {
			return 0, 0, 0, false, fmt.Errorf("%w: %q — a step requires a range or `*` (write `%s-%d/%d`) in %s field",
				ErrCronSpec, term, rangePart, cf.max, step, cf.name)
		}
		lo, err = parseIntInRange(rangePart, cf)
		if err != nil {
			return 0, 0, 0, false, err
		}
		hi = lo
		restricted = true
	}
	return lo, hi, step, restricted, nil
}

// parseIntInRange parses a decimal int and validates it against the field's [min,max]. A non-numeric or
// out-of-range value → typed ErrCronSpec (never a panic).
func parseIntInRange(s string, cf cronField) (int, error) {
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%w: %q is not a valid %s value", ErrCronSpec, s, cf.name)
	}
	if v < cf.min || v > cf.max {
		return 0, fmt.Errorf("%w: %d out of range [%d,%d] for %s field", ErrCronSpec, v, cf.min, cf.max, cf.name)
	}
	return v, nil
}

// maxCronHorizon bounds the next() forward search (DEC-P99-BOUNDED-SEARCH): a spec with no reachable fire (e.g.
// Feb 30) must terminate with ErrCronNoFire, never hang. The bound must exceed the LARGEST gap between two
// consecutive fires of any VALID spec. That worst case is `29 * * 2 *` (Feb 29), which normally gaps 4 years but
// gaps up to 8 across a non-leap CENTURY boundary (e.g. 2096 leap → 2100 NOT leap → 2104 leap — review ph99-F1).
// So 9 years of minutes covers it with margin (an impossible spec like Feb 30 still exhausts the horizon → the
// typed no-fire error, never a hang). Not "at most once a year" — that earlier claim was false for Feb 29.
const maxCronHorizon = 9 * 366 * 24 * 60 // minutes in ~9 years (covers the 8-year Feb-29 century-boundary gap)

// Next returns the earliest instant strictly after `after` that matches the schedule, evaluated in `after`'s
// *time.Location (DEC-P99-TZ). It reconstructs each candidate via time.Date(calendar-fields) so DST is handled
// per the ph99 T1 spike: a spring-forward skipped wall minute never matches, a fall-back doubled wall hour fires
// exactly once. Returns ErrCronNoFire if no match exists within maxCronHorizon (an impossible spec, never a hang).
func (s *Schedule) Next(after time.Time) (time.Time, error) {
	loc := after.Location()
	// Start at the next whole minute strictly after `after`.
	t := after.Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < maxCronHorizon; i++ {
		y, mo, d := t.Date()
		hh, mm, _ := t.Clock()
		if s.minute.matches(mm) && s.hour.matches(hh) && s.month.matches(int(mo)) && s.dayMatches(t) {
			// Reconstruct the canonical instant for this calendar minute (DST-correct — the T1 spike strategy).
			c := time.Date(y, mo, d, hh, mm, 0, 0, loc)
			if c.After(after) {
				return c, nil
			}
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}, fmt.Errorf("%w: %q after %s", ErrCronNoFire, s.spec, after.Format(time.RFC3339))
}

// dayMatches applies the DOM/DOW OR-rule (DEC-P99-DOM-DOW-OR): if BOTH dom and dow are restricted, a day matches
// if it satisfies dom OR dow (the classic cron union — the #1 correctness gotcha). If exactly one is restricted,
// only that one gates. If neither is restricted (both `*`), every day matches.
func (s *Schedule) dayMatches(t time.Time) bool {
	domOK := s.dom.matches(t.Day())
	dowOK := s.dow.matches(int(t.Weekday())) // time.Weekday: Sunday=0 — matches cron 0=Sun exactly
	switch {
	case s.dom.restricted && s.dow.restricted:
		return domOK || dowOK
	case s.dom.restricted:
		return domOK
	case s.dow.restricted:
		return dowOK
	default:
		return true
	}
}
