package workflow

// M19 ph90 approval-gate ADVERSARIAL suite (independent of the author's
// approval_test.go). The author covers the happy contract: park→approve, reject
// fail-fast (InMemory), decode-after-JSONFile-round-trip, no-double-apply, and a
// decoder table over its own fixture shapes. This file attacks the gaps those
// structurally miss, straight at the SECURITY-RELEVANT core of an authorization
// gate:
//
//	THE INVARIANT: a REJECT or a CORRUPT/AMBIGUOUS payload must NEVER decode to
//	Approved=true, and must NEVER drive the gate to Completed with downstream
//	running. A phantom approve = a rejection or garbage silently authorizing
//	downstream work. That is the worst-case defect (CRITICAL).
//
// Attack surface generated here (shapes the author's fixtures EXCLUDE):
//   - the durable round-trip across ALL THREE SignalStores (InMemory keeps the
//     typed struct; JSONFile + FlatBuffers JSON-round-trip to map[string]any) —
//     the author round-tripped only JSONFile and only for approve;
//   - map[string]any with Approved as json.Number / string / float64 / nil / a
//     nested map / a slice; missing Approved; Approver/Comment as non-strings;
//   - the round-tripped-key-casing guard (a mismatch would turn EVERY approve
//     into a silent reject, or worse);
//   - end-to-end delivery of corrupt payloads through real stores → the gate
//     never Completes, downstream never runs;
//   - a coverage-guided fuzz of decodeApprovalDecision (never panics; never
//     returns (Approved=true, nil) for an input that did not carry a bool true).

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// approvalSignalStores returns a fresh factory per store name for ALL THREE
// SignalStore implementations — the InMemory (typed-struct, no serialization),
// JSONFile (JSON round-trip), and FlatBuffers (JSON-in-FB round-trip) paths.
func approvalSignalStores(t *testing.T) map[string]func() WorkflowStore {
	t.Helper()
	return map[string]func() WorkflowStore{
		"InMemory": func() WorkflowStore { return NewInMemoryStore() },
		"JSONFile": func() WorkflowStore {
			s, err := NewJSONFileStore(t.TempDir())
			require.NoError(t, err)
			return s
		},
		"FlatBuffers": func() WorkflowStore {
			s, err := NewFlatBuffersStore(t.TempDir())
			require.NoError(t, err)
			return s
		},
	}
}

// TestApproval_Adversarial_DecodeNeverPhantomApprove — the CRITICAL invariant at
// the decoder in isolation, over shapes the author's fixtures do not carry. Every
// input here is a REJECT, a CORRUPT, or an AMBIGUOUS payload — NONE is a genuine
// bool-true approve. The bar: each MUST return either a non-nil error OR
// Approved=false. A single (Approved=true, err=nil) is a phantom approve (CRITICAL).
func TestApproval_Adversarial_DecodeNeverPhantomApprove(t *testing.T) {
	type nested struct{ X int }
	cases := []struct {
		name    string
		payload any
	}{
		// Approved carried as a NON-bool — the post-round-trip shapes JSON produces
		// (json.Number) plus the classic "truthy" spoofs.
		{"approved_jsonnumber_1", map[string]any{"Approved": json.Number("1")}},
		{"approved_jsonnumber_0", map[string]any{"Approved": json.Number("0")}},
		{"approved_string_true", map[string]any{"Approved": "true"}},
		{"approved_string_True", map[string]any{"Approved": "True"}},
		{"approved_string_1", map[string]any{"Approved": "1"}},
		{"approved_string_yes", map[string]any{"Approved": "yes"}},
		{"approved_float64_1", map[string]any{"Approved": float64(1)}},
		{"approved_float64_0", map[string]any{"Approved": float64(0)}},
		{"approved_int_1", map[string]any{"Approved": 1}},
		{"approved_int64_1", map[string]any{"Approved": int64(1)}},
		{"approved_nil", map[string]any{"Approved": nil}},
		{"approved_nested_map", map[string]any{"Approved": map[string]any{"Approved": true}}},
		{"approved_slice", map[string]any{"Approved": []any{true}}},
		// Missing Approved entirely — the author checks {Approver} but not with
		// competing keys; verify no key-shape resurrects an approve.
		{"missing_approved_with_approver", map[string]any{"Approver": "x", "Comment": "y"}},
		{"missing_approved_extra_keys", map[string]any{"approved": true, "APPROVED": true, "Extra": "z"}},
		{"nil_map", map[string]any(nil)},
		{"empty_map", map[string]any{}},
		// Approver / Comment wrong-typed while Approved is absent — must error, not
		// silently approve.
		{"approver_jsonnumber", map[string]any{"Approver": json.Number("5")}},
		{"approver_nested_map", map[string]any{"Approver": map[string]any{"k": "v"}}},
		{"comment_slice", map[string]any{"Comment": []any{"c"}}},
		// Non-map, non-struct payloads — the durable/typed default arm.
		{"bare_string", "a bare string"},
		{"bare_true_bool", true},
		{"bare_int", 1},
		{"bare_float", 1.0},
		{"bare_jsonnumber", json.Number("1")},
		{"bare_slice", []any{"Approved", true}},
		{"nil_interface", nil},
		{"foreign_struct", nested{X: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeApprovalDecision(tc.payload)
			if err == nil {
				assert.Falsef(t, got.Approved,
					"PHANTOM APPROVE: payload %#v decoded to (Approved=true, err=nil)", tc.payload)
			}
		})
	}
}

// TestApproval_Adversarial_OnlyGenuineApproveDecodesTrue — the dual bound: the
// ONLY inputs that legitimately yield Approved=true are the typed struct, a
// non-nil *ApprovalDecision, and a map whose "Approved" is a genuine bool true.
// Also pins the reject variants of each shape to Approved=false, and audit-field
// fidelity.
func TestApproval_Adversarial_OnlyGenuineApproveDecodesTrue(t *testing.T) {
	approveShapes := []struct {
		name    string
		payload any
	}{
		{"typed", ApprovalDecision{Approved: true, Approver: "alice", Comment: "ship"}},
		{"ptr", &ApprovalDecision{Approved: true, Approver: "alice", Comment: "ship"}},
		{"map_bool_true", map[string]any{"Approved": true, "Approver": "alice", "Comment": "ship"}},
	}
	for _, s := range approveShapes {
		t.Run("approve_"+s.name, func(t *testing.T) {
			got, err := decodeApprovalDecision(s.payload)
			require.NoError(t, err)
			assert.True(t, got.Approved)
			assert.Equal(t, "alice", got.Approver)
			assert.Equal(t, "ship", got.Comment)
		})
	}
	rejectShapes := []struct {
		name    string
		payload any
	}{
		{"typed", ApprovalDecision{Approved: false, Approver: "bob", Comment: "no"}},
		{"ptr", &ApprovalDecision{Approved: false, Approver: "bob", Comment: "no"}},
		{"map_bool_false", map[string]any{"Approved": false, "Approver": "bob", "Comment": "no"}},
	}
	for _, s := range rejectShapes {
		t.Run("reject_"+s.name, func(t *testing.T) {
			got, err := decodeApprovalDecision(s.payload)
			require.NoError(t, err)
			assert.False(t, got.Approved)
			assert.Equal(t, "bob", got.Approver)
		})
	}
}

// TestApproval_Adversarial_RoundTripKeyCasingSurvives — the "or worse" guard the
// author flagged: ApprovalDecision has NO json tags, so marshaling MUST emit the
// exported Go field names (Approved/Approver/Comment, capitalized) and the decoder
// looks those up verbatim. If the round-tripped key casing ever drifted, EVERY
// durably-delivered approve would silently default to Approved=false (a reject) —
// or a rename could resurrect a spoofable lowercase key. This asserts the exact
// marshal→UseNumber-unmarshal→decode chain the file stores use, end to end.
func TestApproval_Adversarial_RoundTripKeyCasingSurvives(t *testing.T) {
	for _, approved := range []bool{true, false} {
		in := ApprovalDecision{Approved: approved, Approver: "carol", Comment: "lgtm"}

		// Exactly the durable codec: marshalSignalPayload (json.Marshal) then
		// unmarshalSignalPayload (json.Decoder + UseNumber).
		s, err := marshalSignalPayload(in)
		require.NoError(t, err)
		back, err := unmarshalSignalPayload(s)
		require.NoError(t, err)

		m, ok := back.(map[string]any)
		require.Truef(t, ok, "round-trip yields a generic map, got %T", back)
		// The exact keys the decoder looks up — capitalized, verbatim.
		_, hasApproved := m["Approved"]
		require.Truef(t, hasApproved, "round-tripped map missing capitalized key %q; keys=%v", "Approved", mapKeys(m))
		require.IsType(t, true, m["Approved"], "Approved must survive as a bool (not stringified/numbered)")

		got, derr := decodeApprovalDecision(back)
		require.NoError(t, derr)
		assert.Equalf(t, approved, got.Approved, "round-tripped Approved=%v must decode faithfully", approved)
		assert.Equal(t, "carol", got.Approver)
		assert.Equal(t, "lgtm", got.Comment)
	}
}

func mapKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// TestApproval_Adversarial_EndToEnd_AllThreeStores — the REAL park→deliver→resume
// invariant driven through EVERY SignalStore (the author drove approve/reject only
// on InMemory and round-trip only on JSONFile; FlatBuffers approval end-to-end was
// untested). Deliver + drive are two SEPARATE steps so a durable store actually
// writes the payload to disk and reads it back before the decode. Approve resumes
// to terminal success with audit fields intact; reject fails fast with a
// classifiable *ApprovalRejectedError, downstream never runs, and a RE-DRIVE of the
// terminal-failed run is a no-op (terminal-no-op-on-redrive) — on the FB store too.
func TestApproval_Adversarial_EndToEnd_AllThreeStores(t *testing.T) {
	for storeName, newStore := range approvalSignalStores(t) {
		t.Run(storeName+"/approve", func(t *testing.T) {
			store := newStore()
			var afterN atomic.Int32
			w := buildApprovalWorkflow(t, store, "wf-appr", &afterN)

			require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)
			require.NoError(t, w.DeliverSignal(ApproveSignal("gate", "alice", "ship it", "d1")))
			require.NoError(t, w.Execute(context.Background()), "round-tripped approve resumes")

			final, err := store.Load("wf-appr")
			require.NoError(t, err)
			assertNodeStatus(t, final, "gate", Completed)
			assertNodeStatus(t, final, "after", Completed)
			require.EqualValues(t, 1, afterN.Load())

			// The approved decision is persisted for audit as the node output. NOTE:
			// the DECODABILITY of that persisted output is store-dependent and BROKEN
			// on FlatBuffers — see TestApproval_Defect_FBAuditReadbackUndecodable
			// (finding F-1). Here we assert only the store-agnostic invariant: the
			// approve authorized (gate Completed + downstream ran) and an output is
			// persisted. The decode-fidelity of the read-back is pinned separately.
			_, ok := final.GetOutput("gate")
			require.Truef(t, ok, "%s: the approved decision is persisted as node output", storeName)
		})

		t.Run(storeName+"/reject_failfast_and_terminal_redrive", func(t *testing.T) {
			store := newStore()
			var afterN atomic.Int32
			w := buildApprovalWorkflow(t, store, "wf-rej", &afterN)

			require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)
			require.NoError(t, w.DeliverSignal(RejectSignal("gate", "bob", "no budget", "d1")))

			err := w.Execute(context.Background())
			require.Error(t, err, "%s: a round-tripped reject must fail the run", storeName)
			var rejErr *ApprovalRejectedError
			require.Truef(t, errors.As(err, &rejErr), "%s: failure classifiable as *ApprovalRejectedError", storeName)
			assert.Equal(t, "gate", rejErr.Node)
			assert.Equal(t, "bob", rejErr.Approver)
			assert.Equal(t, "no budget", rejErr.Comment)

			failed, lerr := store.Load("wf-rej")
			require.NoError(t, lerr)
			assertNodeStatus(t, failed, "gate", Failed)
			require.EqualValues(t, 0, afterN.Load(), "%s: downstream never runs after reject", storeName)

			// Terminal-no-op-on-redrive: the run is durably Failed, so re-driving is a
			// no-op (nil), the gate stays Failed, downstream still never runs, and no
			// phantom completion appears.
			require.NoError(t, w.Execute(context.Background()), "%s: re-drive of terminal-failed run is a no-op", storeName)
			refinal, lerr := store.Load("wf-rej")
			require.NoError(t, lerr)
			assertNodeStatus(t, refinal, "gate", Failed)
			require.EqualValues(t, 0, afterN.Load(), "%s: downstream still never runs across reject re-drives", storeName)
		})
	}
}

// TestApproval_Adversarial_CorruptDeliveredPayloadNeverAuthorizes — the end-to-end
// phantom-approve guard: hand-craft CORRUPT / AMBIGUOUS decision payloads and
// deliver them as real signals through every store, then drive the gate. For NONE
// of them may the gate reach Completed or the downstream node run. A missing
// Approved (default-false → reject) and a wrong-typed Approved (ErrValidation) are
// both fail-safe; this proves neither becomes an authorization.
func TestApproval_Adversarial_CorruptDeliveredPayloadNeverAuthorizes(t *testing.T) {
	corrupt := []struct {
		name    string
		payload any
	}{
		{"approved_string_true", map[string]any{"Approved": "true", "Approver": "mallory"}},
		{"approved_number_1", map[string]any{"Approved": 1, "Approver": "mallory"}},
		{"missing_approved", map[string]any{"Approver": "mallory", "Comment": "sneaky"}},
		{"lowercase_key_spoof", map[string]any{"approved": true, "Approver": "mallory"}},
		{"empty_map", map[string]any{}},
		{"bare_string", "APPROVE"},
	}
	for storeName, newStore := range approvalSignalStores(t) {
		for _, tc := range corrupt {
			t.Run(storeName+"/"+tc.name, func(t *testing.T) {
				store := newStore()
				var afterN atomic.Int32
				w := buildApprovalWorkflow(t, store, "wf-corrupt", &afterN)

				require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)
				require.NoError(t, w.DeliverSignal(Signal{ID: "c1", Name: "gate", Payload: tc.payload}))

				// The drive may return an error (ErrValidation for wrong-typed, or an
				// *ApprovalRejectedError for a fail-safe default-false) — either is
				// acceptable. What is NOT acceptable is a silent authorization.
				_ = w.Execute(context.Background()) //nolint:errcheck // the property is "gate never Completed / downstream never runs", asserted below — not this drive's error

				final, err := store.Load("wf-corrupt")
				require.NoError(t, err)
				st, _ := final.GetNodeStatus("gate")
				assert.NotEqualf(t, Completed, st,
					"%s/%s: corrupt payload drove gate to Completed (phantom approve)", storeName, tc.name)
				require.EqualValuesf(t, 0, afterN.Load(),
					"%s/%s: corrupt payload authorized downstream (phantom approve)", storeName, tc.name)
			})
		}
	}
}

// TestApproval_AuditReadbackDecodesOnAllStores — regression guard for reproduced
// finding F-1 (MEDIUM, now FIXED): the persisted approval decision, when RELOADED
// from ANY of the three production stores, decodes faithfully for the audit
// read-back the contract promises — decodeApprovalDecision(GetOutput("gate")) and
// decodeApprovalDecision(Get(IdempotencyKey(...))).
//
// F-1 was: a FlatBuffersStore reload returns a persisted output/KV value as a raw
// JSON *string* (workflow_store.go:1017 loads outputs without re-decoding), and the
// decoder's original tolerance covered only the map[string]any shape (the
// InMemory/JSONFile shape) — so the FB audit read-back returned ErrValidation. The
// fix added a `case string` to decodeApprovalDecision that JSON-parses (UseNumber)
// and folds into the same field validation, so all three stores now decode. The
// no-phantom-approve invariant is preserved (a non-object / empty / bare-scalar
// string is ErrValidation, never a phantom approve) — see the fuzz below.
func TestApproval_AuditReadbackDecodesOnAllStores(t *testing.T) {
	drive := func(t *testing.T, store WorkflowStore) *WorkflowData {
		t.Helper()
		var afterN atomic.Int32
		w := buildApprovalWorkflow(t, store, "wf-f1", &afterN)
		require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)
		require.NoError(t, w.DeliverSignal(ApproveSignal("gate", "alice", "ship it", "d1")))
		require.NoError(t, w.Execute(context.Background()))
		final, err := store.Load("wf-f1")
		require.NoError(t, err)
		assertNodeStatus(t, final, "gate", Completed) // the approve authorized on every store
		return final
	}

	for _, s := range []struct {
		name  string
		store func() WorkflowStore
	}{
		{"InMemory", func() WorkflowStore { return NewInMemoryStore() }},
		{"JSONFile", func() WorkflowStore { st, e := NewJSONFileStore(t.TempDir()); require.NoError(t, e); return st }},
		{"FlatBuffers", func() WorkflowStore { st, e := NewFlatBuffersStore(t.TempDir()); require.NoError(t, e); return st }},
	} {
		t.Run(s.name+"/audit_readback_decodes", func(t *testing.T) {
			final := drive(t, s.store())
			out, ok := final.GetOutput("gate")
			require.True(t, ok)
			dec, derr := decodeApprovalDecision(out)
			require.NoError(t, derr, "%s: node output must decode for audit (F-1 regression)", s.name)
			assert.True(t, dec.Approved)
			assert.Equal(t, "alice", dec.Approver)
			assert.Equal(t, "ship it", dec.Comment)

			jv, jok := final.Get(IdempotencyKey(final, "gate"))
			require.True(t, jok)
			jdec, jderr := decodeApprovalDecision(jv)
			require.NoError(t, jderr, "%s: journal value must decode for audit (F-1 regression)", s.name)
			assert.True(t, jdec.Approved)
		})
	}
}

// FuzzDecodeApprovalDecision — coverage-guided fuzz of the defensive decoder over
// the ONLY untyped surface that reaches it in production: a durable payload string
// decoded via the store codec (unmarshalSignalPayload + UseNumber). The typed and
// *ApprovalDecision arms have no fuzzable surface and are pinned by the table tests
// above.
//
// Properties (any of which failing is a defect):
//   - decodeApprovalDecision NEVER panics on any decoded payload;
//   - it returns (Approved=true, err=nil) ONLY when the decoded payload is a
//     map[string]any whose "Approved" is a genuine bool true — never for a reject,
//     a number, a string, a nil, or any other shape (the CRITICAL no-phantom bar).
func FuzzDecodeApprovalDecision(f *testing.F) {
	seeds := []string{
		`{"Approved":true,"Approver":"a","Comment":"c"}`,
		`{"Approved":false,"Approver":"b"}`,
		`{"Approved":"true"}`,
		`{"Approved":1}`,
		`{"Approved":1.0}`,
		`{"Approved":null}`,
		`{"Approver":"x"}`,
		`{"approved":true}`,
		`{"Approved":{"Approved":true}}`,
		`{"Approved":[true]}`,
		`{}`,
		`"APPROVE"`,
		`true`,
		`1`,
		`[true]`,
		`null`,
		``,
		`{"Approved":true,"Approved":false}`,
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, payloadStr string) {
		payload, err := unmarshalSignalPayload(payloadStr)
		if err != nil {
			return // corrupt JSON is the store codec's concern, not the decoder's
		}
		got, derr := decodeApprovalDecision(payload) // must not panic on any input
		if derr == nil && got.Approved {
			m, ok := payload.(map[string]any)
			if !ok {
				t.Fatalf("PHANTOM APPROVE: non-map payload %#v (%T) decoded to Approved=true", payload, payload)
			}
			av, ok := m["Approved"].(bool)
			if !ok || !av {
				t.Fatalf("PHANTOM APPROVE: payload %#v decoded to Approved=true but m[\"Approved\"]=%#v", payload, m["Approved"])
			}
		}
	})
}
