package workflow

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/quick"

	"github.com/stretchr/testify/require"
)

// AUD-025 ADVERSARIAL SUITE — independently attacks the approval correlation-nonce
// control (commit 3e2c42a). The author's own tests cover InMemory + JSON happy/inert
// paths and the derivation binding. This suite attacks the partitions those tests do
// NOT reach:
//   - cross-store fidelity through FlatBuffers AND SQLite (author covered only
//     InMemory + JSON), where the payload becomes a JSON string / map and the Nonce
//     could be dropped (→ forever-park) or mangled;
//   - engine/host nonce NON-drift after a real durable round-trip (GetWorkflowID +
//     defDigestKey must survive persistence, or the engine expects a nonce no host
//     can compute → permanent inadvertent park);
//   - malformed / missing / wrong-typed Nonce field → must never phantom-approve;
//   - idempotent re-apply + wrong+correct co-buffered dedupe interaction;
//   - the derivation as a PROPERTY (injectivity / length-prefix boundary) via a
//     generated search, not three hand-picked examples.
//
// Oracles used are stated per test: a property (injectivity/determinism), a
// metamorphic relation (engine-expected == host-recomputed), or the minimum bar
// (a stray/stale/malformed decision must never approve and must not silently
// corrupt the park).

// buildApprovalWorkflowFB / mkSQLiteStore give the durable stores the author's
// helper never exercised for approval.

func mkAUD025SQLite(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "aud025.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // cleanup
	return s
}

func mkAUD025FB(t *testing.T) *FlatBuffersStore {
	t.Helper()
	s, err := NewFlatBuffersStore(t.TempDir())
	require.NoError(t, err)
	return s
}

// ---------------------------------------------------------------------------
// #4 CROSS-STORE FIDELITY — FlatBuffers. The delivered decision's Nonce must
// survive the FB mailbox round-trip (payload → JSON string → map), or a
// correctly-correlated decision goes inert on a durable store (forever-park).
// Oracle: minimum bar (correct nonce approves; wrong nonce stays parked).
// ---------------------------------------------------------------------------

func TestAUD025_Adv_FBStore_CorrectNonceApproves(t *testing.T) {
	store := mkAUD025FB(t)
	var afterN atomic.Int32
	w := buildApprovalWorkflow(t, store, "wf-fb-ok", &afterN)

	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)

	nonce := w.ApprovalNonce("gate")
	require.NotEmpty(t, nonce)
	require.NoError(t, w.DeliverAndResume(context.Background(),
		ApproveSignal("gate", "alice", "ship it", "d1", nonce)),
		"a correct-nonce approve must survive the FlatBuffers mailbox round-trip")

	final, err := store.Load("wf-fb-ok")
	require.NoError(t, err)
	assertNodeStatus(t, final, "gate", Completed)
	assertNodeStatus(t, final, "after", Completed)
	require.EqualValues(t, 1, afterN.Load())
}

func TestAUD025_Adv_FBStore_WrongNonceInert(t *testing.T) {
	store := mkAUD025FB(t)
	var afterN atomic.Int32
	w := buildApprovalWorkflow(t, store, "wf-fb-wrong", &afterN)
	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)

	require.ErrorIs(t,
		w.DeliverAndResume(context.Background(), ApproveSignal("gate", "mallory", "forged", "bad", "not-the-nonce")),
		ErrSuspended, "a wrong-nonce approve must stay parked through the FB store")
	parked, err := store.Load("wf-fb-wrong")
	require.NoError(t, err)
	assertNodeStatus(t, parked, "gate", Waiting)
	require.EqualValues(t, 0, afterN.Load())

	// And the correctly-correlated decision still resumes (the stale one stays buffered).
	require.NoError(t, w.DeliverAndResume(context.Background(),
		ApproveSignal("gate", "alice", "ok", "good", w.ApprovalNonce("gate"))))
	final, err := store.Load("wf-fb-wrong")
	require.NoError(t, err)
	assertNodeStatus(t, final, "gate", Completed)
	require.EqualValues(t, 1, afterN.Load())
}

// ---------------------------------------------------------------------------
// #4 CROSS-STORE FIDELITY — SQLite. Same, over the multi-process store where the
// payload is persisted as a TEXT JSON column and re-parsed on TakeSignals.
// ---------------------------------------------------------------------------

func TestAUD025_Adv_SQLite_CorrectNonceApproves(t *testing.T) {
	store := mkAUD025SQLite(t)
	var afterN atomic.Int32
	w := buildApprovalWorkflow(t, store, "wf-sql-ok", &afterN)

	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)

	require.NoError(t, w.DeliverAndResume(context.Background(),
		ApproveSignal("gate", "alice", "ship it", "d1", w.ApprovalNonce("gate"))),
		"a correct-nonce approve must survive the SQLite mailbox round-trip")

	final, err := store.Load("wf-sql-ok")
	require.NoError(t, err)
	assertNodeStatus(t, final, "gate", Completed)
	assertNodeStatus(t, final, "after", Completed)
	require.EqualValues(t, 1, afterN.Load())
}

func TestAUD025_Adv_SQLite_WrongNonceInert_ThenReject(t *testing.T) {
	store := mkAUD025SQLite(t)
	var afterN atomic.Int32
	w := buildApprovalWorkflow(t, store, "wf-sql-rej", &afterN)
	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)

	// A wrong-nonce reject must NOT fail the node through the durable store.
	require.ErrorIs(t,
		w.DeliverAndResume(context.Background(), RejectSignal("gate", "mallory", "stale", "bad", "wrong")),
		ErrSuspended, "a wrong-nonce reject must not fail the run (SQLite)")
	parked, err := store.Load("wf-sql-rej")
	require.NoError(t, err)
	assertNodeStatus(t, parked, "gate", Waiting)

	// A correct-nonce reject fails fast with the typed error.
	err = w.DeliverAndResume(context.Background(), RejectSignal("gate", "bob", "no", "good", w.ApprovalNonce("gate")))
	var rej *ApprovalRejectedError
	require.True(t, errors.As(err, &rej), "a correct-nonce reject fails with *ApprovalRejectedError (SQLite)")
	require.EqualValues(t, 0, afterN.Load())
}

// ---------------------------------------------------------------------------
// #5 ENGINE / HOST NON-DRIFT after a real durable round-trip. The engine computes
// expectedApprovalNonce(loadedData, node) from GetWorkflowID() + the stamped
// defDigestKey; the host computes w.ApprovalNonce(node). If a durable Load drops
// the workflow ID or the stamped digest, the engine would expect a nonce no host
// can compute → a permanent inadvertent park even for a legitimate decision.
// Oracle: metamorphic relation — the two derivations must be identical on the
// state a durable store hands back.
// ---------------------------------------------------------------------------

func TestAUD025_Adv_NoEngineHostDrift_AfterDurableRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store func(t *testing.T) WorkflowStore
	}{
		{"InMemory", func(t *testing.T) WorkflowStore { return NewInMemoryStore() }},
		{"JSON", func(t *testing.T) WorkflowStore {
			s, err := NewJSONFileStore(t.TempDir())
			require.NoError(t, err)
			return s
		}},
		{"FlatBuffers", func(t *testing.T) WorkflowStore { return mkAUD025FB(t) }},
		{"SQLite", func(t *testing.T) WorkflowStore { return mkAUD025SQLite(t) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := tc.store(t)
			w := buildApprovalWorkflow(t, store, "wf-drift", nil)
			require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)

			loaded, err := store.Load("wf-drift")
			require.NoError(t, err)

			// The workflow ID must survive the durable round-trip, or the engine's
			// expected nonce is derived over an empty/wrong id.
			require.Equal(t, "wf-drift", loaded.GetWorkflowID(),
				"durable Load must restore the workflow id the engine derives the nonce from")

			// The definition digest the executor stamped must survive too.
			dv, ok := loaded.Get(defDigestKey)
			require.True(t, ok, "durable Load must restore the stamped definition digest")
			require.Equal(t, w.dag.DefinitionDigest(), dv,
				"the persisted digest must equal the live graph digest (no drift)")

			// The metamorphic relation: engine-expected == host-recomputed.
			require.Equal(t, w.ApprovalNonce("gate"), expectedApprovalNonce(loaded, "gate"),
				"engine-expected nonce must equal the host-recomputable nonce after a durable resume")
		})
	}
}

// ---------------------------------------------------------------------------
// #7 MALFORMED / MISSING / WRONG-TYPED Nonce field. A stray/persisted decision
// whose Nonce is a non-string JSON type, or a map missing Nonce, must be
// ErrValidation OR fail-safe-inert — NEVER a phantom approve.
// Oracle: minimum bar (Approved=true is never authorized without the exact nonce).
// ---------------------------------------------------------------------------

func TestAUD025_Adv_MalformedNonceField_NeverPhantomApprove(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload map[string]any
	}{
		{"nonce is a float", map[string]any{"Approved": true, "Nonce": float64(42)}},
		{"nonce is a bool", map[string]any{"Approved": true, "Nonce": true}},
		{"nonce is a nested object", map[string]any{"Approved": true, "Nonce": map[string]any{"x": 1}}},
		{"nonce missing entirely", map[string]any{"Approved": true, "Approver": "x"}},
		{"nonce is null", map[string]any{"Approved": true, "Nonce": nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := decodeApprovalDecision(tc.payload)
			// Either a typed ErrValidation, or a clean decode whose Nonce is not a
			// usable correlation token — in neither case may it phantom-approve.
			if err != nil {
				require.ErrorIs(t, err, ErrValidation, "a malformed nonce field must be a typed ErrValidation")
				return
			}
			// If it decoded (missing/null nonce → empty string), the empty nonce can
			// never equal a real 64-hex expected nonce, so it is inert. Assert the
			// engine treats it as such: it must not match any real park.
			real := ApprovalNonce("wf", "gate", "somedigest")
			require.NotEqual(t, real, d.Nonce,
				"a decoded malformed/missing nonce must never equal a real park nonce")
		})
	}
}

// TestAUD025_Adv_MissingNonceThroughEngineIsInert drives the missing-Nonce map
// through the REAL engine (deliver a raw map payload, not the typed struct) to
// confirm the node stays parked — a stray Approved=true with no nonce cannot
// approve. Oracle: minimum bar.
func TestAUD025_Adv_MissingNonceThroughEngineIsInert(t *testing.T) {
	store := NewInMemoryStore()
	var afterN atomic.Int32
	w := buildApprovalWorkflow(t, store, "wf-nononce", &afterN)
	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)

	// A raw map payload (the round-tripped shape) carrying Approved=true but NO Nonce.
	stray := Signal{ID: "stray", Name: "gate", Payload: map[string]any{"Approved": true, "Approver": "mallory"}}
	require.ErrorIs(t, w.DeliverAndResume(context.Background(), stray), ErrSuspended,
		"a no-nonce Approved=true payload must not approve the park")
	parked, err := store.Load("wf-nononce")
	require.NoError(t, err)
	assertNodeStatus(t, parked, "gate", Waiting)
	require.EqualValues(t, 0, afterN.Load())
}

// ---------------------------------------------------------------------------
// #6 IDEMPOTENT re-apply + WRONG+CORRECT co-buffered dedupe. A correct-nonce
// decision re-delivered (same sig.ID) and re-driven stays idempotent; a wrong and
// a correct decision both buffered → the correct one is still found and consumed,
// the wrong one stays inert and never blocks. Oracle: minimum bar + idempotence.
// ---------------------------------------------------------------------------

func TestAUD025_Adv_WrongAndCorrectCoBuffered(t *testing.T) {
	// Deliver the WRONG-nonce decision FIRST with an ID that sorts BEFORE the correct
	// one (TakeSignals iterates sorted by ID) — so the loop hits the inert one first
	// and must keep scanning to the correct one rather than parking.
	store := NewInMemoryStore()
	var afterN atomic.Int32
	w := buildApprovalWorkflow(t, store, "wf-cobuf", &afterN)
	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)

	// "aaa" sorts before "zzz"; the inert wrong-nonce one is scanned first.
	require.NoError(t, w.DeliverSignal(ApproveSignal("gate", "mallory", "forged", "aaa", "wrong-nonce")))
	require.NoError(t, w.DeliverSignal(ApproveSignal("gate", "alice", "ok", "zzz", w.ApprovalNonce("gate"))))

	require.NoError(t, w.DeliverAndResume(context.Background(),
		ApproveSignal("gate", "alice", "ok", "zzz", w.ApprovalNonce("gate"))),
		"the correct decision must be found past the co-buffered inert one")
	final, err := store.Load("wf-cobuf")
	require.NoError(t, err)
	assertNodeStatus(t, final, "gate", Completed)
	require.EqualValues(t, 1, afterN.Load())
}

func TestAUD025_Adv_CorrectNonceReDeliverIsIdempotent(t *testing.T) {
	store := NewInMemoryStore()
	var afterN atomic.Int32
	w := buildApprovalWorkflow(t, store, "wf-idem", &afterN)
	require.ErrorIs(t, w.Execute(context.Background()), ErrSuspended)

	nonce := w.ApprovalNonce("gate")
	require.NoError(t, w.DeliverAndResume(context.Background(),
		ApproveSignal("gate", "alice", "ok", "same-id", nonce)))
	assertN := afterN.Load()
	require.EqualValues(t, 1, assertN)

	// Re-deliver the SAME sig.ID and re-drive; the terminal run is a no-op, downstream
	// does not double-run.
	require.NoError(t, w.DeliverAndResume(context.Background(),
		ApproveSignal("gate", "alice", "ok", "same-id", nonce)))
	require.EqualValues(t, 1, afterN.Load(), "a re-delivered correct-nonce decision must not double-apply")
}

// ---------------------------------------------------------------------------
// #3 BINDING as a PROPERTY (not three examples). Injectivity: distinct
// (workflowID, node, digest) triples must not collide; and the length-prefix
// boundary — no (wf, node) split can collide — asserted over a generated search.
// Oracle: property (injectivity of the derivation).
// ---------------------------------------------------------------------------

func TestAUD025_Adv_NonceInjectivity_Property(t *testing.T) {
	// No two DISTINCT (wf, node, digest) triples produce the same nonce, over
	// randomized inputs including empty strings, NUL bytes, and unicode.
	seen := map[string]string{} // nonce -> "wf\x00node\x00digest"
	f := func(wf, node, digest string) bool {
		key := wf + "\x00" + node + "\x00" + digest
		n := ApprovalNonce(wf, node, digest)
		if len(n) != 64 { // SHA-256 hex is always 64 chars — totality of the output shape.
			return false
		}
		if prev, ok := seen[n]; ok && prev != key {
			t.Errorf("collision: %q and %q share nonce %s", prev, key, n)
			return false
		}
		seen[n] = key
		return true
	}
	require.NoError(t, quick.Check(f, &quick.Config{MaxCount: 5000}))
}

func TestAUD025_Adv_LengthPrefixBoundary_Property(t *testing.T) {
	// For every split of a fixed concatenation into (wf, node), the length-prefix
	// must keep the pre-images distinct: ApprovalNonce(s[:i], s[i:], d) are pairwise
	// distinct across i. A missing/broken length prefix would collide two adjacent
	// splits. Oracle: property (no length-extension collision).
	const s = "abcde"
	const d = "digest"
	seen := map[string]int{}
	for i := 0; i <= len(s); i++ {
		n := ApprovalNonce(s[:i], s[i:], d)
		if prev, ok := seen[n]; ok {
			t.Fatalf("length-prefix collision: split at %d and %d both yield %s", prev, i, n)
		}
		seen[n] = i
	}
}

// TestAUD025_Adv_EmptyInputsAreTotal — boundary: empty workflowID / node / digest
// must each still yield a well-formed 64-hex nonce (never panic, never empty) and
// remain distinct from the all-populated case. Oracle: totality + binding.
func TestAUD025_Adv_EmptyInputsAreTotal(t *testing.T) {
	cases := []struct{ wf, node, digest string }{
		{"", "", ""},
		{"", "gate", "d"},
		{"wf", "", "d"},
		{"wf", "gate", ""},
	}
	seen := map[string]bool{}
	for _, c := range cases {
		n := ApprovalNonce(c.wf, c.node, c.digest)
		require.Len(t, n, 64, "empty-input nonce must still be a 64-hex digest")
		require.False(t, seen[n], "empty-input variants must not collide: %+v", c)
		seen[n] = true
	}
}

// ---------------------------------------------------------------------------
// FUZZ target — no delivered decision payload of any shape can make the engine
// approve without the exact expected nonce, and no shape may panic/hang the
// decode. Seeded from the boundaries above. Run: go test -run=x -fuzz=FuzzAUD025
// ---------------------------------------------------------------------------

func FuzzAUD025_NonceGateNeverPhantomApproves(f *testing.F) {
	f.Add("wf", "gate", "digest", "somenonce")
	f.Add("", "", "", "")
	f.Add("wf", "gate", "digest", "")
	f.Fuzz(func(t *testing.T, wf, node, digest, deliveredNonce string) {
		expected := ApprovalNonce(wf, node, digest)
		// Totality: derivation never panics and is always a 64-hex string.
		if len(expected) != 64 {
			t.Fatalf("nonce not 64-hex for (%q,%q,%q)", wf, node, digest)
		}
		// A decoded decision authorizes ONLY when its nonce is byte-equal to expected.
		payload := map[string]any{"Approved": true, "Nonce": deliveredNonce}
		d, err := decodeApprovalDecision(payload)
		if err != nil {
			return // typed error is a fine (inert) outcome
		}
		authorized := d.Approved && d.Nonce == expected
		wantAuthorized := deliveredNonce == expected
		if authorized != wantAuthorized {
			t.Fatalf("nonce gate mismatch: delivered=%q expected=%q authorized=%v", deliveredNonce, expected, authorized)
		}
	})
}

// sanity: the fuzz body is exercised once even without -fuzz, so it stays live coverage.
func TestAUD025_Adv_FuzzSeedSanity(t *testing.T) {
	require.Equal(t, fmt.Sprintf("%d", 64), fmt.Sprintf("%d", len(ApprovalNonce("wf", "gate", "d"))))
}

// ---------------------------------------------------------------------------
// ROOT-CAUSE PROBE for the SQLite drift found above. The stamped defDigestKey is
// the SAME reserved key the AUD-010 changed-graph resume guard reads. If SQLite
// drops it on the checkpoint round-trip, TWO things break on SQLite that hold on
// InMemory: (a) the AUD-010 digest guard silently does not fire on a changed-graph
// resume, and (b) a host that recomputes the AUD-025 nonce from a RAW store.Load
// (the documented sub-workflow pattern) derives an empty-digest nonce the engine
// will never accept → a permanent inadvertent forever-park.
// ---------------------------------------------------------------------------

// TestAUD025_Adv_ReservedDigestKeySurvivesSave is the minimal reproduction: a
// reserved __def_digest__ written into WorkflowData must survive Save→Load on
// EVERY store. Oracle: cross-store fidelity (the store's own contract, sqlite.go:11).
func TestAUD025_Adv_ReservedDigestKeySurvivesSave(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store func(t *testing.T) WorkflowStore
	}{
		{"InMemory", func(t *testing.T) WorkflowStore { return NewInMemoryStore() }},
		{"JSON", func(t *testing.T) WorkflowStore {
			s, err := NewJSONFileStore(t.TempDir())
			require.NoError(t, err)
			return s
		}},
		{"FlatBuffers", func(t *testing.T) WorkflowStore { return mkAUD025FB(t) }},
		{"SQLite", func(t *testing.T) WorkflowStore { return mkAUD025SQLite(t) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := tc.store(t)
			data := NewWorkflowData("wf-reserved")
			data.setReserved(defDigestKey, "DEADBEEFDIGEST")
			data.Set("ordinary", "v")
			require.NoError(t, store.Save(data))

			loaded, err := store.Load("wf-reserved")
			require.NoError(t, err)
			// The ordinary key always survives — a control proving Save/Load ran.
			ov, ook := loaded.Get("ordinary")
			require.True(t, ook)
			require.Equal(t, "v", ov)

			dv, ok := loaded.Get(defDigestKey)
			require.True(t, ok, "the reserved __def_digest__ key must survive the durable round-trip")
			require.Equal(t, "DEADBEEFDIGEST", dv)
		})
	}
}

// TestAUD025_Adv_AUD010GuardFiresAcrossStores drives the AUD-010 changed-graph
// resume guard end-to-end (the author's own end-to-end test uses ONLY InMemory)
// across every store. A store that drops the stamped digest silently accepts a
// changed-graph resume — a mis-resume onto state that no longer matches the graph.
// Oracle: the AUD-010 contract (a changed graph must be refused with ErrValidation).
func TestAUD025_Adv_AUD010GuardFiresAcrossStores(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store func(t *testing.T) WorkflowStore
	}{
		{"InMemory", func(t *testing.T) WorkflowStore { return NewInMemoryStore() }},
		{"JSON", func(t *testing.T) WorkflowStore {
			s, err := NewJSONFileStore(t.TempDir())
			require.NoError(t, err)
			return s
		}},
		{"FlatBuffers", func(t *testing.T) WorkflowStore { return mkAUD025FB(t) }},
		{"SQLite", func(t *testing.T) WorkflowStore { return mkAUD025SQLite(t) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := tc.store(t)
			build := func(withExtra bool) *Workflow {
				b := NewWorkflowBuilder().WithWorkflowID("wf-guard")
				b.AddStartNode("a").WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return nil }))
				b.AddNode("b").DependsOn("a").WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return nil }))
				if withExtra {
					b.AddNode("c").DependsOn("b").WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return nil }))
				}
				w, err := FromBuilder(b)
				require.NoError(t, err)
				w.Store = store
				return w
			}

			require.NoError(t, build(false).Execute(context.Background()), "first run persists graph G1")
			require.NoError(t, build(false).Execute(context.Background()), "same graph resumes fine")

			// Changed graph (adds c) — the node-name subset check passes ({a,b} still
			// exist), so ONLY the persisted digest can catch it.
			err := build(true).Execute(context.Background())
			require.Error(t, err, "a changed-graph resume must be refused")
			require.ErrorIs(t, err, ErrValidation, "the AUD-010 digest guard must fire on every store")
		})
	}
}

// TestAUD025_Adv_AUD010GuardOnParkedResume — the SHARP reproduction. The scenario
// AUD-025 exists for is a LONG-LIVED approval PARK (a human-in-the-loop wait), and
// that is precisely when a graph definition is most likely to change under it. A
// parked run is persisted via the store's MID-RUN checkpoint path, not the
// completion Save. On SQLite (the only IncrementalCheckpointer) that path is
// SaveDeltaCheckpoint, and the executor stamps __def_digest__ (workflow.go:514)
// BEFORE it arms delta capture (workflow.go:626) — so the digest stamp is not in
// the captured delta and is never persisted for a parked run. On resume,
// data.Get(defDigestKey) is absent, the AUD-010 guard (workflow.go:496) is skipped,
// and a CHANGED-graph resume of a parked workflow silently re-parks instead of
// being refused. InMemory/JSON/FB use the full-snapshot checkpoint and are safe.
//
// Oracle: the AUD-010 contract — a changed-graph resume must be ErrValidation on
// EVERY store, parked or completed.
func TestAUD025_Adv_AUD010GuardOnParkedResume(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store func(t *testing.T) WorkflowStore
	}{
		{"InMemory", func(t *testing.T) WorkflowStore { return NewInMemoryStore() }},
		{"JSON", func(t *testing.T) WorkflowStore {
			s, err := NewJSONFileStore(t.TempDir())
			require.NoError(t, err)
			return s
		}},
		{"FlatBuffers", func(t *testing.T) WorkflowStore { return mkAUD025FB(t) }},
		{"SQLite", func(t *testing.T) WorkflowStore { return mkAUD025SQLite(t) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := tc.store(t)
			build := func(withExtra bool) *Workflow {
				b := NewWorkflowBuilder().WithWorkflowID("wf-parked-guard")
				b.AddApproval("gate")
				b.AddNode("after").DependsOn("gate").
					WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return nil }))
				if withExtra {
					b.AddNode("extra").DependsOn("after").
						WithAction(ActionFunc(func(context.Context, *WorkflowData) error { return nil }))
				}
				w, err := FromBuilder(b)
				require.NoError(t, err)
				w.Store = store
				return w
			}

			// First run PARKS on the undelivered approval; the parked state is checkpointed.
			require.ErrorIs(t, build(false).Execute(context.Background()), ErrSuspended)

			// Resume onto a CHANGED graph (adds "extra"; {gate,after} still exist so the
			// node-name subset check passes — only the persisted digest can catch it).
			err := build(true).Execute(context.Background())
			require.ErrorIs(t, err, ErrValidation,
				"a changed-graph resume of a PARKED workflow must be refused on every store — "+
					"the AUD-010 digest guard must not be silently defeated by the checkpoint path")
		})
	}
}
