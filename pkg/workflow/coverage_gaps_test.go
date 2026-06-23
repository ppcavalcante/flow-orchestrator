package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWithExecutionConfig asserts that a config set on the builder reaches the
// DAG produced by Build, and that omitting it leaves the DAG at DefaultConfig.
func TestWithExecutionConfig(t *testing.T) {
	t.Run("config propagates to the built DAG", func(t *testing.T) {
		b := NewWorkflowBuilder().
			WithExecutionConfig(ExecutionConfig{MaxConcurrency: 3})
		b.AddStartNode("a").WithAction(func(context.Context, *WorkflowData) error { return nil })

		dag, err := b.Build()
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if got := dag.config.MaxConcurrency; got != 3 {
			t.Errorf("MaxConcurrency = %d, want 3 (config did not propagate)", got)
		}
	})

	t.Run("returns the builder for chaining", func(t *testing.T) {
		b := NewWorkflowBuilder()
		if b.WithExecutionConfig(ExecutionConfig{MaxConcurrency: 1}) != b {
			t.Error("WithExecutionConfig should return the same builder for chaining")
		}
	})

	t.Run("absent config leaves DAG at DefaultConfig", func(t *testing.T) {
		b := NewWorkflowBuilder()
		b.AddStartNode("a").WithAction(func(context.Context, *WorkflowData) error { return nil })
		dag, err := b.Build()
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if got := dag.config.MaxConcurrency; got != DefaultMaxConcurrency {
			t.Errorf("MaxConcurrency = %d, want default %d", got, DefaultMaxConcurrency)
		}
	})
}

// TestBuildErrorPaths covers the error branches in WorkflowBuilder.Build:
// no-action node, unsupported-action node, and a missing dependency target.
func TestBuildErrorPaths(t *testing.T) {
	t.Run("node with no action", func(t *testing.T) {
		b := NewWorkflowBuilder()
		b.AddNode("orphan") // no WithAction
		_, err := b.Build()
		if err == nil || !strings.Contains(err.Error(), "no action defined") {
			t.Fatalf("expected no-action error, got %v", err)
		}
	})

	t.Run("node with unsupported action type", func(t *testing.T) {
		b := NewWorkflowBuilder()
		b.AddNode("bad").WithAction(42) // not an Action / supported func
		_, err := b.Build()
		if err == nil || !strings.Contains(err.Error(), "invalid action") {
			t.Fatalf("expected invalid-action error, got %v", err)
		}
	})

	t.Run("dependency on a node that was never added", func(t *testing.T) {
		b := NewWorkflowBuilder()
		b.AddNode("consumer").
			WithAction(func(context.Context, *WorkflowData) error { return nil }).
			DependsOn("ghost")
		_, err := b.Build()
		if err == nil || !strings.Contains(err.Error(), "ghost") {
			t.Fatalf("expected missing-dependency error mentioning ghost, got %v", err)
		}
	})
}

// TestDAGAddNodeDuplicate covers the duplicate-name error branch of DAG.AddNode.
func TestDAGAddNodeDuplicate(t *testing.T) {
	dag := NewDAG("dup")
	n1 := NewNode("x", ActionFunc(func(context.Context, *WorkflowData) error { return nil }))
	n2 := NewNode("x", ActionFunc(func(context.Context, *WorkflowData) error { return nil }))

	if err := dag.AddNode(n1); err != nil {
		t.Fatalf("first AddNode: %v", err)
	}
	err := dag.AddNode(n2)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected duplicate-name error, got %v", err)
	}
}

// TestDAGAddDependencyErrors covers the two missing-node branches of
// DAG.AddDependency (unknown from-node and unknown to-node).
func TestDAGAddDependencyErrors(t *testing.T) {
	dag := NewDAG("deps")
	known := NewNode("known", ActionFunc(func(context.Context, *WorkflowData) error { return nil }))
	if err := dag.AddNode(known); err != nil {
		t.Fatalf("AddNode: %v", err)
	}

	t.Run("unknown from-node", func(t *testing.T) {
		err := dag.AddDependency("missing", "known")
		if err == nil || !strings.Contains(err.Error(), "missing") {
			t.Fatalf("expected from-node error, got %v", err)
		}
	})

	t.Run("unknown to-node", func(t *testing.T) {
		err := dag.AddDependency("known", "missing")
		if err == nil || !strings.Contains(err.Error(), "missing") {
			t.Fatalf("expected to-node error, got %v", err)
		}
	})

	t.Run("both present links them", func(t *testing.T) {
		other := NewNode("other", ActionFunc(func(context.Context, *WorkflowData) error { return nil }))
		if err := dag.AddNode(other); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := dag.AddDependency("known", "other"); err != nil {
			t.Fatalf("AddDependency: %v", err)
		}
		// other now depends on known
		if len(other.DependsOn) != 1 || other.DependsOn[0].Name != "known" {
			t.Errorf("dependency not wired: %+v", other.DependsOn)
		}
	})
}

// TestGetFloat64Conversions exercises every numeric branch and the
// missing-key / wrong-type branches of GetFloat64.
func TestGetFloat64Conversions(t *testing.T) {
	d := NewWorkflowData("floats")
	d.Set("f64", float64(1.5))
	d.Set("f32", float32(2.5))
	d.Set("i", int(3))
	d.Set("i64", int64(4))
	d.Set("i32", int32(5))
	d.Set("str", "not a number")

	cases := []struct {
		key  string
		want float64
		ok   bool
	}{
		{"f64", 1.5, true},
		{"f32", 2.5, true},
		{"i", 3, true},
		{"i64", 4, true},
		{"i32", 5, true},
		{"str", 0, false},
		{"absent", 0, false},
	}
	for _, c := range cases {
		got, ok := d.GetFloat64(c.key)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("GetFloat64(%q) = (%v, %v), want (%v, %v)", c.key, got, ok, c.want, c.ok)
		}
	}
}

// TestGetIntConversions exercises the int/int64/int32 and wrong-type/missing
// branches of GetInt.
func TestGetIntConversions(t *testing.T) {
	d := NewWorkflowData("ints")
	d.Set("i", int(7))
	d.Set("i64", int64(8))
	d.Set("i32", int32(9))
	d.Set("f", float64(1.0)) // float is NOT accepted by GetInt
	d.Set("str", "x")

	cases := []struct {
		key  string
		want int
		ok   bool
	}{
		{"i", 7, true},
		{"i64", 8, true},
		{"i32", 9, true},
		{"f", 0, false},
		{"str", 0, false},
		{"absent", 0, false},
	}
	for _, c := range cases {
		got, ok := d.GetInt(c.key)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("GetInt(%q) = (%v, %v), want (%v, %v)", c.key, got, ok, c.want, c.ok)
		}
	}
}

// TestLoadSnapshotNumberFidelity covers the json.Number int64/float64 fallback
// branches added for lossless numeric round-trip in loadSnapshotInternal, plus
// the decode-error branch.
func TestLoadSnapshotNumberFidelity(t *testing.T) {
	t.Run("large int64 round-trips exactly via int64 branch", func(t *testing.T) {
		const big = int64(9223372036854775807) // MaxInt64 — would corrupt via float64
		d := NewWorkflowData("nums")
		d.Set("big", big)
		snap, err := d.Snapshot()
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}

		loaded := NewWorkflowData("loaded")
		if err := loaded.LoadSnapshot(snap); err != nil {
			t.Fatalf("LoadSnapshot: %v", err)
		}
		got, ok := loaded.GetInt64("big")
		if !ok || got != big {
			t.Errorf("GetInt64(big) = (%d, %v), want (%d, true)", got, ok, big)
		}
	})

	t.Run("real number round-trips via float64 branch", func(t *testing.T) {
		d := NewWorkflowData("nums")
		d.Set("pi", 3.14159)
		snap, err := d.Snapshot()
		if err != nil {
			t.Fatalf("Snapshot: %v", err)
		}
		loaded := NewWorkflowData("loaded")
		if err := loaded.LoadSnapshot(snap); err != nil {
			t.Fatalf("LoadSnapshot: %v", err)
		}
		got, ok := loaded.GetFloat64("pi")
		if !ok || got != 3.14159 {
			t.Errorf("GetFloat64(pi) = (%v, %v), want (3.14159, true)", got, ok)
		}
	})

	t.Run("malformed JSON returns a decode error", func(t *testing.T) {
		d := NewWorkflowData("bad")
		if err := d.LoadSnapshot([]byte("{not json")); err == nil {
			t.Error("expected decode error on malformed snapshot, got nil")
		}
	})
}

// TestIOErrorUnwrap covers both arms of ioError.Unwrap: with and without a cause.
func TestIOErrorUnwrap(t *testing.T) {
	t.Run("with cause exposes ErrIO and the cause", func(t *testing.T) {
		cause := errors.New("disk gone")
		err := newIOError("save", "wf-1", cause)
		if !errors.Is(err, ErrIO) {
			t.Error("errors.Is(err, ErrIO) should hold")
		}
		if !errors.Is(err, cause) {
			t.Error("errors.Is(err, cause) should reach the wrapped cause")
		}
		if strings.Contains(err.Error(), "disk gone") {
			t.Errorf("Error() must stay path/cause-free, got %q", err.Error())
		}
	})

	t.Run("nil cause still unwraps to ErrIO only", func(t *testing.T) {
		err := newIOError("load", "wf-2", nil)
		if !errors.Is(err, ErrIO) {
			t.Error("errors.Is(err, ErrIO) should hold even with nil cause")
		}
	})
}

// TestSaveLoadJSONRoundTrip covers the SaveToJSON / LoadFromJSON happy path and
// the LoadFromJSON read-error branch.
func TestSaveLoadJSONRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wf.json")

	d := NewWorkflowData("rt")
	d.Set("k", "v")
	d.SetNodeStatus("n", Completed)
	if err := d.SaveToJSON(path); err != nil {
		t.Fatalf("SaveToJSON: %v", err)
	}

	loaded := NewWorkflowData("rt2")
	if err := loaded.LoadFromJSON(path); err != nil {
		t.Fatalf("LoadFromJSON: %v", err)
	}
	if got, _ := loaded.Get("k"); got != "v" {
		t.Errorf("round-trip lost data: Get(k) = %v", got)
	}
	if st, ok := loaded.GetNodeStatus("n"); !ok || st != Completed {
		t.Errorf("round-trip lost node status: got (%v, %v), want (Completed, true)", st, ok)
	}

	t.Run("missing file yields a read error", func(t *testing.T) {
		err := NewWorkflowData("x").LoadFromJSON(filepath.Join(dir, "does-not-exist.json"))
		if err == nil || !os.IsNotExist(errors.Unwrap(err)) {
			// LoadFromJSON wraps os.ReadFile's error; the underlying cause is NotExist.
			t.Fatalf("expected not-exist read error, got %v", err)
		}
	})
}

// TestIsNodeRunnable asserts the status gate of IsNodeRunnable: terminal/running
// statuses are not runnable; unknown/pending nodes are.
func TestIsNodeRunnable(t *testing.T) {
	d := NewWorkflowData("runnable")

	if !d.IsNodeRunnable("never-seen") {
		t.Error("an unknown node should be runnable (pending from the data view)")
	}

	d.SetNodeStatus("pending", Pending)
	if !d.IsNodeRunnable("pending") {
		t.Error("a Pending node should be runnable")
	}

	for _, st := range []NodeStatus{Running, Completed, Failed, Skipped} {
		d.SetNodeStatus("n", st)
		if d.IsNodeRunnable("n") {
			t.Errorf("a %s node must not be runnable", st)
		}
	}
}

// TestForEachOutput covers ForEachOutput including the map-clone branch that
// deep-copies map outputs to prevent shared-mutable-state aliasing.
func TestForEachOutput(t *testing.T) {
	d := NewWorkflowData("outputs")
	d.SetOutput("scalar", 42)
	original := map[string]interface{}{"inner": "v"}
	d.SetOutput("mapped", original)

	seen := make(map[string]interface{})
	d.ForEachOutput(func(name string, out interface{}) {
		seen[name] = out
	})

	if seen["scalar"] != 42 {
		t.Errorf("scalar output = %v, want 42", seen["scalar"])
	}
	m, ok := seen["mapped"].(map[string]interface{})
	if !ok || m["inner"] != "v" {
		t.Fatalf("mapped output = %v, want a map with inner=v", seen["mapped"])
	}
	// The iterated map must be a clone, not the stored reference: mutating it
	// must not affect a subsequent read.
	m["inner"] = "mutated"
	got, _ := d.GetOutput("mapped")
	gm, ok := got.(map[string]interface{})
	if !ok || gm["inner"] != "v" {
		t.Errorf("ForEachOutput exposed the live map; stored value was mutated to %v", got)
	}
}

// TestJSONFileStoreDelete covers JSONFileStore.Delete: invalid id, not-found,
// and successful delete.
func TestJSONFileStoreDelete(t *testing.T) {
	store, err := NewJSONFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewJSONFileStore: %v", err)
	}

	t.Run("invalid workflow id is rejected", func(t *testing.T) {
		if err := store.Delete("../escape"); err == nil {
			t.Error("expected validation error for traversal id")
		}
	})

	t.Run("deleting an absent workflow reports ErrNotFound", func(t *testing.T) {
		err := store.Delete("absent")
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("delete after save succeeds", func(t *testing.T) {
		d := NewWorkflowData("wf") // Save derives the store key from data.ID
		d.Set("k", "v")
		if err := store.Save(d); err != nil {
			t.Fatalf("Save: %v", err)
		}
		if err := store.Delete("wf"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		// Second delete now reports not-found.
		if err := store.Delete("wf"); !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound on second delete, got %v", err)
		}
	})
}

// TestFlatBuffersStoreDelete covers FlatBuffersStore.Delete (the recommended
// store): invalid id, not-found, and delete-after-save, plus a Save/Load
// round-trip to exercise the happy path.
func TestFlatBuffersStoreDelete(t *testing.T) {
	store, err := NewFlatBuffersStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFlatBuffersStore: %v", err)
	}

	t.Run("invalid workflow id is rejected", func(t *testing.T) {
		if err := store.Delete("a/b"); err == nil {
			t.Error("expected validation error for id with separator")
		}
	})

	t.Run("deleting an absent workflow reports ErrNotFound", func(t *testing.T) {
		if err := store.Delete("absent"); !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("save, load round-trip, then delete", func(t *testing.T) {
		d := NewWorkflowData("fbwf")
		d.Set("k", "v")
		d.SetNodeStatus("n", Completed)
		if err := store.Save(d); err != nil {
			t.Fatalf("Save: %v", err)
		}

		loaded, err := store.Load("fbwf")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got, _ := loaded.Get("k"); got != "v" {
			t.Errorf("round-trip lost data: Get(k) = %v", got)
		}

		if err := store.Delete("fbwf"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if err := store.Delete("fbwf"); !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound on second delete, got %v", err)
		}
	})
}
