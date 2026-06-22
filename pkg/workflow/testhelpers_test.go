package workflow

import "testing"

// mustAddNode / mustAddDep wrap the error-returning DAG construction methods in
// tests so a failed Add fatals the test instead of being silently dropped (the
// pattern the M5/Phase 14.1 A3 errcheck de-blinding required across the DAG
// construction tests). They mirror the benchmark-package helpers of the same
// name; kept here because that package's are not importable from package workflow.
func mustAddNode(t *testing.T, dag *DAG, node *Node) {
	t.Helper()
	if err := dag.AddNode(node); err != nil {
		t.Fatalf("AddNode(%v): %v", node, err)
	}
}

func mustAddDep(t *testing.T, dag *DAG, from, to string) {
	t.Helper()
	if err := dag.AddDependency(from, to); err != nil {
		t.Fatalf("AddDependency(%q,%q): %v", from, to, err)
	}
}
