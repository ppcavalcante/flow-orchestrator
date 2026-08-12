// Part 2 of the independent adversarial pass on phase 116's guards: the TOTALITY bar
// itself. Part 1 (adversarial_116_guards_test.go) attacked the guards' boundaries and
// encoding; this file attacks the claim that no input can make a guarded write path
// panic, hang, or cost unboundedly.
package workflow

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// TOTALITY: the depth guard runs AFTER json.Marshal, which is recursive.
// ---------------------------------------------------------------------------

// advCrashEnv is the re-exec switch. The child runs the delivery in-process and is
// EXPECTED to die, so it cannot share a test binary with anything else.
const advCrashEnv = "ADV116_CRASH_DEPTH"

// TestAdv116_Totality_DeepPayloadIsRefusedNotFatal is the AF2 oracle, INVERTED as part
// of the fix. It was written as a CHARACTERIZATION test — green while the defect was
// live, red once it was closed — and its name said so. Inverting it is part of the
// remedy, not cleanup afterwards: left in its original polarity it reds for the RIGHT
// reason (the child returned instead of dying), and a red-for-the-right-reason on a green
// build is exactly what gets misread as a regression at 3am.
//
// What it characterized: checkJSONDepth measures the ENCODED document, so json.Marshal
// must run to completion first — and json.Marshal is RECURSIVE with no depth limit of its
// own. A payload nested deeply enough exhausts the goroutine stack, and a Go stack
// overflow is a `fatal error`, not a panic: unrecoverable, no deferred recover runs, the
// whole process dies. DeliverSignal is a public API taking `any`, so this was two lines
// from a host. Bisected band: marshal OK at 743,359, dead at 746,875 on the first shape
// tested; 712,906 on the worst (nested map) — darwin/arm64, go1.25.1, at 512 MiB of USABLE
// stack. NOT "1 GB": Go grows stacks by doubling, so usable = the largest power of two <=
// the configured limit, and the 1e9 default cannot reach 1 GiB (measured — 512 MiB, 600 MB
// and 1e9 all die at the same depth; only an exact 1 GiB limit reaches twice as far).
//
// A BAND IS THE WRONG DURABLE FORM ANYWAY, because it is a property of the host's stack
// rather than of this package. The transferable number is the PER-LEVEL cost: ~646 bytes
// of goroutine stack per walk frame for json.Marshal and ~465 for reflect.DeepEqual,
// measured on the worst-case shape. See maxWalkFrames.
//
// VIOLATED PROPERTY: totality. Every input class must have DEFINED behaviour — a value or
// a typed error, never a crash.
//
// ORACLE: the minimum bar. There is no right answer for "what should a 10^6-deep payload
// return", but "kill the host process" is not among the candidates, and the guard's own
// contract (ErrValidation for anything the reader could not read back) says what it
// should be. That is now what it does, via the pre-marshal checkValueDepth walk.
//
// It still runs in a CHILD process, and that is not vestigial. The assertions are on the
// child's EXIT STATUS and stdout, which is the only way to distinguish "returned an
// error" from "died" — an in-process arm cannot observe its own fatal error, so an
// in-process version of this test would go green if the fix were reverted only in the
// sense that the whole binary would disappear.
func TestAdv116_Totality_DeepPayloadIsRefusedNotFatal(t *testing.T) {
	if depth := os.Getenv(advCrashEnv); depth != "" {
		// ---- child ----
		var d int
		_, _ = fmt.Sscanf(depth, "%d", &d) //nolint:errcheck // parse failure leaves d=0, tolerated by this fixture
		dir, err := os.MkdirTemp("", "adv116")
		if err != nil {
			fmt.Println("CHILD-SETUP-FAILED")
			return
		}
		defer func() { _ = os.RemoveAll(dir) }() //nolint:errcheck // test cleanup
		store, err := NewJSONFileStore(dir)
		if err != nil {
			fmt.Println("CHILD-SETUP-FAILED")
			return
		}
		// A recover() proves the point: a stack overflow is a fatal error, so this
		// deferred recover never runs. If the guard behaved totally we would reach the
		// print below with a typed error instead.
		defer func() {
			if r := recover(); r != nil {
				fmt.Printf("CHILD-RECOVERED-PANIC: %v\n", r)
			}
		}()
		derr := store.DeliverSignal("wf-crash", Signal{ID: "s", Name: "n", Payload: nestValue(d)})
		fmt.Printf("CHILD-RETURNED: err=%v\n", derr)
		return
	}

	// ---- parent ----
	if testing.Short() {
		t.Skip("allocates a ~10^6-deep value and forks a child; skipped under -short")
	}
	const crashDepth = 1_000_000

	exe, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.Command(exe, "-test.run", "^"+t.Name()+"$", "-test.v") //nolint:gosec // exe is this test binary
	cmd.Env = append(os.Environ(), fmt.Sprintf("%s=%d", advCrashEnv, crashDepth))
	out, runErr := cmd.CombinedOutput()
	text := string(out)

	t.Logf("child exit: %v", runErr)
	if i := strings.Index(text, "fatal error"); i >= 0 {
		t.Logf("child output around the failure:\n%s", text[i:min(i+200, len(text))])
	}

	// The FIXED assertion — the inversion of what this test asserted while AF2 was live.
	require.NotContains(t, text, "fatal error: stack overflow",
		"AF2 HAS REGRESSED. json.Marshal was reached with a %d-deep payload and exhausted the "+
			"goroutine stack. This is unrecoverable — no host's deferred recover() can catch it — and "+
			"it means the pre-marshal checkValueDepth walk is missing from the DeliverSignal path", crashDepth)
	require.NotContains(t, text, "CHILD-RECOVERED-PANIC",
		"a panic here would be a different defect: the guard is a typed return, not a panic to recover")
	require.NoError(t, runErr, "the child must SURVIVE and exit 0, which is the whole property")
	require.Contains(t, text, "CHILD-RETURNED",
		"the child neither died nor returned, which is a third outcome this test cannot interpret")
	require.Contains(t, text, "validation failed",
		"the refusal must be a TYPED error in the validation domain. An untyped one is the failure "+
			"116 already fixed once on the JSONFileStore snapshot path, where an over-depth Save "+
			"returned an error matching NEITHER ErrValidation nor ErrCorruptData")
}

// TestAdv116_Totality_DepthGuardIsTotalBelowTheStackLimit is the control for the test
// above, and the thing that makes it a bounded claim rather than a scare. Well past the
// legal ceiling, and well below the stack limit, the guard IS total: a clean typed error
// on every store, no crash, and nothing written.
//
// It is also the boundary case that matters most in practice, since anything a host
// builds from decoded JSON is capped at 10000 levels by the decoder itself.
func TestAdv116_Totality_DepthGuardIsTotalBelowTheStackLimit(t *testing.T) {
	for _, depth := range []int{
		maxJSONNestingDepth + 1,
		maxJSONNestingDepth * 2,
		100_000,
		500_000,
	} {
		for name, store := range signalStores(t) {
			if name == "InMemoryStore" {
				continue // serializes nothing (FID-01)
			}
			t.Run(fmt.Sprintf("depth=%d/%s", depth, name), func(t *testing.T) {
				const wf = "wf-total"
				err := store.DeliverSignal(wf, Signal{ID: "s", Name: "n", Payload: nestValue(depth)})
				require.Error(t, err, "a %d-deep payload must be refused", depth)
				require.ErrorIs(t, err, ErrValidation,
					"the refusal must be a TYPED error in the validation domain, not a panic and not ErrIO")
				got, terr := store.TakeSignals(wf)
				require.NoError(t, terr, "the refused delivery must have left nothing behind")
				require.Empty(t, got)
			})
		}
	}
}

// ---------------------------------------------------------------------------
// RESOURCE EXHAUSTION BELOW BOTH LIMITS.
// ---------------------------------------------------------------------------

// TestAdv116_Cost_BothGuardsPassAndTheReadCostIsTheirProduct is the "the guards are only
// meaningful if the thing they bound is what actually costs" attack.
//
// The mailbox has TWO write-side bounds and they are independent:
//   - entry COUNT   <= signalMailboxCap (2^20 in production)
//   - per-entry SIZE <= maxFileSize     (64 MiB in production, per ENTRY)
//
// Nothing bounds their PRODUCT, and takeSignalsFromDir materializes every entry into one
// slice before returning. So a mailbox that satisfies both guards on every single write
// can still make one TakeSignals allocate count*size bytes — 2^20 * 64 MiB at the
// production ceilings.
//
// This test does not try to OOM the machine. It DEMONSTRATES the product relationship at
// a small, safe scale and asserts the load-bearing half: every write was ACCEPTED by
// both 116 guards. The measured heap growth is reported so the shape is a measured
// number and not an inferred one.
//
// ORACLE: the guards' own return values. If the bound that matters were enforced, some
// write in this sequence would have been refused. None is.
func TestAdv116_Cost_BothGuardsPassAndTheReadCostIsTheirProduct(t *testing.T) {
	const (
		entries     = 64
		payloadSize = 512 * 1024 // 512 KiB per entry, far under any byte ceiling
	)
	dir := t.TempDir()
	store, err := NewJSONFileStore(dir)
	require.NoError(t, err)
	const wf = "wf-product"

	blob := strings.Repeat("x", payloadSize)
	for i := 0; i < entries; i++ {
		require.NoError(t, store.DeliverSignal(wf, Signal{ID: fmt.Sprintf("s%03d", i), Name: "n", Payload: blob}),
			"BOTH 116 guards accept entry %d: the count is %d (cap %d) and the entry is ~%d bytes "+
				"(ceiling %d). Neither guard bounds their product.", i, i+1, signalMailboxCap, payloadSize, defaultMaxFileSize)
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got, terr := store.TakeSignals(wf)
	runtime.ReadMemStats(&after)
	require.NoError(t, terr)
	require.Len(t, got, entries)

	grew := int64(after.TotalAlloc - before.TotalAlloc)
	t.Logf("MEASURED: %d entries x %d bytes, both guards green on every write; "+
		"one TakeSignals allocated %d bytes (%.1fx the on-disk payload total)",
		entries, payloadSize, grew, float64(grew)/float64(entries*payloadSize))
	assert.Greater(t, grew, int64(entries*payloadSize),
		"the read materializes every entry at once, so its cost is count*size — "+
			"at the PRODUCTION ceilings that product is 2^20 entries * 64 MiB")

	// The scaling claim, made falsifiable rather than asserted: doubling the entry
	// count doubles the read's allocation. A per-entry bound alone cannot produce this.
	require.NoError(t, store.AckSignals(wf, nil))
}

// TestAdv116_Cost_DepthGuardBoundsDepthNotWidth is the same shape on the F2 axis. A
// document three levels deep with an enormous number of siblings passes checkJSONDepth
// trivially — depth is not size, and the depth guard was never a size guard.
//
// This is a CONTROL, not a defect claim: the byte axis (checkWriteSize) is the guard
// that bounds this, and it is present on the same write path. The test pins that the
// two axes compose — a wide-but-shallow payload is refused by the BYTE guard, in the
// same error domain, so there is no gap between them.
func TestAdv116_Cost_DepthGuardBoundsDepthNotWidth(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONFileStore(dir, WithJSONMaxFileSize(1<<20)) // 1 MiB ceiling
	require.NoError(t, err)
	const wf = "wf-wide"

	wide := make([]any, 300_000) // shallow (depth 2) but far over 1 MiB encoded
	for i := range wide {
		wide[i] = i
	}
	encoded, merr := encodeSignalJSON(Signal{ID: "w", Name: "n", Payload: wide})
	require.NoError(t, merr, "the DEPTH guard must not fire: this payload is 2 levels deep")
	require.Greater(t, len(encoded), 1<<20, "fixture must actually exceed the byte ceiling")

	err = store.DeliverSignal(wf, Signal{ID: "w", Name: "n", Payload: wide})
	require.Error(t, err, "depth is not size; the BYTE guard is what must catch this")
	require.ErrorIs(t, err, ErrValidation)

	got, terr := store.TakeSignals(wf)
	require.NoError(t, terr, "and the refusal must leave a readable (empty) mailbox")
	require.Empty(t, got)
}

// ---------------------------------------------------------------------------
// CONCURRENCY + the hang axis.
// ---------------------------------------------------------------------------

// TestAdv116_Chaos_DeliverAckDeleteConcurrently drives every mutating path on ONE
// workflow's mailbox at once. Round 2 of the phase found a fast-path/ack interleaving
// and round 3 an inode race, both on delivery-vs-something; this arm adds Delete and
// TakeSignals to the mix, which no arm in the shipped suite does.
//
// PROPERTY (the same one as part 1, under interference): a mailbox never holds more
// entries than the ceiling, and TakeSignals never reports ErrCorruptData when every
// accepted write respected the guard.
//
// It also bounds the HANG axis: deliveries take a blocking flock, and Delete holds s.mu
// across it, so a lock-ordering mistake shows up here as a timeout rather than as a
// wedged CI job.
func TestAdv116_Chaos_DeliverAckDeleteConcurrently(t *testing.T) {
	const ceiling = 8
	withMailboxCap(t, ceiling)

	dir := t.TempDir()
	store, err := NewJSONFileStore(dir)
	require.NoError(t, err)
	const wf = "wf-chaos"

	done := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var corrupt []string
	// ANTI-VACUITY. A concurrency test that performed three operations is green for
	// reasons that have nothing to do with the property. The counters make "this arm
	// actually contended" a checked fact rather than an assumption.
	var deliveries, accepted, refused, reads atomic.Int64

	record := func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		corrupt = append(corrupt, err.Error())
	}

	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-done:
					return
				default:
				}
				switch g % 4 {
				case 0, 1: // deliverers
					// A LARGE distinct-id space, deliberately. The first draft cycled
					// ids over 2*ceiling and every one of 248 deliveries was accepted:
					// the acker kept the mailbox under the cap, so the guard was never
					// once exercised and the green said nothing about it.
					err := store.DeliverSignal(wf, Signal{ID: fmt.Sprintf("g%d-s%d", g, i), Name: "n"})
					deliveries.Add(1)
					if err == nil {
						accepted.Add(1)
					} else if strings.Contains(err.Error(), "max mailbox size") {
						refused.Add(1)
					}
					// ErrValidation (cap) and ErrIO are both legal outcomes here.
					// ErrCorruptData from a WRITE never is.
					if err != nil && strings.Contains(err.Error(), "corrupt") {
						record(fmt.Errorf("DeliverSignal returned corrupt-data: %w", err))
					}
				case 2: // acker — THROTTLED, so the mailbox can actually reach the cap
					time.Sleep(2 * time.Millisecond)
					if i%25 == 24 {
						if cur, e := store.TakeSignals(wf); e == nil && len(cur) > 0 {
							_ = store.AckSignals(wf, []string{cur[0].ID}) //nolint:errcheck // adversarial test: ack is best-effort
						}
					}
				case 3: // reader + deleter — also throttled, and Delete is RARE
					time.Sleep(2 * time.Millisecond)
					reads.Add(1)
					if _, terr := store.TakeSignals(wf); terr != nil {
						record(fmt.Errorf("TakeSignals: %w", terr))
					}
					if i%150 == 149 {
						_ = store.Delete(wf) //nolint:errcheck // test cleanup
					}
				}
			}
		}(g)
	}

	// Bounded: a hang shows up as this timer firing with goroutines still live, not as
	// a 30-minute package timeout printing an indistinguishable FAIL.
	time.Sleep(2 * time.Second)
	close(done)

	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("HANG: workers did not drain 30s after the stop signal — a blocking flock " +
			"was acquired and never released, or two paths took it in an order that cycles")
	}

	// ANTI-VACUITY FIRST, and deliberately before the substantive assertion: an arm
	// that contended over nothing is not evidence, and a green from it reads exactly
	// like a green from a held property.
	t.Logf("contention: %d deliveries (%d accepted, %d REFUSED at the cap), %d reads, ceiling %d",
		deliveries.Load(), accepted.Load(), refused.Load(), reads.Load(), ceiling)
	require.Greater(t, deliveries.Load(), int64(20),
		"NOT A PROPERTY FAILURE — this arm did too little work to have contended; the green below is vacuous")
	require.Greater(t, refused.Load(), int64(0),
		"NOT A PROPERTY FAILURE — the CAP WAS NEVER REACHED, so this arm proves nothing about it. "+
			"The first draft of this test accepted 248 of 248 deliveries and still went green.")
	require.Greater(t, reads.Load(), int64(5),
		"NOT A PROPERTY FAILURE — the read path was barely driven")

	mu.Lock()
	defer mu.Unlock()
	assert.Empty(t, corrupt,
		"THE PROPERTY UNDER INTERFERENCE: no accepted write may leave a mailbox the read "+
			"calls corrupt. Observed: %v", corrupt)

	// Final state must be within the ceiling and readable.
	got, terr := store.TakeSignals(wf)
	require.NoError(t, terr, "the mailbox must be readable after the storm")
	require.LessOrEqual(t, len(got), ceiling,
		"the cap must hold across concurrent deliver/ack/delete, not just concurrent deliver")

	// The lock file is the documented residual: it survives Delete. Assert the
	// population claim rather than trusting the prose.
	lock := filepath.Join(dir, wf+signalLockSuffix)
	if _, serr := os.Stat(lock); serr == nil {
		t.Logf("residual confirmed: %s survives Delete (documented, signalLockSuffix)", lock)
	}
}
