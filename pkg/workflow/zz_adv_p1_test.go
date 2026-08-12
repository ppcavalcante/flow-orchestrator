package workflow

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

// ============================================================================
// P1 — NO UNDER-REPORT. The load-bearing property.
//
//	checkDeepEqualPairDepth(a,b) == nil  =>  the real reflect.DeepEqual(a,b)
//	                                         descent stays within maxWalkFrames.
//
// Oracle: advPairDepth (zz_adv_oracle_test.go), a calibrated LOWER bound on the
// true peak depth. An accepted pair whose oracle depth exceeds the bound is the
// crash this guard exists to prevent.
// ============================================================================

// advG is deliberately promiscuous: pointer, slice, map, interface, array,
// struct and scalar in one type, so a generated graph exercises every descent
// rule and every memo key kind (and the interface kind deKeyOf refuses to key).
type advG struct {
	A, B *advG
	S    []any
	M    map[string]any
	I    any
	Arr  [2]*advG
	X    int
}

// advGen builds a random object graph of n nodes with controlled sharing and
// cycles. Every field may point at any node, including an ancestor.
//
// 🔴 THE GENERATOR IS ITSELF PART OF THE INSTRUMENT. A first version drew every
// edge uniformly over n<=40 nodes; the deepest pair it ever ACCEPTED was 14
// frames against a bound of 32768, so it searched ~0.04% of the interesting
// range and could not have found an under-report if one existed. It is now
// SPINE-BIASED: edge i -> i+1 dominates, which makes depth ~ n, and n runs past
// the bound. The measured "deepest ACCEPTED pair" line in the test output is
// the vacuity check — if it collapses again, the arm is decorative.
func advGen(rng *rand.Rand, n int, cycleProb float64) *advG {
	if n < 1 {
		n = 1
	}
	ns := make([]*advG, n)
	for i := range ns {
		ns[i] = &advG{X: rng.Intn(3)}
	}
	pick := func(i int) *advG {
		if rng.Float64() < cycleProb {
			return ns[rng.Intn(n)] // may be an ancestor: cycle
		}
		if i+1 >= n {
			return nil
		}
		if rng.Intn(4) > 0 {
			return ns[i+1] // SPINE: the edge that makes the graph deep
		}
		return ns[i+1+rng.Intn(n-i-1)] // a forward jump: sharing, still acyclic
	}
	for i, nd := range ns {
		// A IS THE SPINE and is not left to chance. With `if rng.Intn(3) > 0`
		// the chain broke with probability 1/3 per node, so reachable depth was
		// a dying random walk: the deepest pair the arm ever accepted was 105
		// frames against a bound of 32768. Depth must be a PARAMETER of the
		// generator, not an accident of it.
		if rng.Float64() < cycleProb {
			nd.A = ns[rng.Intn(n)] // back-edge: a cycle
		} else if i+1 < n {
			nd.A = ns[i+1]
		}
		if rng.Intn(3) == 0 {
			nd.B = pick(i)
		}
		if rng.Intn(4) == 0 {
			nd.Arr[rng.Intn(2)] = pick(i)
		}
		if rng.Intn(3) == 0 {
			k := rng.Intn(3)
			nd.S = make([]any, k)
			for j := range nd.S {
				nd.S[j] = pick(i)
			}
		}
		if rng.Intn(4) == 0 {
			nd.M = map[string]any{}
			for j := 0; j < rng.Intn(3); j++ {
				nd.M[fmt.Sprint(j)] = pick(i)
			}
		}
		if rng.Intn(3) == 0 {
			nd.I = pick(i)
		}
	}
	return ns[0]
}

const advOracleCap = 300_000

// advCheckPair is the single P1 assertion, reused by the seeded suite and the
// fuzz target.
func advCheckPair(t *testing.T, a, b any, label string) {
	t.Helper()
	err := checkDeepEqualPairDepth(a, b, "p1")
	if err != nil {
		return // a refusal is never a P1 violation
	}
	peak, capped := advPairDepth(a, b, advOracleCap)
	if capped || peak > maxWalkFrames {
		t.Errorf("P1 VIOLATED — UNDER-REPORT / UNSAFE ACCEPT [%s]\n"+
			"  guard: ACCEPTED (returned nil)\n"+
			"  oracle: reflect.DeepEqual descends to depth %d (capped=%v), bound is %d\n"+
			"  The guard let through a pair whose real comparison exceeds the bound it "+
			"exists to enforce.", label, peak, capped, maxWalkFrames)
	}
}

// ---------------------------------------------------------------------------
// P1a — randomized differential over random object graphs.
// ---------------------------------------------------------------------------
func TestADV_P1_RandomGraphsNeverUnderReport(t *testing.T) {
	const iters = 3000
	worst := 0
	accepted, refused := 0, 0
	for i := 0; i < iters; i++ {
		rng := rand.New(rand.NewSource(int64(i)))
		// n straddles the bound: half the sizes are in the accept range, half
		// past it, so the accept/refuse EDGE is where most samples land.
		// Sized so that samples land ON the accept/refuse edge. A decorated node
		// costs more than 2 frames, so the edge for this generator sits well
		// below maxWalkFrames/2 — the sizes are spread across it rather than
		// guessed at.
		n := []int{1, 5, 40, 500, 4000, 8000, 10000, 12000, 14000, 15000, 16000,
			maxWalkFrames/2 - 3, maxWalkFrames/2 + 3, maxWalkFrames + 1, 40000}[rng.Intn(15)]
		cp := []float64{0, 0.00005, 0.1, 0.35, 0.7}[rng.Intn(5)]
		a := advGen(rng, n, cp)

		var b any
		switch rng.Intn(3) {
		case 0: // structurally identical, distinct objects (worst case for DeepEqual)
			b = advGen(rand.New(rand.NewSource(int64(i))), n, cp)
		case 1: // independent graph
			b = advGen(rand.New(rand.NewSource(int64(i)+1<<20)), n, cp)
		case 2: // the same object
			b = a
		}

		if err := checkDeepEqualPairDepth(a, b, "p1"); err != nil {
			refused++
			continue
		}
		accepted++
		peak, capped := advPairDepth(a, b, advOracleCap)
		if peak > worst {
			worst = peak
		}
		if capped || peak > maxWalkFrames {
			t.Fatalf("P1 VIOLATED at seed %d (n=%d cycleProb=%v): guard ACCEPTED, oracle depth %d (capped=%v) > bound %d",
				i, n, cp, peak, capped, maxWalkFrames)
		}
	}
	t.Logf("MEASURED: %d iterations, %d accepted / %d refused, deepest ACCEPTED pair = %d frames (bound %d)",
		iters, accepted, refused, worst, maxWalkFrames)
}

// ---------------------------------------------------------------------------
// P1b — the memo is the prime suspect, so aim directly at it.
//
// Shapes where a subtree is reached from several places at DIFFERENT stack
// depths. Node-keyed memoisation caches ONE depth per node; if that cached
// depth is ever consumed at a deeper position than the one it was computed at,
// the walk under-reports.
// ---------------------------------------------------------------------------
func TestADV_P1_MemoConsumedAtADeeperPosition(t *testing.T) {
	// A "diamond ladder": a deep tail T, reached both directly from the root
	// (shallow) and through a long spine (deep). The memo necessarily stores
	// T's depth from whichever arm the DFS walks first.
	mk := func(spine, tail int, tailFirst bool) any {
		tl := mkAdvChain(tail)
		root := &advG{}
		cur := root
		for i := 0; i < spine; i++ {
			nx := &advG{}
			cur.A = nx
			cur = nx
		}
		// field order decides DFS order: A is walked before I.
		if tailFirst {
			root.I = tl // shallow arm reached first? no: A is field 0
			cur.I = tl  // deep arm
		} else {
			cur.I = tl
			root.I = tl
		}
		return root
	}
	for _, spine := range []int{1, 8, 100, 4000, 16000} {
		for _, tail := range []int{1, 8, 100, 4000, 16000} {
			for _, tf := range []bool{true, false} {
				a := mk(spine, tail, tf)
				b := mk(spine, tail, tf)
				advCheckPair(t, a, b, fmt.Sprintf("diamond spine=%d tail=%d", spine, tail))
			}
		}
	}
}

// P1c — shared substructure reached through EVERY key kind, so the memo is
// exercised on pointer, slice and map keys and on the deliberately-unkeyed
// interface.
func TestADV_P1_SharedSubtreeThroughEveryKeyKind(t *testing.T) {
	tail := mkAdvChain(12000)
	mk := func(kind int) any {
		root := &advG{}
		deep := root
		for i := 0; i < 6000; i++ {
			nx := &advG{}
			deep.A = nx
			deep = nx
		}
		switch kind {
		case 0:
			root.I, deep.I = tail, tail
		case 1:
			root.S, deep.S = []any{tail}, []any{tail}
		case 2:
			root.M = map[string]any{"t": tail}
			deep.M = map[string]any{"t": tail}
		case 3:
			sh := []any{tail}
			root.S, deep.S = sh, sh // the SAME slice object, shared
		}
		return root
	}
	for k := 0; k < 4; k++ {
		advCheckPair(t, mk(k), mk(k), fmt.Sprintf("shared-tail kind=%d", k))
	}
}

// P1d — boundary sweep on the accept/refuse edge, in FRAMES.
func TestADV_P1_BoundarySweepInFrames(t *testing.T) {
	for _, links := range []int{
		1, 2,
		maxWalkFrames/2 - 2, maxWalkFrames/2 - 1, maxWalkFrames / 2, maxWalkFrames/2 + 1, maxWalkFrames/2 + 2,
		maxWalkFrames - 1, maxWalkFrames, maxWalkFrames + 1,
	} {
		a, b := mkAdvChain(links), mkAdvChain(links)
		fr, _, ex := deepEqualFrames(a, maxWalkFrames)
		err := checkDeepEqualPairDepth(a, b, "sweep")
		peak, capped := advPairDepth(a, b, advOracleCap)
		t.Logf("links=%-6d walkFrames=%-6d exceeded=%-5v guard=%-6v oracleDepth=%d capped=%v",
			links, fr, ex, err == nil, peak, capped)
		if err == nil && (capped || peak > maxWalkFrames) {
			t.Errorf("P1 VIOLATED at links=%d: accepted, real depth %d > bound %d", links, peak, maxWalkFrames)
		}
	}
}

// ---------------------------------------------------------------------------
// P2 — TERMINATION. Trading a crash for a hang is not a fix.
// Every shape here must return; the budget is generous so only a real hang trips.
// ---------------------------------------------------------------------------
func TestADV_P2_TerminatesOnEveryShape(t *testing.T) {
	mkRing := func(n int) any {
		ns := make([]*advChain, n)
		for i := range ns {
			ns[i] = &advChain{}
		}
		for i := range ns {
			ns[i].Next = ns[(i+1)%n]
		}
		return ns[0]
	}
	selfSlice := func() any { s := make([]any, 1); s[0] = s; return s }
	selfMap := func() any { m := map[string]any{}; m["k"] = m; return m }

	// v = []any{v,v} x24: 2^24 paths, linear only if the memo works.
	expShare := func() any {
		var v any = 1
		for i := 0; i < 24; i++ {
			v = []any{v, v}
		}
		return v
	}
	// The same, through the UNKEYED kind (interface inside a struct).
	expShareIface := func() any {
		var v any = 1
		for i := 0; i < 22; i++ {
			v = &advG{I: v, Arr: [2]*advG{}, S: []any{v, v}}
		}
		return v
	}

	cases := []struct {
		name string
		mk   func() any
	}{
		{"ring 10", func() any { return mkRing(10) }},
		{"ring maxWalkFrames-1", func() any { return mkRing(maxWalkFrames - 1) }},
		{"ring maxWalkFrames", func() any { return mkRing(maxWalkFrames) }},
		{"ring maxWalkFrames+1", func() any { return mkRing(maxWalkFrames + 1) }},
		{"ring 1e6 (far past the bound)", func() any { return mkRing(1_000_000) }},
		{"self slice", selfSlice},
		{"self map", selfMap},
		{"exponential sharing x24 (slices)", expShare},
		{"exponential sharing x22 (unkeyed interface spine)", expShareIface},
		{"deep acyclic 1e6 links", func() any { return mkAdvChain(1_000_000) }},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			a, b := tc.mk(), tc.mk()
			done := make(chan time.Duration, 1)
			go func() {
				st := time.Now()
				_ = checkDeepEqualPairDepth(a, b, "term")      //nolint:errcheck // timing probe: result unused, measuring wall time
				_ = checkDeepEqualPairDepth(a, a, "term-same") //nolint:errcheck // timing probe: result unused, measuring wall time
				done <- time.Since(st)
			}()
			select {
			case d := <-done:
				t.Logf("MEASURED: terminated in %v", d)
				if d > 5*time.Second {
					t.Errorf("P2 DEGRADED: the guard took %v on this shape. A guard that costs "+
						"seconds on a few-KB value is a denial-of-service on the path it protects", d)
				}
			case <-time.After(60 * time.Second):
				t.Fatalf("P2 VIOLATED: checkDeepEqualPairDepth did not terminate within 60s. " +
					"Trading a crash for a hang is not a fix")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// P3 — the 800/801 ring case, exercised directly and generalised.
// ---------------------------------------------------------------------------
func TestADV_P3_CoprimeRings(t *testing.T) {
	for _, p := range [][2]int{{800, 801}, {2, 3}, {3, 5}, {7, 11}, {64, 65}, {800, 800}, {1, 2}} {
		a, b := mkAdvCyc(p[0]), mkAdvCyc(p[1])
		err := checkDeepEqualPairDepth(a, b, "coprime")
		peak, capped := advPairDepth(a, b, advOracleCap)
		t.Logf("rings %d/%d: guard=%v oracleDepth=%d capped=%v", p[0], p[1],
			map[bool]string{true: "ACCEPT", false: "refuse"}[err == nil], peak, capped)
		if err == nil && (capped || peak > maxWalkFrames) {
			t.Errorf("P3 VIOLATED: rings %d/%d accepted, real depth %d > bound %d",
				p[0], p[1], peak, maxWalkFrames)
		}
	}
}

// ---------------------------------------------------------------------------
// FUZZ — seeded from the boundaries above. `go test -run=Fuzz -fuzz=FuzzADVPair`
// ---------------------------------------------------------------------------
func FuzzADVPair(f *testing.F) {
	// 116-AF8. The corpus used to end with `f.Add(int64(99), maxWalkFrames, 9, 0)` — a seed
	// placed at the accept bound, whose evident purpose was to exercise it — while the body
	// range-checked `n > 5000` and skipped. That seed had never run and never would.
	//
	// No coverage was missing: the accept bound is covered exhaustively by
	// TestADV_G1_AcceptEdgeIsAtTheBound and TestADV_P1_BoundarySweepInFrames, and this
	// target deliberately searches SHAPE space rather than the boundary, because a 16k-link
	// chain per exec would cost the fuzzer its throughput. What was wrong is the SIGNAL — a
	// reader saw a boundary seed in the corpus and concluded the boundary was fuzzed.
	//
	// BOTH halves of that are fixed here, because normalizing alone would have been worse:
	// the misleading seed would then silently run at n=2769 and still READ as a boundary
	// seed. So the seed now states its real value, and the note above says where the bound
	// is actually covered.
	f.Add(int64(0), 8, 3, 0)
	f.Add(int64(1), 40, 0, 1)
	f.Add(int64(7), 800, 7, 2)
	f.Add(int64(99), 5000, 9, 0) // the top of the range this target searches
	f.Fuzz(func(t *testing.T, seed int64, n int, cyc int, mode int) {
		// NORMALIZE, NEVER RANGE-CHECK. A range check makes a seed's fate depend on a
		// value the author cannot see when writing it, which is exactly how the dead seed
		// above survived. Normalizing puts every input in range BY CONSTRUCTION, so no
		// seed in this corpus can be silently dead — the same discipline
		// FuzzGroupCommitFrontier uses one file over (%32, %97), where all 10 seeds run.
		// Written to be total over int, including negatives and math.MinInt (% by a
		// positive constant cannot overflow).
		//
		// 🔴 116-GC-F2: THE FIRST VERSION OF THIS WAS `n = 1 + ((n%5000)+5000)%5000`,
		// which is NOT the identity on [1,5000] — it is n+1 on [0,4998] and it maps
		// 5000 to 1. So the seed annotated "the top of the range" ran at the BOTTOM, and
		// the three older seeds silently shifted 8->9, 40->41, 800->801. That is the
		// SAME signal defect 116-AF8 was filed for, surviving inside its own fix: an
		// annotated value in the corpus, and the body running something else. The `1 +`
		// belonged on the input, not on the result.
		n = ((n % 5000) + 5000) % 5000
		if n == 0 {
			n = 5000 // identity on [1,5000]
		}
		cyc = ((cyc % 11) + 11) % 11 // cyc in [0,10]
		rng := rand.New(rand.NewSource(seed))
		cp := float64(cyc) / 10.0
		a := advGen(rng, n, cp)
		var b any
		switch ((mode % 3) + 3) % 3 {
		case 0:
			b = advGen(rand.New(rand.NewSource(seed)), n, cp)
		case 1:
			b = advGen(rand.New(rand.NewSource(seed+1)), n, cp)
		default:
			b = a
		}
		advCheckPair(t, a, b, fmt.Sprintf("fuzz seed=%d n=%d cyc=%d mode=%d", seed, n, cyc, mode))
	})
}
