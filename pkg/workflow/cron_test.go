package workflow

// M20 ph99 T2/T3 — the cron parser + next() hard bar (pure, table-driven). Parse round-trips the 5-field grammar
// + rejects malformed specs with typed errors (never panics); Next computes the earliest matching instant with
// the DOM-DOW-OR rule, month rollover, step boundaries, end-of-month, the ⭐ DST semantics (spike-fixed in
// cron_dst_test.go), and the impossible-spec → ErrCronNoFire (never hangs). The property + fuzz live at the end.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// mustParse parses a spec that is expected to be valid.
func mustParse(t *testing.T, spec string) *Schedule {
	t.Helper()
	s, err := ParseCron(spec)
	require.NoError(t, err, "spec %q should parse", spec)
	return s
}

// utc builds a UTC reference time.
func utc(y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, time.UTC)
}

// TestParseCron_Valid — the grammar round-trips: *, ranges, steps, lists parse to the expected matching sets.
func TestParseCron_Valid(t *testing.T) {
	// every-minute
	s := mustParse(t, "* * * * *")
	require.True(t, s.minute.matches(0) && s.minute.matches(59))
	require.False(t, s.minute.restricted, "`*` is not restricted")
	require.False(t, s.dom.restricted && s.dow.restricted, "both `*` → neither restricted")

	// fixed value
	s = mustParse(t, "30 2 15 6 3")
	require.True(t, s.minute.matches(30) && !s.minute.matches(31))
	require.True(t, s.hour.matches(2))
	require.True(t, s.dom.matches(15) && s.dom.restricted)
	require.True(t, s.month.matches(6))
	require.True(t, s.dow.matches(3) && s.dow.restricted)

	// range + step + list
	s = mustParse(t, "0-10/2 * * * 1,3,5")
	for _, m := range []int{0, 2, 4, 6, 8, 10} {
		require.True(t, s.minute.matches(m), "step should include %d", m)
	}
	require.False(t, s.minute.matches(1), "odd minute excluded by /2")
	require.True(t, s.dow.matches(1) && s.dow.matches(3) && s.dow.matches(5))
	require.False(t, s.dow.matches(2), "list excludes 2")

	// */n restricts; */1 does not
	s = mustParse(t, "*/15 * * * *")
	require.True(t, s.minute.restricted, "*/15 restricts")
	for _, m := range []int{0, 15, 30, 45} {
		require.True(t, s.minute.matches(m))
	}
	s = mustParse(t, "*/1 * * * *")
	require.False(t, s.minute.restricted, "*/1 ≡ * (not restricted)")
}

// TestParseCron_Malformed — every malformed spec → typed ErrCronSpec, NEVER a panic.
func TestParseCron_Malformed(t *testing.T) {
	bad := []string{
		"",             // empty
		"* * * *",      // 4 fields
		"* * * * * *",  // 6 fields
		"60 * * * *",   // minute out of range
		"* 24 * * *",   // hour out of range
		"* * 0 * *",    // dom below min (1)
		"* * 32 * *",   // dom above max
		"* * * 13 *",   // month above max
		"* * * * 7",    // dow above max (0-6)
		"*/0 * * * *",  // zero step
		"*/x * * * *",  // non-numeric step
		"a * * * *",    // non-numeric value
		"5-3 * * * *",  // descending range
		"1,,2 * * * *", // empty list term
		"1- * * * *",   // malformed range (empty hi)
	}
	for _, spec := range bad {
		_, err := ParseCron(spec)
		require.Error(t, err, "spec %q must error", spec)
		require.ErrorIs(t, err, ErrCronSpec, "spec %q must wrap ErrCronSpec", spec)
	}
}

// TestCronNext_Table — the core hard-bar table: spec × reference → exact expected next fire (all UTC).
func TestCronNext_Table(t *testing.T) {
	cases := []struct {
		name string
		spec string
		from time.Time
		want time.Time
	}{
		{"every-minute", "* * * * *", utc(2024, 1, 1, 0, 0), utc(2024, 1, 1, 0, 1)},
		{"fixed daily", "30 2 * * *", utc(2024, 1, 1, 0, 0), utc(2024, 1, 1, 2, 30)},
		{"fixed daily rolls to next day", "30 2 * * *", utc(2024, 1, 1, 3, 0), utc(2024, 1, 2, 2, 30)},
		{"step */15 boundary", "*/15 * * * *", utc(2024, 1, 1, 0, 7), utc(2024, 1, 1, 0, 15)},
		{"step */15 at boundary → next", "*/15 * * * *", utc(2024, 1, 1, 0, 15), utc(2024, 1, 1, 0, 30)},
		{"month rollover Dec→Jan", "0 0 1 * *", utc(2024, 12, 15, 0, 0), utc(2025, 1, 1, 0, 0)},
		{"end-of-month 31 skips 30-day months", "0 0 31 * *", utc(2024, 4, 1, 0, 0), utc(2024, 5, 31, 0, 0)},
		{"Feb 29 leap year", "0 0 29 2 *", utc(2024, 1, 1, 0, 0), utc(2024, 2, 29, 0, 0)},
		{"range hour", "0 9-17 * * 1", utc(2024, 1, 1, 8, 0), utc(2024, 1, 1, 9, 0)}, // Mon Jan 1 2024
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mustParse(t, tc.spec).Next(tc.from)
			require.NoError(t, err)
			require.Equal(t, tc.want, got, "spec %q from %s", tc.spec, tc.from)
		})
	}
}

// TestCronNext_DomDowOR — DEC-P99-DOM-DOW-OR (the #1 gotcha): when BOTH dom and dow are restricted, a day fires
// if it matches dom OR dow. Spec "0 0 13 * 5" = midnight on the 13th OR any Friday. In a month where the 13th is
// NOT a Friday, BOTH the 13th and every Friday must fire.
func TestCronNext_DomDowOR(t *testing.T) {
	s := mustParse(t, "0 0 13 * 5") // 00:00 on day-13 OR Friday
	// Sep 2024: the 13th is a Friday, but Sep 6 (Fri) comes first → dow arm fires.
	got, err := s.Next(utc(2024, 9, 1, 0, 0))
	require.NoError(t, err)
	require.Equal(t, utc(2024, 9, 6, 0, 0), got, "the OR-rule fires on the first Friday, not waiting for the 13th")

	// From just after Sep 6: next Friday is Sep 13 (also the 13th — both arms agree).
	got, err = s.Next(utc(2024, 9, 6, 0, 1))
	require.NoError(t, err)
	require.Equal(t, utc(2024, 9, 13, 0, 0), got)

	// The dom arm fires the 13th EVEN when it is not a Friday: Oct 13 2024 is a Sunday. From Oct 12 12:00 the
	// next fire is Oct 13 00:00 (via the dom arm) — proving the union includes the 13th independent of dow.
	got13, err := s.Next(utc(2024, 10, 12, 12, 0))
	require.NoError(t, err)
	require.Equal(t, time.Sunday, utc(2024, 10, 13, 0, 0).Weekday(), "Oct 13 2024 is a Sunday (not the dow=5 Friday)")
	require.Equal(t, utc(2024, 10, 13, 0, 0), got13, "the 13th fires via the dom arm even though it is a Sunday, not Friday")
}

// TestCronNext_DomOnly_vs_DowOnly — when exactly ONE of dom/dow is restricted, only that one gates (no OR).
func TestCronNext_DomOnly_vs_DowOnly(t *testing.T) {
	// dom-only: "0 0 15 * *" fires ONLY on the 15th, regardless of weekday.
	dom := mustParse(t, "0 0 15 * *")
	got, err := dom.Next(utc(2024, 1, 1, 0, 0))
	require.NoError(t, err)
	require.Equal(t, utc(2024, 1, 15, 0, 0), got)

	// dow-only: "0 0 * * 0" fires every Sunday. Jan 7 2024 is the first Sunday.
	dow := mustParse(t, "0 0 * * 0")
	got, err = dow.Next(utc(2024, 1, 1, 0, 0))
	require.NoError(t, err)
	require.Equal(t, utc(2024, 1, 7, 0, 0), got, "Sunday-only fires on the first Sunday")
	require.Equal(t, time.Sunday, got.Weekday())
}

// TestCronNext_ImpossibleSpec_NoHang — an impossible spec (Feb 30) returns ErrCronNoFire within the horizon,
// never hanging (DEC-P99-BOUNDED-SEARCH).
func TestCronNext_ImpossibleSpec_NoHang(t *testing.T) {
	s := mustParse(t, "0 0 30 2 *") // Feb 30 — never exists
	_, err := s.Next(utc(2024, 1, 1, 0, 0))
	require.Error(t, err)
	require.ErrorIs(t, err, ErrCronNoFire, "an impossible spec yields the typed no-fire error, not a hang")
}

// TestCronNext_Feb29_CenturyBoundaryGap — review ph99-F1: a valid Feb-29 spec must still fire across the 2100
// non-leap century boundary (2096 leap → 2100 NOT leap → 2104 leap = an 8-year gap). The horizon must cover it,
// so Next returns the real fire, NOT a false ErrCronNoFire. (The old 5-year horizon reddened here.)
func TestCronNext_Feb29_CenturyBoundaryGap(t *testing.T) {
	s := mustParse(t, "0 0 29 2 *")
	// From just after the 2096 leap-day fire, the next Feb 29 is 2104 (2100 is NOT a leap year).
	got, err := s.Next(utc(2096, 3, 1, 0, 0))
	require.NoError(t, err, "a valid Feb-29 spec must fire across the century boundary, not return ErrCronNoFire")
	require.Equal(t, utc(2104, 2, 29, 0, 0), got, "next Feb 29 after 2096 is 2104 (2100 is not leap)")
}

// TestParseCron_BareValueStepRejected — review ph99-F2: `a/n` (a bare value with a step) is OUT OF GRAMMAR and
// must be REJECTED, not silently accepted as {a} (standard cron reads `5/10` as 5-max/10; accepting it as {5} is
// a silent-wrong schedule). `*/n` and `a-b/n` remain valid.
func TestParseCron_BareValueStepRejected(t *testing.T) {
	for _, spec := range []string{"5/10 * * * *", "* 3/2 * * *", "* * 1/5 * *"} {
		_, err := ParseCron(spec)
		require.ErrorIs(t, err, ErrCronSpec, "bare value-with-step %q must be rejected (out of grammar)", spec)
	}
	// The valid step forms still parse.
	_, err := ParseCron("*/10 * * * *")
	require.NoError(t, err, "*/n stays valid")
	_, err = ParseCron("5-55/10 * * * *")
	require.NoError(t, err, "a-b/n stays valid")
}

// TestCronNext_DST_SpringForward — the ⭐ DST hard bar (spike-fixed): a spec at the skipped wall hour is skipped
// that day and fires the next day; a spec at 01:30 (exists once) fires that day. America/New_York.
func TestCronNext_DST_SpringForward(t *testing.T) {
	ny := nyLoc(t)
	// "30 2 * * *" — 02:30 does not exist on Mar 10 2024 → fires Mar 11 02:30 EDT.
	got, err := mustParse(t, "30 2 * * *").Next(time.Date(2024, 3, 10, 0, 0, 0, 0, ny))
	require.NoError(t, err)
	require.Equal(t, time.Date(2024, 3, 11, 2, 30, 0, 0, ny).Unix(), got.Unix(), "skipped spring-forward hour → next day")
	require.Equal(t, "EDT", zoneName(got))
}

// TestCronNext_DST_FallBack_SingleFire — the ⭐ fall-back single-fire: a spec at the doubled wall hour (01:30
// Nov 3 2024) fires ONCE (first occurrence, EDT); the next fire jumps to the following day (25h), NOT the second
// 01:30 EST. This is the double-fire trap the reconstruct-canonical next() avoids.
func TestCronNext_DST_FallBack_SingleFire(t *testing.T) {
	ny := nyLoc(t)
	s := mustParse(t, "30 1 * * *")
	fire1, err := s.Next(time.Date(2024, 11, 3, 0, 0, 0, 0, ny))
	require.NoError(t, err)
	require.Equal(t, time.Date(2024, 11, 3, 1, 30, 0, 0, ny).Unix(), fire1.Unix(), "fires at the FIRST 01:30 (EDT)")
	require.Equal(t, "EDT", zoneName(fire1))

	fire2, err := s.Next(fire1)
	require.NoError(t, err)
	require.Equal(t, time.Date(2024, 11, 4, 1, 30, 0, 0, ny).Unix(), fire2.Unix(), "next fire is the FOLLOWING day — no double-fire")
	require.InDelta(t, 25.0, fire2.Sub(fire1).Hours(), 0.001, "the 25h gap spans the fall-back")
}

// TestCronNext_StrictlyAfter — next(t) is STRICTLY after t (a match AT t returns the NEXT one).
func TestCronNext_StrictlyAfter(t *testing.T) {
	s := mustParse(t, "* * * * *")
	from := utc(2024, 6, 1, 12, 0)
	got, err := s.Next(from)
	require.NoError(t, err)
	require.True(t, got.After(from), "next is strictly after")
	require.Equal(t, from.Add(time.Minute), got)
}
