package workflow

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"
	"go.opentelemetry.io/otel/trace/noop"
)

// --- A minimal in-memory recording tracer, API-only -------------------------
//
// These types implement the go.opentelemetry.io/otel/trace API directly (no
// SDK) so tests can assert on emitted spans without pulling the SDK into the
// library's test dependency tree. Unimplemented methods are inherited from the
// noop embeds; we only record what we assert on: span name, attributes, status,
// recorded errors, and parent/child linkage (via a synthetic span-id chain).

type recordedSpan struct {
	name       string
	attrs      map[string]attribute.Value
	statusCode codes.Code
	statusDesc string
	errs       []error
	parentID   uint64
	spanID     uint64
	ended      bool
}

func (s *recordedSpan) attrInt(key string) (int64, bool) {
	v, ok := s.attrs[key]
	if !ok {
		return 0, false
	}
	return v.AsInt64(), true
}

func (s *recordedSpan) attrString(key string) (string, bool) {
	v, ok := s.attrs[key]
	if !ok {
		return "", false
	}
	return v.AsString(), true
}

type recordingProvider struct {
	embedded.TracerProvider
	mu     sync.Mutex
	spans  []*recordedSpan
	nextID uint64
}

func newRecordingProvider() *recordingProvider { return &recordingProvider{} }

func (p *recordingProvider) Tracer(string, ...trace.TracerOption) trace.Tracer {
	return &recordingTracer{p: p}
}

// finishedSpans returns a snapshot of the spans created so far.
func (p *recordingProvider) finishedSpans() []*recordedSpan {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*recordedSpan, len(p.spans))
	copy(out, p.spans)
	return out
}

func (p *recordingProvider) spanNames() []string {
	spans := p.finishedSpans()
	names := make([]string, 0, len(spans))
	for _, s := range spans {
		names = append(names, s.name)
	}
	sort.Strings(names)
	return names
}

// findByName returns the first recorded span with the given name (nil if none).
func (p *recordingProvider) findByName(name string) *recordedSpan {
	for _, s := range p.finishedSpans() {
		if s.name == name {
			return s
		}
	}
	return nil
}

type recordingTracer struct {
	embedded.Tracer
	p *recordingProvider
}

// spanIDKey carries our synthetic span id through context so a child span can
// record its parent. We do not use real SpanContext propagation (that needs
// the SDK); a uint64 in context is enough to assert parent/child linkage.
type spanIDKey struct{}

func (t *recordingTracer) Start(ctx context.Context, name string, _ ...trace.SpanStartOption) (context.Context, trace.Span) {
	t.p.mu.Lock()
	t.p.nextID++
	id := t.p.nextID
	var parent uint64
	if pid, ok := ctx.Value(spanIDKey{}).(uint64); ok {
		parent = pid
	}
	rs := &recordedSpan{
		name:     name,
		attrs:    make(map[string]attribute.Value),
		spanID:   id,
		parentID: parent,
	}
	t.p.spans = append(t.p.spans, rs)
	t.p.mu.Unlock()

	ctx = context.WithValue(ctx, spanIDKey{}, id)
	return ctx, &recordingSpanHandle{p: t.p, rs: rs}
}

type recordingSpanHandle struct {
	noop.Span // inherit no-op impls for methods we do not record
	p         *recordingProvider
	rs        *recordedSpan
}

func (h *recordingSpanHandle) End(...trace.SpanEndOption) {
	h.p.mu.Lock()
	defer h.p.mu.Unlock()
	h.rs.ended = true
}

func (h *recordingSpanHandle) SetName(name string) {
	h.p.mu.Lock()
	defer h.p.mu.Unlock()
	h.rs.name = name
}

func (h *recordingSpanHandle) SetAttributes(kv ...attribute.KeyValue) {
	h.p.mu.Lock()
	defer h.p.mu.Unlock()
	for _, a := range kv {
		h.rs.attrs[string(a.Key)] = a.Value
	}
}

func (h *recordingSpanHandle) SetStatus(code codes.Code, desc string) {
	h.p.mu.Lock()
	defer h.p.mu.Unlock()
	h.rs.statusCode = code
	h.rs.statusDesc = desc
}

func (h *recordingSpanHandle) RecordError(err error, _ ...trace.EventOption) {
	if err == nil {
		return
	}
	h.p.mu.Lock()
	defer h.p.mu.Unlock()
	h.rs.errs = append(h.rs.errs, err)
}

func (h *recordingSpanHandle) IsRecording() bool { return true }

// allRecordedText concatenates every span name, attribute key, attribute string
// value, status description, and recorded error string — the full surface a
// leaked secret could land in. Used by the no-leak assertion.
func (p *recordingProvider) allRecordedText() string {
	var b strings.Builder
	for _, s := range p.finishedSpans() {
		b.WriteString(s.name)
		b.WriteByte('\n')
		b.WriteString(s.statusDesc)
		b.WriteByte('\n')
		for k, v := range s.attrs {
			b.WriteString(k)
			b.WriteByte('=')
			if v.Type() == attribute.STRING {
				b.WriteString(v.AsString())
			} else {
				b.WriteString(v.Emit())
			}
			b.WriteByte('\n')
		}
		for _, e := range s.errs {
			b.WriteString(e.Error())
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// --- helpers ----------------------------------------------------------------

func traceOKAction(string) Action {
	return ActionFunc(func(_ context.Context, _ *WorkflowData) error { return nil })
}

func traceFailAction(msg string) Action {
	return ActionFunc(func(_ context.Context, _ *WorkflowData) error { return errors.New(msg) })
}

// buildDAG wires a small DAG directly (avoiding the builder) so tests control
// node config precisely. nodes is name->action; deps is child->parents.
func buildDAG(t *testing.T, tp trace.TracerProvider, nodes map[string]Action, deps map[string][]string) *DAG {
	t.Helper()
	d := NewDAG("test-dag")
	for name, act := range nodes {
		if err := d.AddNode(NewNode(name, act)); err != nil {
			t.Fatalf("AddNode(%s): %v", name, err)
		}
	}
	for child, parents := range deps {
		for _, p := range parents {
			if err := d.AddDependency(p, child); err != nil {
				t.Fatalf("AddDependency(%s->%s): %v", p, child, err)
			}
		}
	}
	if tp != nil {
		d.WithTracerProvider(tp)
	}
	return d
}

// --- tests ------------------------------------------------------------------

// One span per executed node, named after the node, plus the parent workflow
// span; node spans are children of the parent.
func TestTracing_SpanPerExecutedNode(t *testing.T) {
	tp := newRecordingProvider()
	d := buildDAG(t, tp,
		map[string]Action{"a": traceOKAction("a"), "b": traceOKAction("b"), "c": traceOKAction("c")},
		map[string][]string{"b": {"a"}, "c": {"a"}},
	)
	data := NewWorkflowData("wf")
	if err := d.Execute(context.Background(), data); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	got := tp.spanNames()
	want := []string{"a", "b", "c", workflowSpanName}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("span names = %v, want %v", got, want)
	}

	// Every node span is a child of the parent workflow span.
	parent := tp.findByName(workflowSpanName)
	if parent == nil {
		t.Fatal("no workflow parent span")
	}
	for _, n := range []string{"a", "b", "c"} {
		s := tp.findByName(n)
		if s.parentID != parent.spanID {
			t.Errorf("node %s span parentID=%d, want parent span %d", n, s.parentID, parent.spanID)
		}
		if !s.ended {
			t.Errorf("node %s span was not ended", n)
		}
	}
	if !parent.ended {
		t.Error("workflow span was not ended")
	}
}

// Completed nodes carry node.status=completed; retry_count attribute appears
// only when RetryCount > 0.
func TestTracing_StatusAndRetryAttributes(t *testing.T) {
	tp := newRecordingProvider()
	d := NewDAG("d")
	plain := NewNode("plain", traceOKAction("plain"))
	retried := NewNode("retried", traceOKAction("retried")).WithRetries(3)
	if err := d.AddNode(plain); err != nil {
		t.Fatalf("AddNode(plain): %v", err)
	}
	if err := d.AddNode(retried); err != nil {
		t.Fatalf("AddNode(retried): %v", err)
	}
	d.WithTracerProvider(tp)

	if err := d.Execute(context.Background(), NewWorkflowData("wf")); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	ps := tp.findByName("plain")
	if v, _ := ps.attrString(attrNodeStatus); v != string(Completed) {
		t.Errorf("plain node.status=%q, want %q", v, Completed)
	}
	if _, ok := ps.attrInt(attrNodeRetryCount); ok {
		t.Error("plain node should not carry retry_count (RetryCount==0)")
	}

	rs := tp.findByName("retried")
	if v, ok := rs.attrInt(attrNodeRetryCount); !ok || v != 3 {
		t.Errorf("retried node retry_count=%d ok=%v, want 3", v, ok)
	}
}

// A failed node records the error and sets span status to Error.
func TestTracing_FailureRecordsErrorAndStatus(t *testing.T) {
	tp := newRecordingProvider()
	d := buildDAG(t, tp,
		map[string]Action{"boom": traceFailAction("kaboom")},
		nil,
	)
	err := d.Execute(context.Background(), NewWorkflowData("wf"))
	if err == nil {
		t.Fatal("expected execution error")
	}

	s := tp.findByName("boom")
	if s.statusCode != codes.Error {
		t.Errorf("status code = %v, want Error", s.statusCode)
	}
	if len(s.errs) != 1 {
		t.Fatalf("recorded %d errors, want 1", len(s.errs))
	}
	if v, _ := s.attrString(attrNodeStatus); v != string(Failed) {
		t.Errorf("node.status=%q, want %q", v, Failed)
	}
}

// Off-by-default: a nil TracerProvider emits ZERO spans and the workflow still
// runs correctly (the noop path is exercised, not the recording one).
func TestTracing_OffByDefaultEmitsNoSpans(t *testing.T) {
	// No WithTracerProvider call at all.
	d := buildDAG(t, nil,
		map[string]Action{"a": traceOKAction("a"), "b": traceOKAction("b")},
		map[string][]string{"b": {"a"}},
	)
	data := NewWorkflowData("wf")
	if err := d.Execute(context.Background(), data); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if st, _ := data.GetNodeStatus("b"); st != Completed {
		t.Fatalf("node b status=%v, want completed (workflow must run with tracing off)", st)
	}
	// Nothing to assert on spans because no provider was wired; the point is
	// Execute must not panic and must resolve a noop tracer. An explicit noop
	// provider is covered by TestTracing_NoopProviderSafe.
}

// An explicit noop provider is safe and records nothing.
func TestTracing_NoopProviderSafe(t *testing.T) {
	d := buildDAG(t, noop.NewTracerProvider(),
		map[string]Action{"a": traceOKAction("a")},
		nil,
	)
	if err := d.Execute(context.Background(), NewWorkflowData("wf")); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

// No-leak: a secret planted in WorkflowData must never appear in any span name,
// attribute, status description, or recorded error. The action reads the secret
// (so it is genuinely in the data plane) and fails with its OWN message — the
// span must carry the action's message but nothing from WorkflowData.
func TestTracing_NoWorkflowDataLeak(t *testing.T) {
	const secret = "TOP-SECRET-VALUE-9f3a"
	tp := newRecordingProvider()

	leakyOK := ActionFunc(func(_ context.Context, data *WorkflowData) error {
		data.Set("api_key", secret)
		return nil
	})
	leakyFail := ActionFunc(func(_ context.Context, data *WorkflowData) error {
		_, _ = data.Get("api_key")
		return errors.New("downstream failed") // action's own message, no secret
	})

	d := buildDAG(t, tp,
		map[string]Action{"writer": leakyOK, "reader": leakyFail},
		map[string][]string{"reader": {"writer"}},
	)

	if err := d.Execute(context.Background(), NewWorkflowData("wf")); err == nil {
		t.Fatal("expected execution to fail (reader returns an error)")
	}

	text := tp.allRecordedText()
	if strings.Contains(text, secret) {
		t.Fatalf("secret leaked into span data:\n%s", text)
	}
	// Sanity: the action's own error message DID flow through (proves the span
	// surface is actually populated, so the no-leak assertion is meaningful).
	if !strings.Contains(text, "downstream failed") {
		t.Fatalf("expected action error in span surface, got:\n%s", text)
	}
}

// Skipped nodes get NO span; the parent workflow span carries skipped_count.
func TestTracing_SkippedNodesNoSpanParentCount(t *testing.T) {
	tp := newRecordingProvider()
	// a (fails, fail-fast) -> b -> c. b and c become Skipped.
	d := buildDAG(t, tp,
		map[string]Action{"a": traceFailAction("a-failed"), "b": traceOKAction("b"), "c": traceOKAction("c")},
		map[string][]string{"b": {"a"}, "c": {"b"}},
	)
	data := NewWorkflowData("wf")
	if err := d.Execute(context.Background(), data); err == nil {
		t.Fatal("expected failure")
	}

	// b and c must be Skipped (precondition for the trace assertion).
	for _, n := range []string{"b", "c"} {
		if st, _ := data.GetNodeStatus(n); st != Skipped {
			t.Fatalf("precondition: node %s status=%v, want Skipped", n, st)
		}
	}

	// Only "a" and the parent span exist; no spans for b or c.
	if s := tp.findByName("b"); s != nil {
		t.Error("skipped node b must not produce a span")
	}
	if s := tp.findByName("c"); s != nil {
		t.Error("skipped node c must not produce a span")
	}
	if s := tp.findByName("a"); s == nil {
		t.Error("failed-but-executed node a must produce a span")
	}

	parent := tp.findByName(workflowSpanName)
	if parent == nil {
		t.Fatal("no parent span")
	}
	if v, ok := parent.attrInt(attrWorkflowSkipped); !ok || v != 2 {
		t.Errorf("workflow.skipped_count=%d ok=%v, want 2", v, ok)
	}
}

// The builder's WithTracerProvider wires tracing onto the produced DAG, and
// survives a custom execution config set on the same builder.
func TestTracing_BuilderWithTracerProvider(t *testing.T) {
	tp := newRecordingProvider()
	dag, err := NewWorkflowBuilder().
		WithExecutionConfig(ExecutionConfig{MaxConcurrency: 2}).
		WithTracerProvider(tp).
		AddStartNode("only").WithAction(traceOKAction("only")).workflow.
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := dag.Execute(context.Background(), NewWorkflowData("wf")); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if s := tp.findByName("only"); s == nil {
		t.Fatal("builder WithTracerProvider did not wire tracing (no node span)")
	}
}

// benchDAG builds a small fan-out DAG used by the tracing-overhead benchmarks.
func benchDAG(tb testing.TB, tp trace.TracerProvider) *DAG {
	tb.Helper()
	d := NewDAG("bench")
	if err := d.AddNode(NewNode("root", traceOKAction("root"))); err != nil {
		tb.Fatalf("AddNode(root): %v", err)
	}
	for _, n := range []string{"a", "b", "c", "d"} {
		if err := d.AddNode(NewNode(n, traceOKAction(n))); err != nil {
			tb.Fatalf("AddNode(%s): %v", n, err)
		}
		if err := d.AddDependency("root", n); err != nil {
			tb.Fatalf("AddDependency(root->%s): %v", n, err)
		}
	}
	if tp != nil {
		d.WithTracerProvider(tp)
	}
	return d
}

// benchErrSink keeps Execute's error from being elided and satisfies errcheck
// (check-blank) without per-iteration branching in the hot loop.
var benchErrSink error

// BenchmarkExecute_NoTracing is the baseline: no provider wired at all.
func BenchmarkExecute_NoTracing(b *testing.B) {
	d := benchDAG(b, nil)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchErrSink = d.Execute(ctx, NewWorkflowData("wf"))
	}
}

// BenchmarkExecute_TracingOff measures the cost of the resolved-noop-tracer
// path: tracing is "off" but the span machinery (resolveTracer + noop
// tracer.Start/End) is present. Compared against BenchmarkExecute_NoTracing
// this quantifies the off-path overhead the contract asked us to measure.
func BenchmarkExecute_TracingOff(b *testing.B) {
	d := benchDAG(b, noop.NewTracerProvider())
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchErrSink = d.Execute(ctx, NewWorkflowData("wf"))
	}
}
