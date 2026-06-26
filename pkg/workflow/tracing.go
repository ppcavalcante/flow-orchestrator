package workflow

import (
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// tracerScope is the OpenTelemetry instrumentation scope name for this
// library's span emission (the module path, per OTel convention). It matches
// the metrics bridge scope (pkg/workflow/metrics) so traces and metrics from
// this library carry a consistent instrumentation identity.
const tracerScope = "github.com/ppcavalcante/flow-orchestrator"

// Span/attribute names emitted by the executor. The node span is named after
// the node itself (a readable trace waterfall — span "fetch-user", not a
// generic verb); the workflow (parent) span carries a fixed name. Attribute
// keys are a closed, code-defined set — never workflow-controlled strings —
// so trace backends see bounded cardinality on the keys (DEC-CHUNK5).
const (
	workflowSpanName    = "workflow.execute"
	attrNodeStatus      = "node.status"
	attrNodeRetryCount  = "node.retry_count"
	attrWorkflowSkipped = "workflow.skipped_count"
)

// resolveTracer returns the trace.Tracer to use for a run. A nil provider
// (the zero value of ExecutionConfig.TracerProvider — tracing off) resolves
// to the noop tracer once here, so the hot path is an unconditional
// tracer.Start whose noop implementation short-circuits to zero work. The
// resolution is API-only: the library never imports the OTel SDK; the host
// owns the real provider (DEC-M6-otel-api-only parity).
func resolveTracer(tp trace.TracerProvider) trace.Tracer {
	if tp == nil {
		tp = noop.NewTracerProvider()
	}
	return tp.Tracer(tracerScope)
}

// nodeSpanAttributes builds the closed-set attributes for a finished node span.
// It reads ONLY engine-controlled values — the node's name is already the span
// name, the final status comes from WorkflowData, and the retry count is the
// node's configured RetryCount (attached only when > 0 to keep spans lean). No
// WorkflowData values, keys, paths, or arbitrary state are read here: the
// chunk-2 no-leak discipline extends to traces (the action's own error string,
// recorded via span.RecordError at the call site, is the action's contract —
// nothing more from our side).
func nodeSpanAttributes(n *Node, status NodeStatus) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 2)
	attrs = append(attrs, attribute.String(attrNodeStatus, string(status)))
	if n.RetryCount > 0 {
		attrs = append(attrs, attribute.Int(attrNodeRetryCount, n.RetryCount))
	}
	return attrs
}
