// Command capstone-document-pipeline is the full-capacity showcase: one
// realistic, durable document-processing workflow that composes several of the
// library's capabilities on a SQLite store.
//
//	seed ─▶ ocr-pages (FAN-OUT over N pages) ─▶ route (CHOICE by doc type)
//	          image ─▶ transcode ┐
//	          text  ─▶ index-arm ├─▶ route-merge ─▶ index (SUB-WORKFLOW)
//	          other ─▶ skip-arm  ┘                      │
//	                                                     ▼
//	                              approval (APPROVAL gate) ─▶ moderation-cleared
//	                                (WAIT-FOR-SIGNAL) ─▶ publish
//
// The capabilities on display, in one graph:
//
//   - a dynamic FAN-OUT — the page count is discovered at run time and each page
//     is OCR'd on its own branch (example 05);
//   - a CHOICE that routes by document type, reconverged with a merge (example 04);
//   - a SUB-WORKFLOW that indexes the document as a distinct child run (example 08);
//   - an APPROVAL gate that parks the run until a decision is delivered, using the
//     AUD-025 correlation nonce read straight from the store — the store-only
//     driver path (example 07); and
//   - a downstream WAIT-FOR-SIGNAL (moderation cleared), all on a durable SQLite
//     store (example 03).
//
// The run PARKS twice — at the approval and at the moderation wait. A parked
// Execute returns ErrSuspended (a success arm, "suspend is a crash you chose"),
// so the host re-drives Execute until the run completes. A background driver —
// holding ONLY the store, never the *Workflow — delivers the approval and the
// moderation signal once the run has parked on them. That is the realistic
// shape: the thing that approves a run is not the thing that drives it.
//
// This is a teaching capstone, not the full production rig — it trims retries,
// caps, cron, and the compensation saga (each has its own focused example) down
// to the clearest subset that still shows the pieces working together.
//
// Run it:
//
//	go run ./examples/capstone-document-pipeline
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// docInput is what a real worker would learn by opening the file: the doc id,
// its page count (the fan-out width), and its type (the choice arm). Approval is
// always required in this demo so the gate is exercised.
type docInput struct {
	docID   string
	pages   int    // N for the fan-out — the number of pages to OCR
	docType string // "image" → transcode arm, "text" → index arm, else → skip arm
}

// Node names and data keys — named constants keep producers and consumers honest.
const (
	nodeSeed       = "seed"
	nodeOCR        = "ocr-pages"
	nodeRoute      = "route"
	nodeTranscode  = "transcode"
	nodeIndexArm   = "index-arm"
	nodeSkipArm    = "skip-arm"
	nodeMerge      = "route-merge"
	nodeIndex      = "index"
	nodeApproval   = "approval"
	nodeModeration = "moderation-cleared"
	nodePublish    = "publish"

	keyDocID     = "doc_id"
	keyPages     = "pages"
	keyDocType   = "doc_type"
	keyPageOCR   = "page-ocr" // WithResults base key for the fan-out
	keyIndexed   = "indexed"  // the index sub-workflow's result
	keyPublished = "published"
)

// ledger is the process-shared record of real side effects (OCR a page, index,
// publish). The pipeline calls record at every effect so the smoke test can
// assert exactly-once per stage independently of the engine's own journal. It is
// mutex-guarded because fan-out branches run concurrently.
type ledger struct {
	mu   sync.Mutex
	recs []string
}

func (l *ledger) record(effect, key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recs = append(l.recs, effect+":"+key)
}

// count reports how many recorded effects start with prefix.
func (l *ledger) count(prefix string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, r := range l.recs {
		if len(r) >= len(prefix) && r[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}

func (l *ledger) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.recs...)
}

// pageItem is one fan-out item: the (docID, page) a branch OCRs. A branch sees
// ONLY its own item (via FanOutItemKey), so everything it needs is encoded here.
type pageItem struct {
	DocID string `json:"d"`
	Page  int    `json:"p"`
}

// buildPipeline constructs the document-processing workflow on the given store
// for the given input. It is a standalone helper — the smoke test builds and
// drives the identical graph — recording every real effect into led.
func buildPipeline(store workflow.WorkflowStore, doc docInput, led *ledger) (*workflow.Workflow, error) {
	b := workflow.NewWorkflowBuilder().
		WithWorkflowID(workflowID(doc.docID)).
		WithStore(store)

	// seed: publish the doc's parameters into the shared data. The fan-out
	// expander and the choice read them from here.
	b.AddStartNode(nodeSeed).WithActionFunc(func(_ context.Context, data *workflow.WorkflowData) error {
		data.Set(keyDocID, doc.docID)
		data.Set(keyPages, int64(doc.pages))
		data.Set(keyDocType, doc.docType)
		return nil
	})

	// FAN-OUT over the document's pages. The expander resolves N items at run
	// time (the width the worker discovered); branchAction OCRs one page each,
	// recording the per-page effect. WithResults collects each branch's typed
	// result into page-ocr[i].
	b.AddFanOut(nodeOCR,
		func(_ context.Context, data *workflow.WorkflowData) ([]interface{}, error) {
			n, _ := data.GetInt64(keyPages)
			docID, _ := data.GetString(keyDocID)
			items := make([]interface{}, n)
			for i := range items {
				items[i] = pageItem{DocID: docID, Page: i}
			}
			return items, nil
		},
		workflow.ActionFunc(func(_ context.Context, data *workflow.WorkflowData) error {
			raw, _ := data.Get(workflow.FanOutItemKey)
			m, ok := raw.(map[string]interface{})
			if !ok {
				return fmt.Errorf("ocr branch: item is not a decoded object: %T", raw)
			}
			docID, _ := m["d"].(string)
			page := toInt(m["p"])
			led.record("ocr", fmt.Sprintf("%s/page%d", docID, page))
			data.Set("ocr-out", int64(page)) // the branch's typed result → page-ocr[page]
			return nil
		}),
	).WithResults(keyPageOCR, "ocr-out").DependsOn(nodeSeed)

	// CHOICE by document type. First-match routes to exactly one arm; the others
	// are Bypassed.
	b.AddChoice(nodeRoute).
		DependsOn(nodeOCR).
		When(func(d *workflow.WorkflowData) bool { return getStr(d, keyDocType) == "image" }, nodeTranscode).
		When(func(d *workflow.WorkflowData) bool { return getStr(d, keyDocType) == "text" }, nodeIndexArm).
		Otherwise(nodeSkipArm)

	// The three choice arms. transcode is the image path (records an effect);
	// index-arm and skip-arm are cheap markers so text/other docs converge.
	b.AddNode(nodeTranscode).DependsOn(nodeRoute).WithActionFunc(func(_ context.Context, d *workflow.WorkflowData) error {
		led.record("transcode", getStr(d, keyDocID))
		return nil
	})
	b.AddNode(nodeIndexArm).DependsOn(nodeRoute).WithActionFunc(noop)
	b.AddNode(nodeSkipArm).DependsOn(nodeRoute).WithActionFunc(noop)

	// A downstream node may not depend on multiple choice branches directly (that
	// is "unstructured reconvergence", a build error). A MERGE OR-joins the taken
	// arm — exactly one path reconverges once.
	b.AddMerge(nodeMerge).From(nodeTranscode, nodeIndexArm, nodeSkipArm)

	// SUB-WORKFLOW: the "index" child indexes the document as a distinct run with
	// its own journal, and writes its result back into the parent under keyIndexed.
	indexChild, err := buildIndexChild(led)
	if err != nil {
		return nil, fmt.Errorf("build index child: %w", err)
	}
	b.AddSubWorkflow(nodeIndex, indexChild).
		WithResult(keyIndexed, "indexed").
		DependsOn(nodeMerge)

	// APPROVAL gate: parks the run until an approve/reject decision is delivered
	// under the signal name equal to the node name ("approval").
	b.AddApproval(nodeApproval).DependsOn(nodeIndex)

	// WAIT-FOR-SIGNAL: after approval, park again until moderation is cleared.
	b.AddWaitForSignal(nodeModeration, nodeModeration).DependsOn(nodeApproval)

	// PUBLISH: the terminal effect, gated behind everything above.
	b.AddNode(nodePublish).DependsOn(nodeModeration).WithActionFunc(func(_ context.Context, d *workflow.WorkflowData) error {
		led.record("publish", getStr(d, keyDocID))
		d.Set(keyPublished, true)
		return nil
	})

	return workflow.FromBuilder(b)
}

// buildIndexChild is the "index" sub-workflow: a single-node child that records
// the index effect once and sets its "indexed" result (a scalar, so it
// round-trips type-faithfully through the store into the parent's keyIndexed).
func buildIndexChild(led *ledger) (*workflow.DAG, error) {
	cb := workflow.NewWorkflowBuilder()
	cb.AddStartNode("do-index").WithActionFunc(func(_ context.Context, d *workflow.WorkflowData) error {
		led.record("index", d.GetWorkflowID())
		d.Set("indexed", true)
		return nil
	})
	return cb.Build()
}

// run drives one document through the pipeline to its published terminal.
func run() error {
	dir, err := os.MkdirTemp("", "capstone-pipeline-*")
	if err != nil {
		return fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(dir) //nolint:errcheck // best-effort cleanup

	store, err := workflow.NewSQLiteStore(filepath.Join(dir, "pipeline.db"))
	if err != nil {
		return fmt.Errorf("open sqlite store: %w", err)
	}
	defer store.Close() //nolint:errcheck // best-effort cleanup

	doc := docInput{docID: "doc42", pages: 3, docType: "image"}
	led := &ledger{}

	wf, err := buildPipeline(store, doc, led)
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}

	if err := driveToCompletion(context.Background(), wf, store, doc.docID, 30*time.Second); err != nil {
		return err
	}

	// Read the durable result back, as an operator would.
	data, err := store.Load(workflowID(doc.docID))
	if err != nil {
		return fmt.Errorf("load: %w", err)
	}
	published, _ := data.GetBool(keyPublished)
	fmt.Printf("\nresult: published=%v\n", published)
	fmt.Printf("effects: ocr=%d transcode=%d index=%d publish=%d\n",
		led.count("ocr:"), led.count("transcode:"), led.count("index:"), led.count("publish:"))
	fmt.Printf("ledger: %v\n", led.snapshot())
	return nil
}

// driveToCompletion runs the workflow to completion across its two parks. The
// main loop re-drives Execute (a parked run returns ErrSuspended); a background
// driver — holding only the store — delivers the approval decision and the
// moderation signal once the run has parked on them.
func driveToCompletion(ctx context.Context, wf *workflow.Workflow, store *workflow.SQLiteStore, docID string, budget time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	// The store-only driver: it never sees the *Workflow. It reads the approval
	// nonce the executor stamped into the parked state and delivers a matching
	// decision, then clears moderation. Both deliveries are idempotent (deduped
	// by signal id), so retrying until parked is harmless.
	go driveSignals(ctx, store, workflowID(docID))

	for {
		err := wf.Execute(ctx)
		switch {
		case err == nil:
			return nil // completed
		case errors.Is(err, workflow.ErrSuspended):
			// Parked on approval or moderation. Give the driver a moment to
			// deliver, then re-drive to resume.
			select {
			case <-ctx.Done():
				return fmt.Errorf("pipeline did not complete before deadline (still parked)")
			case <-time.After(25 * time.Millisecond):
			}
		default:
			return fmt.Errorf("execute: %w", err)
		}
	}
}

// driveSignals is the store-only approval/moderation driver. It polls until the
// run has parked (the approval nonce is derivable only from a parked state), then
// delivers a matching approve decision and the moderation-cleared signal.
func driveSignals(ctx context.Context, store *workflow.SQLiteStore, wid string) {
	approved, cleared := false, false
	for {
		if ctx.Err() != nil {
			return
		}
		if !approved {
			// ApprovalNonceFromStore errors until the run has parked and stamped
			// its digest — which is exactly when we want to deliver — so an error
			// here just means "not parked yet, retry".
			if nonce, nerr := workflow.ApprovalNonceFromStore(store, wid, nodeApproval); nerr == nil {
				sig := workflow.ApproveSignal(nodeApproval, "capstone-approver", "looks good", "approve-1", nonce)
				if store.DeliverSignal(wid, sig) == nil {
					approved = true
				}
			}
		}
		if !cleared {
			// A plain signal needs a non-empty single-segment id; reuse the name.
			if store.DeliverSignal(wid, workflow.Signal{ID: nodeModeration, Name: nodeModeration}) == nil {
				cleared = true
			}
		}
		if approved && cleared {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(40 * time.Millisecond):
		}
	}
}

func workflowID(docID string) string { return "doc:" + docID }

func noop(_ context.Context, _ *workflow.WorkflowData) error { return nil }

func getStr(d *workflow.WorkflowData, k string) string {
	s, _ := d.GetString(k)
	return s
}

// toInt coerces a fan-out item's page number to an int. Items travel as JSON
// (the expansion journal is a JSON string), so a decoded number arrives as a
// json.Number (numbers are decoded with UseNumber), or a float64 / int.
func toInt(v interface{}) int {
	switch n := v.(type) {
	case json.Number:
		iv, _ := n.Int64()
		return int(iv)
	case float64:
		return int(n)
	case int64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("capstone-document-pipeline: %v", err)
	}
}
