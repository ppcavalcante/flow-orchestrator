// Command 05-dynamic-fanout maps a branch action over N items that are discovered at
// RUNTIME — the width is not known when the graph is built. A single AddFanOut node
// expands into N parallel branches; each branch sees only its own item and records a
// per-item result; the node collects the N typed results back into the parent data.
//
//	seed ─▶ render (fan-out) ──┬─▶ branch(item 0)
//	                           ├─▶ branch(item 1)
//	                           └─▶ branch(item N-1)   (N discovered by the expander)
//
// It shows:
//   - AddFanOut(name, expander, branchAction) — the expander returns the ordered list
//     of per-branch inputs (len == N), discovered at run time from the parent data,
//   - the branch reads its item under workflow.FanOutItemKey (JSON-journaled, so an
//     integer item arrives as json.Number — int64-faithful),
//   - WithResults(base, branchKey) — each branch's branchKey scalar is collected TYPED
//     into parent data under base[i], plus base.__count__ = N,
//   - WithMaxWidth(n) — a guardrail: an expansion wider than n is refused at run time.
//
// The expander runs EXACTLY ONCE even across a crash+resume (its result is journaled),
// which is why fan-out requires a Checkpointer store.
//
// Run it:
//
//	go run ./examples/05-dynamic-fanout
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

const (
	keyPageCount = "page_count" // seeded: how many pages the "document" turned out to have
	keyResults   = "ocr_sizes"  // WithResults base key: ocr_sizes[i] + ocr_sizes.__count__
	branchOutKey = "size"       // the scalar each branch Sets; WithResults reads it per branch
	maxPages     = 64           // WithMaxWidth guardrail — refuse a runaway expansion
)

// buildFanOutWorkflow constructs the fan-out graph on the given store. seed publishes the
// page count (as if a worker had opened the document and counted its pages); the fan-out's
// expander reads that count at run time and produces one branch per page. Standalone so the
// smoke test builds the identical graph and asserts the durable per-branch results.
func buildFanOutWorkflow(store workflow.WorkflowStore, id string, pages int) (*workflow.Workflow, error) {
	b := workflow.NewWorkflowBuilder().
		WithWorkflowID(id).
		WithStore(store)

	b.AddStartNode("seed").WithActionFunc(func(_ context.Context, data *workflow.WorkflowData) error {
		data.Set(keyPageCount, int64(pages))
		fmt.Printf("seed: document has %d pages\n", pages)
		return nil
	})

	// render fans out over the pages discovered by the expander. The expander runs INSIDE
	// the parent Execute and reads the run data — the width is a runtime value, not a build
	// constant. Each item is the page index; a branch sees ONLY its item, so everything a
	// branch needs must be encoded into that item.
	b.AddFanOut("render", expandPages, workflow.ActionFunc(ocrBranch)).
		WithResults(keyResults, branchOutKey).
		WithMaxWidth(maxPages).
		DependsOn("seed")

	return workflow.FromBuilder(b)
}

// expandPages is the FanOutExpander: it discovers N at run time (here from the seeded page
// count) and returns the ordered per-branch inputs. len(items) == N == the fan-out width.
func expandPages(_ context.Context, data *workflow.WorkflowData) ([]interface{}, error) {
	n, ok := data.GetInt64(keyPageCount)
	if !ok {
		return nil, fmt.Errorf("expand: %q not seeded", keyPageCount)
	}
	items := make([]interface{}, n)
	for i := range items {
		items[i] = i // one item per discovered page; branches run in discovery order
	}
	fmt.Printf("expand: fanning out over %d pages\n", n)
	return items, nil
}

// ocrBranch is one page's work: it reads its item (the page index) and records a typed
// per-item result — here a stand-in "OCR output size" derived from the page. The item
// arrives JSON-journaled, so an integer item is a json.Number (int64-faithful).
func ocrBranch(_ context.Context, data *workflow.WorkflowData) error {
	raw, ok := data.Get(workflow.FanOutItemKey)
	if !ok {
		return fmt.Errorf("branch: no item under %q", workflow.FanOutItemKey)
	}
	num, ok := raw.(json.Number)
	if !ok {
		return fmt.Errorf("branch: item is not a json.Number: %T", raw)
	}
	page, err := num.Int64()
	if err != nil {
		return fmt.Errorf("branch: item .Int64(): %w", err)
	}
	size := (page + 1) * 100 // deterministic stand-in for real per-page work
	data.Set(branchOutKey, size)
	fmt.Printf("branch: page %d → size %d\n", page, size)
	return nil
}

// fanOutIndexKey / fanOutCountKey mirror the parent-data key convention the fan-out writes
// its typed results under: base[i] for the i-th branch result, base.__count__ = N.
func fanOutIndexKey(base string, i int) string { return fmt.Sprintf("%s[%d]", base, i) }
func fanOutCountKey(base string) string        { return base + ".__count__" }

func run() error {
	store := workflow.NewInMemoryStore()
	const id, pages = "fanout-doc", 5

	wf, err := buildFanOutWorkflow(store, id, pages)
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}

	// A clean success (no branch fails), so a non-nil Execute error is a real defect.
	if err := wf.Execute(context.Background()); err != nil {
		return fmt.Errorf("execute: %w", err)
	}

	data, err := store.Load(id)
	if err != nil {
		return fmt.Errorf("load: %w", err)
	}

	// Read the collected results back exactly as an operator would: the count key, then
	// each typed per-branch result under base[i].
	count, _ := data.GetInt64(fanOutCountKey(keyResults))
	var total int64
	for i := int64(0); i < count; i++ {
		v, _ := data.GetInt64(fanOutIndexKey(keyResults, int(i)))
		total += v
	}
	fmt.Printf("\nresult: %d branches, total size %d\n", count, total)
	return nil
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("05-dynamic-fanout: %v", err)
	}
}
