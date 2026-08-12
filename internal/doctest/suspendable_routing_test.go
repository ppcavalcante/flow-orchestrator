// Suspendable-child routing gate (M23 ph116, authored by qa).
//
// WHY THIS EXISTS. One documentation claim — that AddSubWorkflowQueued is THE
// route for a suspendable sub-workflow child — took FIVE repair rounds, each
// caught by an independent gate, each fixing a real defect. The affected-site
// count went 5 → 7 → 8. The author's own diagnosis of the recurring miss:
//
//	"The recurring failure isn't carelessness in any instance; it's asserting
//	 over a category I hadn't enumerated."
//
// And, in the same phase, the engineer diagnosed a vacuity shape, documented it,
// banked it to its KB — then committed the identical shape again:
//
//	"Knowing the failure mode did not prevent me from committing it; only
//	 running the bite and reading its output did."
//
// A lesson in a knowledge base does not fire at the moment of authorship. A
// check does. This file is that check.
//
// THE CLAIM. A suspendable child is accepted on EITHER the parked or the queued
// path; only INLINE refuses one (only AddSubWorkflow calls scanChildInlineSafe;
// AddSubWorkflowParked deliberately skips the scan). The retired framing routes
// such a child to the queue path exclusively.
//
// WHY A RULE, NOT A GOLDEN LIST. The diagnosed failure mode is "asserting over a
// category I hadn't enumerated". A golden list of known sites cannot catch a site
// nobody enumerated — it re-implements the failure. These checks encode the banned
// ASSERTION, so a site written tomorrow reds too. On its first run this gate found
// a live 9th site that all five rounds missed, in a file round 5 had itself edited
// (F-DOC-01, pkg/workflow/builder.go AddSubWorkflow godoc).
//
// WHY IT DOESN'T RED ON THE LEGITIMATE QUEUE-ONLY CLAIMS. Several statements of
// the form "queue only" are TRUE and must stay: WithInput really is queue-only,
// the depth-ceiling override really is inline-only, F-P95-04 cycle extraction
// really is queue-edges-only. A gate that reds on those gets silenced in a week.
// The separation is a CONJUNCTION, and different true claims are excluded by
// different clauses of it — that is measured, not assumed, in
// TestSuspendableRouting_DetectorClausesAreLoadBearing, which pairs each clause
// with a fixture whose exclusion depends on THAT clause alone.
// WHAT THIS MATCHER STRUCTURALLY CANNOT SEE — written down deliberately, because
// a check whose limits are recorded is worth more than one that appears total,
// and because every miss in this thread was an INSTRUMENT failure rather than an
// attention failure. These are the known blind spots, not a disclaimer:
//
//  1. PARAPHRASE WITHOUT THE ANCHOR WORD. Check A needs "suspendable"; check C
//     needs the park sense. "A child that waits for a signal belongs on the queue
//     path" carries the retired claim with neither anchor and is invisible here.
//  2. WINDOW BOUNDARY. Claim and correction more than 3 lines apart (2 for C) are
//     not seen together. A directive whose exempting "parked" sits 6 lines away
//     still reds — a false positive that must be fixed by moving the text, not by
//     widening the window (a wide window silently exempts real defects).
//  3. SEMANTIC NEGATION BEYOND THE MARKER LIST. Check C reads a fixed set of
//     negation/counterfactual markers. A guarded sentence phrased without one
//     ("the store mismatch is caught at Build") would red.
//  4. NON-PROSE SURFACES. Diagrams, tables rendered from data, generated docs,
//     and anything outside the scoped extensions are not read at all.
//  5. IT CANNOT JUDGE TRUTH. It encodes two SPECIFIC retired claims. A third
//     retired claim needs a third check; this file does not generalise itself.
//
// The value is not that it is more careful than a person. It is that these five
// limits are fixed and inspectable, rather than rediscovered one per round.
package doctest

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// --- scope -------------------------------------------------------------------

// scopedDocCorpus returns every file the claim is gated over: docs/, pkg/ godoc
// and comments, examples/, README.md, CHANGELOG.md.
//
// /adr/ is EXCLUDED by design: an ADR records a decision at a point in time and
// is deliberately left historical. _test.go is excluded — a test may quote a
// retired framing as a fixture (this file does).
func scopedDocCorpus(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	for _, entry := range []string{"README.md", "CHANGELOG.md", "STABILITY.md"} {
		p := filepath.Join(root, entry)
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	for _, dir := range []string{"docs", "pkg", "examples"} {
		base := filepath.Join(root, dir)
		if _, err := os.Stat(base); err != nil {
			continue
		}
		err := filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "adr" {
					return filepath.SkipDir
				}
				return nil
			}
			name := d.Name()
			if strings.HasSuffix(name, "_test.go") {
				return nil
			}
			if strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".go") {
				out = append(out, p)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", base, err)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatal("scoped corpus is EMPTY — the gate would pass vacuously")
	}
	return out
}

// --- the detector ------------------------------------------------------------

// normalizeEmphasis strips markdown emphasis so a claim written as
// "route it to the **queue** path instead" cannot evade a match that
// "route it to the queue path instead" would trip. This is load-bearing, not
// cosmetic: the FIRST draft of this gate matched on raw lines and the bolded
// form slipped straight through (see the evasion fixture + its clause bite).
// Underscores are left alone so snake_case identifiers are not mangled.
func normalizeEmphasis(s string) string {
	return strings.NewReplacer("*", "", "`", "").Replace(s)
}

// reLeadMarker strips a leading comment / prose marker ("//", "/*", "*", "#",
// ">", "|") so a claim wrapped across godoc lines flattens into one sentence.
var reLeadMarker = regexp.MustCompile(`(?m)^\s*(//+|/\*+|\*+|#+|>+|\|)\s*`)

var reWhitespace = regexp.MustCompile(`\s+`)

// flattenWindow is the instrument fix, and it is load-bearing. FIVE successive
// hand sweeps for this claim all used line-oriented matching, and two sites
// survived every one of them for a structural reason no amount of pattern
// tuning addresses: the claim SPANS A LINE BREAK ("...to the\n// queue path").
// A line-oriented matcher is blind to that no matter how gap-tolerant its
// pattern is — the wrong instrument, not a mis-tuned one. Flattening the whole
// window (strip markers, drop emphasis, collapse whitespace) is what makes a
// wrapped claim visible at all.
func flattenWindow(lines []string) string {
	joined := strings.Join(lines, "\n")
	joined = reLeadMarker.ReplaceAllString(joined, " ")
	joined = normalizeEmphasis(joined)
	return reWhitespace.ReplaceAllString(joined, " ")
}

// suspRouteDetector is a VALUE so the clause bites can disable exactly one
// component and assert the specific outcome that flips.
type suspRouteDetector struct {
	anchor    *regexp.Regexp // SUBJECT gate: the text is about a suspendable child
	queue     *regexp.Regexp // the queued route is named
	parked    *regexp.Regexp // the parked route is named (the exemption)
	directive *regexp.Regexp // the text ROUTES/obliges rather than merely describing
	liveness  *regexp.Regexp // the park-FOREVER hazard, in its liveness sense only
	guarded   *regexp.Regexp // the hazard is negated or counterfactual (i.e. prevented)
	window    int            // lines of context each side of an anchor line
	normalize bool           // strip markdown emphasis before matching
}

func defaultDetector() suspRouteDetector {
	return suspRouteDetector{
		anchor:    regexp.MustCompile(`(?i)suspendable`),
		queue:     regexp.MustCompile(`(?i)AddSubWorkflowQueued|queue[- ](path|dispatch)|\bqueued\b|the queue\b`),
		parked:    regexp.MustCompile(`(?i)AddSubWorkflowParked|\bparked\b`),
		directive: regexp.MustCompile(`(?i)\binstead\b|\bmust\b|\bopt-in\b|\broute\b`),
		liveness: regexp.MustCompile(
			`(?i)forever[- ]park|park\w*(\s+\S+){0,4}\s+forever|forever(\s+\S+){0,2}\s+park\w*|strand\w*(\s+\S+){0,6}\s+forever`),
		guarded: regexp.MustCompile(
			`(?i)\bnever\b|\bnot\b|n't\b|\brather than\b|\bwould\b|\bavoid\b|\bprevent\w*\b|` +
				`\bbeats\b|\binstead of\b|\bloud\w*\b|\brefus\w+\b|\bcannot\b|\bdeliberately\b|\blapse\b`),
		window:    3,
		normalize: true,
	}
}

type violation struct {
	check string // "A-directive" | "B-symmetry"
	line  int    // 1-based
	text  string
	why   string
}

func (d suspRouteDetector) prep(lines []string) []string {
	if !d.normalize {
		return lines
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = normalizeEmphasis(l)
	}
	return out
}

// checkADirective flags a window that (1) is about a suspendable child, (2) names
// the queued route, (3) ROUTES or obliges, and (4) never names the parked route.
// Clause (4) is the heart: the correct fix for this defect class IS naming both
// routes, so the check's green condition equals the correct documentation state.
func (d suspRouteDetector) checkADirective(raw []string) []violation {
	lines := d.prep(raw)
	var vs []violation
	for i, l := range lines {
		if !d.anchor.MatchString(reLeadMarker.ReplaceAllString(l, " ")) {
			continue
		}
		lo, hi := i-d.window, i+d.window+1
		if lo < 0 {
			lo = 0
		}
		if hi > len(lines) {
			hi = len(lines)
		}
		w := lines[lo:hi]
		var flat string
		if d.normalize {
			flat = flattenWindow(w)
		} else {
			flat = strings.Join(w, "\n")
		}
		if d.queue.MatchString(flat) && d.directive.MatchString(flat) && !d.parked.MatchString(flat) {
			vs = append(vs, violation{
				check: "A-directive",
				line:  i + 1,
				text:  strings.TrimSpace(raw[i]),
				why:   "routes a SUSPENDABLE child to the queued path without naming the parked path, which accepts one too",
			})
		}
	}
	return vs
}

var (
	reParkedSym = regexp.MustCompile(`AddSubWorkflowParked`)
	reQueuedSym = regexp.MustCompile(`AddSubWorkflowQueued`)
)

// checkBSymmetry flags a CAPABILITY LISTING (a markdown table or a signature
// block) that annotates exactly one of the parked/queued pair as accepting a
// suspendable child. This is an OMISSION defect: nothing false is asserted, the
// parked row is merely left untagged while the queued row is tagged — which reads
// as "queue is where suspendable children go". Check A is structurally blind to
// it (the sibling row names "parked", so A's exemption fires), which is exactly
// why a second, differently-shaped check is needed rather than a wider regex.
func (d suspRouteDetector) checkBSymmetry(raw []string) []violation {
	lines := d.prep(raw)
	var vs []violation
	for i, l := range lines {
		if !reParkedSym.MatchString(l) {
			continue
		}
		for j := i - 2; j <= i+2; j++ {
			if j < 0 || j >= len(lines) || j == i {
				continue
			}
			if !reQueuedSym.MatchString(lines[j]) || reParkedSym.MatchString(lines[j]) {
				continue
			}
			pk := d.anchor.MatchString(l)
			qd := d.anchor.MatchString(lines[j])
			if pk != qd {
				tagged, untagged := "queued", "parked"
				if pk {
					tagged, untagged = "parked", "queued"
				}
				vs = append(vs, violation{
					check: "B-symmetry",
					line:  i + 1,
					text:  strings.TrimSpace(raw[i]),
					why: "capability listing tags " + tagged + " as suspendable-capable but leaves " +
						untagged + " untagged; both accept a suspendable child",
				})
			}
			break
		}
	}
	return vs
}

// checkCLiveness flags the retired LIVENESS claim: that a wrong-store parked
// child parks the parent "forever". The true form is the mechanism — the load
// returns ErrNotFound, which maps to ErrSuspended, so the node RE-PARKS on every
// wake and no number of re-drives converges it.
//
// The precision problem here is the hardest in this file and it is NOT solved by
// the word: "forever" carries three unrelated senses in this repo ("gone
// forever", "stays pending forever" by design, and the park hazard). Anchoring
// on the bare word produced 3 false positives on TRUE sentences. Two clauses fix
// it, and both are bitten below:
//
//	liveness — matches only the PARK sense (gap-tolerant, so "parks THE PARENT
//	           forever" is caught where an adjacency pattern misses it);
//	guarded  — exempts the mirror-image majority. Most "forever" text in this
//	           repo describes a hazard the code PREVENTS ("never a forever-park",
//	           "rather than parking forever", "that WOULD strand ... so ...").
//	           The discriminator is not the word but whether the sentence asserts
//	           the hazard EXISTS or asserts it is GUARDED.
func (d suspRouteDetector) checkCLiveness(raw []string) []violation {
	lines := d.prep(raw)
	var vs []violation
	for i := range lines {
		if !d.liveness.MatchString(flattenWindow(lines[i : i+1])) {
			continue
		}
		lo, hi := i-2, i+3
		if lo < 0 {
			lo = 0
		}
		if hi > len(lines) {
			hi = len(lines)
		}
		if !d.guarded.MatchString(flattenWindow(lines[lo:hi])) {
			vs = append(vs, violation{
				check: "C-liveness",
				line:  i + 1,
				text:  strings.TrimSpace(raw[i]),
				why: "asserts a park-forever hazard as fact; the mechanism is ErrNotFound → ErrSuspended, " +
					"so the node RE-PARKS on every wake and no number of re-drives converges it",
			})
		}
	}
	return vs
}

func (d suspRouteDetector) scan(lines []string) []violation {
	vs := append(d.checkADirective(lines), d.checkBSymmetry(lines)...)
	return append(vs, d.checkCLiveness(lines)...)
}

func (d suspRouteDetector) scanText(s string) []violation {
	return d.scan(strings.Split(s, "\n"))
}

// --- 1. the live gate --------------------------------------------------------

// TestSuspendableRouting_LiveCorpus is the gate proper: it reds when the retired
// framing appears anywhere in the scoped corpus, including at a site nobody has
// enumerated.
func TestSuspendableRouting_LiveCorpus(t *testing.T) {
	root := repoRoot(t)
	det := defaultDetector()

	scanned := 0
	var found []string
	for _, p := range scopedDocCorpus(t, root) {
		b, err := os.ReadFile(p) //nolint:gosec // paths come from the repo walk above
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		scanned++
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			rel = p // absolute path is still a usable citation
		}
		for _, v := range det.scanText(string(b)) {
			found = append(found, "  "+rel+":"+strconv.Itoa(v.line)+" ["+v.check+"]\n"+
				"    text: "+v.text+"\n    why:  "+v.why)
		}
	}
	t.Logf("scanned %d scoped files", scanned)
	if scanned < 20 {
		t.Fatalf("only %d files scanned — the corpus collapsed; the gate would pass vacuously", scanned)
	}
	if len(found) > 0 {
		t.Fatalf("retired suspendable-routing framing found at %d site(s):\n%s\n\n"+
			"A suspendable child is accepted on EITHER the parked or the queued path; only INLINE\n"+
			"refuses one. Name both routes and let the reader choose on the STORE (parked needs a\n"+
			"SignalStore; queued needs a multi-process *SQLiteStore + Pool + Registry).",
			len(found), strings.Join(found, "\n"))
	}
}

// --- 2. bite: every historical defect must RED, by the RIGHT check ------------

// historicalDefects are the real texts removed across the five repair rounds,
// extracted mechanically from the commit diffs — never retyped from memory, which
// is the failure mode this file exists to stop. Each names the check that must
// catch it, so a bite cannot pass merely because the OTHER check happened to fire.
var historicalDefects = []struct {
	name  string
	check string
	text  string
}{
	{
		name:  "guide/summary-table-parked-untagged (sub-workflows.md:75-76)",
		check: "B-symmetry",
		text: "| `AddSubWorkflow(name, child *DAG)` | definition-value, **non-suspendable** | **inline** (blocks) | blocking |\n" +
			"| `AddSubWorkflowParked(name, child *DAG)` | definition-value | **out-of-band** | park → wake |\n" +
			"| `AddSubWorkflowQueued(name, childType)` | **type-ref**, may be suspendable | **queue** (`Pool`) | park → wake |",
	},
	{
		name:  "guide/route-such-a-child-to-the-queue-path-instead (sub-workflows.md:123)",
		check: "A-directive",
		text: "The child's whole spawn-closure is **scanned at build**: a suspendable node anywhere in it fails\n" +
			"`Build` with `ErrSubWorkflowSuspendableChild` (an inline child blocks the parent, so it can never\n" +
			"park). Route such a child to the queue path instead. Do **not** also call `WithAction`.",
	},
	{
		name:  "guide/heading-queue-owns-suspendable-children (sub-workflows.md:125,127)",
		check: "A-directive",
		text: "### Queue sub-workflow (`AddSubWorkflowQueued`) — type-ref / suspendable children\n\n" +
			"The explicit opt-in for a child referenced by **type** and/or one that **parks** (e.g. a child with\n" +
			"its own approval).",
	},
	{
		name:  "guide/code-fence-must-be-the-queue-path (sub-workflows.md:142)",
		check: "A-directive",
		text: "    b := workflow.NewWorkflowBuilder()\n" +
			"    b.AddApproval(\"analyst-sign-off\")           // suspendable child → must be the queue path\n" +
			"    b.AddNode(\"score\").WithAction(score).DependsOn(\"analyst-sign-off\")",
	},
	{
		name:  "api-reference/route-it-to-AddSubWorkflowQueued-instead (api-reference.md:1402)",
		check: "A-directive",
		text: "// Build-time (surfaced from Build): an inline AddSubWorkflow child (or a transitive descendant)\n" +
			"// contains a suspendable node — route it to AddSubWorkflowQueued instead.\n" +
			"var ErrSubWorkflowSuspendableChild = errors.New(\"inline sub-workflow child contains a suspendable node: ...\")",
	},
	{
		name:  "api-reference/signature-block-parked-untagged (api-reference.md:1010-1011)",
		check: "B-symmetry",
		text: "func (b *WorkflowBuilder) AddSubWorkflow(name string, child *DAG) *NodeBuilder       // inline, blocks; non-suspendable child\n" +
			"func (b *WorkflowBuilder) AddSubWorkflowParked(name string, child *DAG) *NodeBuilder // out-of-band, park→wake\n" +
			"func (b *WorkflowBuilder) AddSubWorkflowQueued(name, childType string) *NodeBuilder  // queue (Pool), type-ref/suspendable",
	},
	{
		name:  "builder.go/AddSubWorkflow-godoc-route-to-the-queue-path-instead (the LIVE 9th site, F-DOC-01)",
		check: "A-directive",
		text: "// The child's whole spawn-closure is scanned AT BUILD for any suspendable node (an inline\n" +
			"// child BLOCKS the parent, so it can never park): a suspendable node anywhere in the\n" +
			"// closure fails Build with ErrSubWorkflowSuspendableChild — route such a child to the\n" +
			"// queue path (ph94) instead. The action is set directly, so do NOT also call WithAction.",
	},
	{
		name:  "builder.go/AddSubWorkflowQueued-godoc-the-explicit-opt-in",
		check: "A-directive",
		text: "// the coe-aware verdict. This is the queue counterpart to AddSubWorkflow (inline, ph91) — the explicit\n" +
			"// opt-in for a TYPE-REF and/or SUSPENDABLE child (which the inline path refuses). It structurally\n" +
			"// requires a multi-process *SQLiteStore + a worker Pool + a Registry (the type→DAG map, injected at\n" +
			"// Execute — the DAG carries only the type STRING, keeping the workflow pure DATA).",
	},
	{
		name:  "guide/fence-comment-parks-the-parent-forever (site 9, sub-workflows.md:244)",
		check: "C-liveness",
		text: "    // The child is spawned under a deterministic ID derived from the\n" +
			"    // parent's store, so a different store parks the parent forever.",
	},
	{
		// Not from history: an EVASION the first draft of this gate let through.
		// Kept permanently so a future simplification of normalizeEmphasis reds.
		name:  "evasion/bolded-queue-path (regression on normalizeEmphasis)",
		check: "A-directive",
		text:  "a suspendable node fails `Build`. Route such a child to the **queue** path instead.",
	},
}

func TestSuspendableRouting_RedsOnEveryHistoricalDefect(t *testing.T) {
	det := defaultDetector()
	for _, d := range historicalDefects {
		t.Run(d.name, func(t *testing.T) {
			vs := det.scanText(d.text)
			if len(vs) == 0 {
				t.Fatalf("NON-BITE: the detector stayed GREEN on a real defect.\ntext:\n%s", d.text)
			}
			var got []string
			hit := false
			for _, v := range vs {
				got = append(got, v.check)
				if v.check == d.check {
					hit = true
				}
			}
			if !hit {
				t.Fatalf("WRONG-REASON BITE: expected check %q to fire, only %v did.\n"+
					"A defect caught by the wrong check is not evidence that check works.\ntext:\n%s",
					d.check, got, d.text)
			}
			t.Logf("red by %s: %s", d.check, vs[0].why)
		})
	}
}

// --- 3. bite: every legitimate claim must stay GREEN, named individually ------

// legitimateClaims are the queue-exclusivity statements that are TRUE. A gate
// that reds on these gets silenced within a week, so each is pinned by name.
var legitimateClaims = []struct{ name, text string }{
	{
		name: "builder.go WithInput godoc — queue-only is TRUE",
		text: "// WithInput sets the seeded KV input for a QUEUE-dispatched sub-workflow child (M19 ph94): the map is\n" +
			"// JSON-encoded into the work_queue row's input, and RunNext's seedInput sets each key as a child data\n" +
			"// key on the fresh run (so the child's first nodes read it). Only valid on an AddSubWorkflowQueued node.",
	},
	{
		name: "builder.go WithInput runtime error — queue-only is TRUE",
		text: "\t\tn.actionErr = fmt.Errorf(\"%w: WithInput is only valid on an AddSubWorkflowQueued node\", ErrValidation)",
	},
	{
		name: "guide WithInput — valid only on a queued node",
		text: "`WithInput(map)` seeds the child's data keys (JSON-encoded into the queue row's input). It is valid\n" +
			"**only** on a queued node.",
	},
	{
		name: "guide — only the queue path has an input mechanism",
		text: "in from the parent. Only the **queue** path has an input mechanism (`WithInput`).",
	},
	{
		name: "api-reference WithInput — queued node only",
		text: "func (n *NodeBuilder) WithInput(kv map[string]any) *NodeBuilder                      // queued node only — seeds child data",
	},
	{
		name: "guide depth ceiling — the override governs the inline path only",
		text: "> raises/lowers the ceiling for the **inline** path only. On the **queue** path a child runs in a\n" +
			"> fresh worker drive, so the accumulated depth is carried across the dispatch instead.",
	},
	{
		name: "api-reference depth ceiling — F-P95-02 inline-only scoping",
		text: "  `MaxSubWorkflowDepth` override governs the **inline** path only (`F-P95-02`); the queue path uses the\n" +
			"  carried depth.",
	},
	{
		name: "guide F-P95-04 — cycle check extracts only queue edges",
		text: "**Scope (`F-P95-04`):** the check extracts only the queue-sub-workflow edges from each factory's\n" +
			"built DAG.",
	},
	{
		name: "subworkflow_queue.go F-P95-04 — honestly scoped extraction",
		text: "// HONESTLY SCOPED (the load-bearing caveat, F-P95-04): this extracts only the queueSubWorkflowAction\n" +
			"// edges from each registered factory's built DAG.",
	},
	{
		name: "CORRECTED sentinel (subworkflow.go) — names BOTH routes",
		text: "var ErrSubWorkflowSuspendableChild = errors.New(\"inline sub-workflow child contains a suspendable node: \" +\n" +
			"\t\"inline cannot park, so use AddSubWorkflowParked (host runs the child; requires a SignalStore) \" +\n" +
			"\t\"or AddSubWorkflowQueued (engine dispatches it; requires a multi-process *SQLiteStore, Pool and Registry)\")",
	},
	{
		name: "CORRECTED guide table — both rows tagged suspendable",
		text: "| `AddSubWorkflowParked(name, child *DAG)` | definition-value (a verdict **classifier**, never executed), **may be suspendable** | **out-of-band** | park → wake |\n" +
			"| `AddSubWorkflowQueued(name, childType)` | **type-ref**, may be suspendable | **queue** (`Pool`) | park → wake |",
	},
	{
		name: "CORRECTED guide prose — route to EITHER path",
		text: "park). Route such a child to **either** the queued or the parked path — both accept a suspendable\n" +
			"child; only inline refuses one.",
	},
	{
		name: "forever/GUARDED — loud failure, never a forever-park (approval.go)",
		text: "// ErrWaitRequiresSignalStore (loud failure, never a forever-park). Named signal absent",
	},
	{
		name: "forever/GUARDED — fails loudly rather than parking forever (workflow.go)",
		text: "\t// fails loudly with ErrWaitRequiresSignalStore rather than parking forever).",
	},
	{
		name: "forever/GUARDED — counterfactual: a journal-only gate WOULD re-park (subworkflow_parked.go)",
		text: "// child's DATA journal, so a journal-only gate would re-park the woken parent forever. The queue row is",
	},
	{
		name: "forever/OTHER SENSE — a fired one-shot is gone forever (schedule.go)",
		text: "\t\t// A one-shot that FIRED (admitted) is gone forever → delete the row + release its lease atomically",
	},
	{
		name: "forever/OTHER SENSE — an unregistered type stays pending forever, by design (dispatch.md)",
		text: "Because an unregistered type or a too-old item stays `pending` forever, an operator can inspect the\nqueue and act.",
	},
	{
		name: "neutral description — names the machinery, obliges nothing",
		text: "A suspendable child parks mid-run. The queued path dispatches it on a Pool;\n" +
			"this paragraph merely describes the machinery.",
	},
}

func TestSuspendableRouting_GreenOnEveryLegitimateClaim(t *testing.T) {
	det := defaultDetector()
	for _, c := range legitimateClaims {
		t.Run(c.name, func(t *testing.T) {
			if vs := det.scanText(c.text); len(vs) != 0 {
				t.Fatalf("FALSE POSITIVE on a legitimate claim — a gate that reds here gets silenced:\n"+
					"  check: %s\n  why:   %s\ntext:\n%s", vs[0].check, vs[0].why, c.text)
			}
		})
	}
}

// --- 4. bite the DETECTOR: every clause must be load-bearing -----------------

// The first draft of this test paired each clause with the WRONG fixture and two
// bites came back non-biting: those fixtures were excluded by SEVERAL clauses at
// once, so disabling one changed nothing. That is the "partial mutation" non-bite
// — and it was only visible by reading the failure text, not the colour.
//
// The corrected form pairs each clause with a fixture whose exclusion depends on
// THAT clause ALONE, so disabling it MUST flip the fixture to red. If a pairing
// ever stops flipping, the clause has become decorative and the gate's precision
// is accidental rather than designed.
func TestSuspendableRouting_DetectorClausesAreLoadBearing(t *testing.T) {
	matchAll := regexp.MustCompile(`(?s).*`)
	matchNone := regexp.MustCompile("\x00NEVER\x00")

	cases := []struct {
		clause  string
		fixture string // must be GREEN under the default detector
		disable func(*suspRouteDetector)
		meaning string
	}{
		{
			clause: "SUBJECT anchor (text must be about a suspendable child)",
			fixture: "> raises/lowers the ceiling for the **inline** path only. On the **queue** path a child runs in a\n" +
				"> fresh worker drive, so the accumulated depth is carried across the dispatch instead.",
			disable: func(d *suspRouteDetector) { d.anchor = matchAll },
			meaning: "without the subject gate, a TRUE depth-ceiling claim (queue + 'instead', no parked) reds — " +
				"the anchor is what keeps queue-scoping statements out of this gate",
		},
		{
			clause: "PARKED exemption (naming the other route clears the finding)",
			fixture: "park). Route such a child to **either** the queued or the parked path — both accept a suspendable\n" +
				"child; only inline refuses one.",
			disable: func(d *suspRouteDetector) { d.parked = matchNone },
			meaning: "without the exemption the CORRECTED prose reds — naming both routes is exactly what a fix " +
				"must do, so the exemption is what makes the gate's green state equal the correct doc state",
		},
		{
			clause: "DIRECTIVE marker ('queue is THE route' vs 'queue is A route')",
			fixture: "A suspendable child parks mid-run. The queued path dispatches it on a Pool;\n" +
				"this paragraph merely describes the machinery.",
			disable: func(d *suspRouteDetector) { d.directive = matchAll },
			meaning: "without it, neutral description reds — the directive marker is the whole distinction " +
				"between describing the queue path and prescribing it",
		},
		{
			clause:  "LIVENESS sense narrowing (park-forever, not every 'forever')",
			fixture: "\t\t// A one-shot that FIRED (admitted) is gone forever → delete the row + release its lease atomically",
			disable: func(d *suspRouteDetector) { d.liveness = regexp.MustCompile(`(?i)forever`) },
			meaning: "widened to the bare word, a TRUE sentence in an unrelated sense of 'forever' reds — " +
				"the word carries three senses here, and only the park sense is the retired claim",
		},
		{
			clause:  "GUARDED exemption (hazard asserted vs hazard prevented)",
			fixture: "// ErrWaitRequiresSignalStore (loud failure, never a forever-park). Named signal absent",
			disable: func(d *suspRouteDetector) { d.guarded = matchNone },
			meaning: "without it the mirror-image majority reds — most forever-text in this repo describes a " +
				"hazard the code PREVENTS, so the discriminator is asserted-vs-guarded, not the word",
		},
		{
			clause:  "EMPHASIS normalization (markdown must not hide a directive)",
			fixture: "a suspendable node fails `Build`. Route such a child to the **queue** path instead.",
			disable: func(d *suspRouteDetector) { d.normalize = false },
			meaning: "with normalization off the bolded form goes GREEN — this is the evasion the first draft " +
				"of this gate shipped, kept as a permanent regression",
		},
	}

	for _, c := range cases {
		t.Run(c.clause, func(t *testing.T) {
			base := defaultDetector()
			baseRed := len(base.scanText(c.fixture)) > 0

			mut := defaultDetector()
			c.disable(&mut)
			mutRed := len(mut.scanText(c.fixture)) > 0

			if baseRed == mutRed {
				t.Fatalf("NON-BITE: disabling %q did not change the verdict on its paired fixture "+
					"(base red=%v, mutated red=%v).\nEither the clause is decorative, or this fixture is "+
					"excluded by another clause too — a PARTIAL mutation, which is not a bite.\nfixture:\n%s",
					c.clause, baseRed, mutRed, c.fixture)
			}
			t.Logf("load-bearing: %s", c.meaning)
		})
	}

	// Check B exists because check A is structurally blind to an OMISSION.
	t.Run("check B is the only thing that catches an omission defect", func(t *testing.T) {
		var table string
		for _, d := range historicalDefects {
			if strings.HasPrefix(d.name, "guide/summary-table") {
				table = d.text
			}
		}
		if table == "" {
			t.Fatal("fixture missing")
		}
		det := defaultDetector()
		lines := strings.Split(table, "\n")
		if vs := det.checkADirective(lines); len(vs) != 0 {
			t.Fatalf("check A caught the omission defect, so check B may be redundant: %+v", vs[0])
		}
		if vs := det.checkBSymmetry(lines); len(vs) == 0 {
			t.Fatal("NON-BITE: check B missed the very defect class it exists for")
		}
		t.Log("as designed: a directive rule cannot see an absence — check A is blind here, and check B " +
			"is why the pair covers all 8 historical sites rather than 6")
	})
}
