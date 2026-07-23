package workflow

// M20 ph99 T4 — the cron non-vacuity property + a parse fuzz. The property proves next() is CORRECT-by-scan for
// random valid specs × random reference times: next(t) is strictly after t AND no matching minute exists in the
// open gap (t, next(t)) — a scan of the gap finds nothing. This is the witness that the table tests aren't
// passing vacuously (a next() that returns t+horizon would fail the gap scan; one that returns t would fail
// strictly-after). The fuzz proves Parse never panics on arbitrary bytes.

import (
	"fmt"
	"testing"
	"time"

	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
)

// randSpec builds a valid 5-field spec from bounded seeds — a mix of `*`, a fixed value, and a step, per field,
// so the generated specs exercise real matching sets without ever being malformed.
func randSpec(mSeed, hSeed, domSeed, moSeed, dowSeed int) string {
	field := func(seed, min, max int) string {
		switch seed % 3 {
		case 0:
			return "*"
		case 1:
			return fmt.Sprintf("%d", min+(seed%(max-min+1)))
		default:
			step := 2 + (seed % 4) // 2..5
			return fmt.Sprintf("*/%d", step)
		}
	}
	return field(mSeed, 0, 59) + " " + field(hSeed, 0, 23) + " " + field(domSeed, 1, 31) + " " +
		field(moSeed, 1, 12) + " " + field(dowSeed, 0, 6)
}

// matchesMinute reports whether the schedule matches the wall-clock minute of t (the same predicate Next uses,
// re-expressed for the gap scan — independent of Next's search loop so it is a genuine oracle).
func (s *Schedule) matchesMinute(t time.Time) bool {
	return s.minute.matches(t.Minute()) && s.hour.matches(t.Hour()) &&
		s.month.matches(int(t.Month())) && s.dayMatches(t)
}

// TestCronProperty_NextIsEarliestMatch — the non-vacuity property (DEC-P99 hard bar).
func TestCronProperty_NextIsEarliestMatch(t *testing.T) {
	params := gopter.DefaultTestParameters()
	params.MinSuccessfulTests = 400
	properties := gopter.NewProperties(params)

	properties.Property("next(t) is strictly after t and no match exists in the gap (t, next(t))", prop.ForAll(
		func(mS, hS, domS, moS, dowS int, refUnixMin int64) bool {
			spec := randSpec(mS, hS, domS, moS, dowS)
			sched, err := ParseCron(spec)
			if err != nil {
				return false // randSpec only builds valid specs → a parse error is a generator bug
			}
			// A reference time somewhere in a ~4-year window (2023-2026), UTC, at minute granularity.
			ref := time.Unix(refUnixMin*60, 0).UTC()
			next, err := sched.Next(ref)
			if err != nil {
				// A valid spec might have no fire only if it is genuinely impossible; randSpec never builds Feb-30
				// class specs (each field independent, dom capped 31 but month may be any) — but "31 in a
				// short-only month set" can happen. Accept ErrCronNoFire as a valid outcome ONLY if a full-horizon
				// scan also finds nothing (else it is a real bug).
				return scanFindsNoMatch(sched, ref)
			}
			// (1) strictly after.
			if !next.After(ref) {
				return false
			}
			// (2) next itself matches.
			if !sched.matchesMinute(next) {
				return false
			}
			// (3) NO match in the open gap (ref, next) — scan minute-by-minute.
			for t := ref.Truncate(time.Minute).Add(time.Minute); t.Before(next); t = t.Add(time.Minute) {
				if sched.matchesMinute(t) {
					return false // a nearer match exists → next was not the EARLIEST
				}
			}
			return true
		},
		gen.IntRange(0, 1000),
		gen.IntRange(0, 1000),
		gen.IntRange(0, 1000),
		gen.IntRange(0, 1000),
		gen.IntRange(0, 1000),
		// ~2023-01-01 .. ~2026-12-31 in minutes since epoch.
		gen.Int64Range(27794880, 29892960),
	))

	properties.TestingRun(t)
}

// scanFindsNoMatch confirms a spec has NO match within the horizon (the honest oracle for an ErrCronNoFire).
func scanFindsNoMatch(s *Schedule, after time.Time) bool {
	t := after.Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < maxCronHorizon; i++ {
		if s.matchesMinute(t) {
			return false
		}
		t = t.Add(time.Minute)
	}
	return true
}

// FuzzParseCron — Parse never panics on arbitrary input (the robustness floor). A well-formed OR a malformed spec
// both return cleanly (schedule+nil OR nil+error); the ONLY failure mode this guards is a panic.
func FuzzParseCron(f *testing.F) {
	for _, seed := range []string{
		"* * * * *", "30 2 * * *", "*/15 9-17 * * 1-5", "0 0 30 2 *", "", "garbage",
		"* * * *", "60 * * * *", "*/0 * * * *", "1,2,3 * * * 0",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, spec string) {
		// Must not panic. A returned schedule must be non-nil iff err is nil, and Next on it must also not panic.
		s, err := ParseCron(spec)
		if err == nil {
			if s == nil {
				t.Fatalf("nil schedule with nil error for %q", spec)
			}
			_, _ = s.Next(time.Unix(0, 0).UTC()) //nolint:errcheck // fuzz only asserts no-panic; the error value is irrelevant here
		}
	})
}
