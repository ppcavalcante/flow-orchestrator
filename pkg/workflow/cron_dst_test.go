package workflow

// M20 ph99 T1 — the DST SPIKE, landed as a committed test (red-team MAJOR-2a). Before writing the cron parser
// we VERIFY (not assume) what Go's in-location `time` arithmetic does on a real America/New_York transition day,
// and pin those answers as the DST hard bar the parser's next() is written to. The findings:
//
//  1. SPRING-FORWARD (Mar 10 2024, 02:00→03:00 — the 02:00–02:59 wall hour does not exist): a real-minute scan's
//     wall clock jumps 01:59 EST → 03:00 EDT; it NEVER reads 02:xx. So a spec at the skipped hour (e.g. "30 2")
//     is simply SKIPPED that day and fires on the next day it exists — no phantom time, no panic.
//     time.Date(2024,3,10,2,30,…) itself NORMALIZES the non-existent 02:30 to 01:30 EST (reads back .Hour()==1).
//
//  2. FALL-BACK (Nov 3 2024, 02:00→01:00 — the 01:00–01:59 wall hour occurs TWICE, first EDT then EST): a spec at
//     the doubled hour (e.g. "30 1") must fire EXACTLY ONCE. The canonical-reconstruct next() (iterate calendar
//     minutes, match on the calendar fields, return time.Date(fields) guarded by .After(after)) fires ONCE at the
//     FIRST occurrence (01:30 EDT) and its next() jumps to the FOLLOWING DAY (25h later, spanning the fall-back).
//     A naive real-instant scan that returns the incremented instant would DOUBLE-FIRE (01:30 EDT then 01:30 EST,
//     1h apart) — that is the bug the reconstruct strategy avoids, and the reason next() reconstructs via
//     time.Date rather than returning the scanned real instant. (DEC-P99-TZ.)
//
// The parser's next() (T3) uses the reconstruct-canonical strategy proven here. These tests are the DST hard bar:
// if a future Go changes this normalization, THEY red first.

import (
	"testing"
	"time"
	_ "time/tzdata" // embed the IANA tz database so LoadLocation("America/New_York") is hermetic (no host tzdata dep)

	"github.com/stretchr/testify/require"
)

// nyLoc loads America/New_York (hermetic via the embedded tzdata import).
func nyLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	require.NoError(t, err, "embedded tzdata must provide America/New_York")
	return loc
}

// dstScanNext is the SPIKE's model of the reconstruct-canonical next() the parser will implement (T3): iterate
// real minutes forward from `after`, match on the WALL-CLOCK fields, and return time.Date(matched fields) guarded
// strictly After(after). It fires once per wall-clock occurrence (fall-back single-fire) and skips non-existent
// wall times (spring-forward). Reduced here to an (hour,minute) daily spec — enough to pin the DST semantics.
func dstScanNext(after time.Time, loc *time.Location, specH, specM int) time.Time {
	t := after.In(loc).Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < 60*24*370; i++ { // ~1yr of minutes, ample for a daily spec
		y, mo, d := t.Date()
		h, mi, _ := t.Clock()
		if h == specH && mi == specM {
			c := time.Date(y, mo, d, h, mi, 0, 0, loc)
			if c.After(after) {
				return c
			}
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}
}

// TestDSTSpike_SpringForward_SkipsPhantomHour — the verified spring-forward semantics: a spec at the SKIPPED wall
// hour (02:30 on Mar 10 2024) does NOT fire that day; it fires the next day at 02:30 EDT. And time.Date normalizes
// the non-existent 02:30 back to 01:30 EST.
func TestDSTSpike_SpringForward_SkipsPhantomHour(t *testing.T) {
	ny := nyLoc(t)

	// time.Date normalization of the non-existent 02:30.
	phantom := time.Date(2024, 3, 10, 2, 30, 0, 0, ny)
	require.Equal(t, 1, phantom.Hour(), "the non-existent 02:30 normalizes to 01:30 (hour 1)")
	require.Equal(t, "EST", zoneName(phantom), "normalized into the pre-transition offset")

	// A "30 2 * * *" spec skips Mar 10 (02:30 doesn't exist) and fires Mar 11 02:30 EDT.
	got := dstScanNext(time.Date(2024, 3, 10, 0, 0, 0, 0, ny), ny, 2, 30)
	want := time.Date(2024, 3, 11, 2, 30, 0, 0, ny)
	require.Equal(t, want.Unix(), got.Unix(), "spec 02:30 skips the spring-forward day, fires next day")
	require.Equal(t, "EDT", zoneName(got), "fires in the post-transition offset")

	// A spec at 01:30 (which DOES exist, once, on spring day) fires that day.
	got0130 := dstScanNext(time.Date(2024, 3, 10, 0, 0, 0, 0, ny), ny, 1, 30)
	require.Equal(t, time.Date(2024, 3, 10, 1, 30, 0, 0, ny).Unix(), got0130.Unix(), "01:30 exists once on spring day")
	require.Equal(t, "EST", zoneName(got0130))
}

// TestDSTSpike_FallBack_FiresOnceNoDoubleFire — the verified fall-back semantics + the double-fire trap: a spec at
// the DOUBLED wall hour (01:30 on Nov 3 2024) fires EXACTLY ONCE, at the FIRST occurrence (EDT); the following
// next() jumps to the next day (25h later, spanning the repeated hour), NOT the second 01:30 EST occurrence.
func TestDSTSpike_FallBack_FiresOnceNoDoubleFire(t *testing.T) {
	ny := nyLoc(t)

	fire1 := dstScanNext(time.Date(2024, 11, 3, 0, 0, 0, 0, ny), ny, 1, 30)
	require.Equal(t, time.Date(2024, 11, 3, 1, 30, 0, 0, ny).Unix(), fire1.Unix(), "fires at the FIRST 01:30 (EDT)")
	require.Equal(t, "EDT", zoneName(fire1), "the first occurrence is EDT (pre-fall-back offset)")

	// The CRITICAL no-double-fire assertion: next() after fire1 must NOT be the second 01:30 (EST, +1h) — the
	// reconstruct-canonical strategy jumps to the NEXT DAY. A real-instant scan would double-fire here.
	fire2 := dstScanNext(fire1, ny, 1, 30)
	require.Equal(t, time.Date(2024, 11, 4, 1, 30, 0, 0, ny).Unix(), fire2.Unix(), "next fire is the FOLLOWING day, not the doubled hour")
	require.InDelta(t, 25.0, fire2.Sub(fire1).Hours(), 0.001, "the gap spans the fall-back (25h), proving no double-fire")

	// Guard the trap explicitly: the second 01:30 EST instant is NOT a fire.
	secondOccurrence := fire1.Add(time.Hour) // 01:30 EDT + 1h = 01:30 EST (the doubled instant)
	require.Equal(t, "EST", zoneName(secondOccurrence), "the +1h instant is the second (EST) 01:30")
	require.NotEqual(t, secondOccurrence.Unix(), fire2.Unix(), "the second 01:30 (EST) is NOT fired — single-fire")
}

// zoneName returns the abbreviated zone name (EST/EDT) of an instant.
func zoneName(t time.Time) string {
	n, _ := t.Zone()
	return n
}
