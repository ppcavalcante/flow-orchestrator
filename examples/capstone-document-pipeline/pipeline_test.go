package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ppcavalcante/flow-orchestrator/pkg/workflow"
)

// TestCapstone_ReachesPublished drives the exact pipeline the command builds and
// asserts it reaches its published terminal with the expected per-stage effects:
// one OCR per page (fan-out), the image arm transcoded (choice), the index
// sub-workflow ran, and publish recorded — across the two parks (approval +
// moderation), resumed by the store-only driver.
//
// The pipeline uses a real SQLite store and a polling driver loop, so it is
// guarded behind testing.Short(): `go test -short` skips it.
func TestCapstone_ReachesPublished(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping capstone pipeline (SQLite + park/resume driver loop) in -short mode")
	}

	store, err := workflow.NewSQLiteStore(filepath.Join(t.TempDir(), "pipeline.db"))
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	doc := docInput{docID: "doc42", pages: 3, docType: "image"}
	led := &ledger{}

	wf, err := buildPipeline(store, doc, led)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if err := driveToCompletion(context.Background(), wf, store, doc.docID, 20*time.Second); err != nil {
		t.Fatalf("drive: %v (ledger=%v)", err, led.snapshot())
	}

	// --- terminal durable state -------------------------------------------------
	data, err := store.Load(workflowID(doc.docID))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if published, ok := data.GetBool(keyPublished); !ok || !published {
		t.Errorf("published = %v (ok=%v), want true", published, ok)
	}

	// --- per-stage effects (exactly once each) ---------------------------------
	for _, tc := range []struct {
		stage  string
		prefix string
		want   int
	}{
		{"OCR (one per page)", "ocr:", 3},
		{"transcode (image arm)", "transcode:", 1},
		{"index (sub-workflow)", "index:", 1},
		{"publish (terminal)", "publish:", 1},
	} {
		if got := led.count(tc.prefix); got != tc.want {
			t.Errorf("%s: %d effect(s), want %d (ledger=%v)", tc.stage, got, tc.want, led.snapshot())
		}
	}

	// --- choice routed to the image arm; the other arms were bypassed ----------
	assertStatus(t, data, nodeTranscode, workflow.Completed)
	assertStatus(t, data, nodeIndexArm, workflow.Bypassed)
	assertStatus(t, data, nodeSkipArm, workflow.Bypassed)

	// --- the gate and the terminal completed -----------------------------------
	for _, node := range []string{nodeApproval, nodeModeration, nodePublish} {
		assertStatus(t, data, node, workflow.Completed)
	}

	// --- the fan-out collected N typed per-branch results ----------------------
	if n, ok := data.GetInt64(keyPageOCR + ".__count__"); !ok || n != 3 {
		t.Errorf("%s.__count__ = %d (ok=%v), want 3", keyPageOCR, n, ok)
	}
}

func assertStatus(t *testing.T, data *workflow.WorkflowData, node string, want workflow.NodeStatus) {
	t.Helper()
	if st, ok := data.GetNodeStatus(node); !ok || st != want {
		t.Errorf("node %q status = %v (ok=%v), want %v", node, st, ok, want)
	}
}
