package workflow

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Adversarial suite for the ph93 SQLite SignalStore (M19). The author's fixtures (workflow_store_
// sqlite_signals_test.go) exercise the HAPPY deliver→take→ack path. This suite attacks the two
// security-relevant surfaces the phase introduces:
//
//  1. The signals.payload DESERIALIZATION surface — an EXTERNAL-WRITABLE persisted column (a corrupt
//     DB or a direct SQL write) decoded by unmarshalSignalPayload on the read path (M9 input-TCB
//     threat model). We INSERT crafted payloads directly via s.db, bypassing DeliverSignal's marshal,
//     then TakeSignals must never panic and never drive an unbounded alloc.
//  2. The F37 mailbox-entry cap on that external-writable channel, under raw over-fill.
//
// It also pins durability/atomicity, last-writer-wins under concurrency, deterministic ordering, and
// the reuse of the validateSignalID / validateWorkflowID guards on the SQLite path.

// mustSQLiteAdv opens a fresh SQLite store, registered for close.
func mustSQLiteAdv(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "adv.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // test cleanup; a close error cannot affect the assertions
	return s
}

// rawInsertSignal writes a row DIRECTLY to the signals table, bypassing DeliverSignal's
// marshalSignalPayload / validation. This simulates a corrupt DB or a hostile direct-SQL writer —
// the external-writable persisted surface the decode path must defend against. When nullPayload is
// true the payload column is written as SQL NULL (the DDL's documented "NULL = nil payload").
func rawInsertSignal(t *testing.T, s *SQLiteStore, wf, id, name, payload string, nullPayload bool) {
	t.Helper()
	var err error
	if nullPayload {
		_, err = s.db.Exec(
			`INSERT INTO signals(workflow_id, sig_id, name, payload, enqueued_at) VALUES(?,?,?,NULL,?)`,
			wf, id, name, 1)
	} else {
		_, err = s.db.Exec(
			`INSERT INTO signals(workflow_id, sig_id, name, payload, enqueued_at) VALUES(?,?,?,?,?)`,
			wf, id, name, payload, 1)
	}
	require.NoError(t, err, "raw signal insert must succeed (simulating an external writer)")
}

// TestSQLiteSignalsAdv_DecodeRawPayloadFuzz drives the payload DESERIALIZATION surface with hostile
// raw rows. The contract (workflow_store_sqlite_signals.go, TakeSignals): a bad payload yields
// ErrCorruptData, a nil-representation yields a nil payload, and NOTHING panics or allocates
// unboundedly. Each row is inserted raw (a corrupt-DB / direct-SQL-write attacker), then read back.
func TestSQLiteSignalsAdv_DecodeRawPayloadFuzz(t *testing.T) {
	cases := []struct {
		name        string
		payload     string
		nullPayload bool
		wantErr     error // nil = expect success
		wantNil     bool  // when success, expect a nil payload
		checkVal    func(t *testing.T, v any)
	}{
		{
			name:    "invalid_json_object",
			payload: "{not valid json",
			wantErr: ErrCorruptData,
		},
		{
			name:    "invalid_json_trailing_garbage_bytes",
			payload: "\x00\x01\x02\xff",
			wantErr: ErrCorruptData,
		},
		{
			// A pathologically deep array. Go's encoding/json enforces a 10000-nesting-depth limit,
			// so this returns "exceeded max depth" (ErrCorruptData) rather than blowing the stack.
			// Confirms the decoder self-bounds recursion on the persisted surface — no crash.
			name:    "deeply_nested_array_200k",
			payload: strings.Repeat("[", 200000),
			wantErr: ErrCorruptData,
		},
		{
			// A 100k-digit integer literal. With UseNumber it lands as a json.Number (the raw literal
			// string), so there is NO parse into a bignum and NO alloc beyond the payload itself — the
			// decode is bounded by the input size, not by the number's magnitude.
			name:    "huge_number_literal_100k_digits",
			payload: "1" + strings.Repeat("0", 100000),
			checkVal: func(t *testing.T, v any) {
				t.Helper()
				_, ok := v.(interface{ String() string }) // json.Number satisfies Stringer
				assert.True(t, ok, "huge number must decode to json.Number, got %T", v)
			},
		},
		{
			// A wide, deeply-keyed but shallow object — a large-but-legal payload must decode fine
			// (bounded alloc), proving the guard rejects DEPTH/corruption, not mere size.
			name:     "large_flat_object",
			payload:  "{" + strings.TrimSuffix(strings.Repeat(`"k":1,`, 5000), ",") + "}",
			checkVal: func(t *testing.T, v any) { t.Helper(); assert.IsType(t, map[string]any{}, v) },
		},
		{
			name:    "empty_string_is_nil_payload",
			payload: "",
			wantNil: true,
		},
		{
			name:    "json_null_literal_is_nil_payload",
			payload: "null",
			wantNil: true,
		},
		{
			// THE reproduced defect's regression: a SQL NULL payload — the DDL's documented nil-
			// payload representation ("NULL = nil payload"). Before the fix, TakeSignals scanned NULL
			// into a plain string and failed the WHOLE read with ErrIO ("converting NULL to string is
			// unsupported"), a poison-pill. It must now yield a nil payload.
			name:        "sql_null_payload_is_nil_payload",
			nullPayload: true,
			wantNil:     true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := mustSQLiteAdv(t)
			const wf = "wf-fuzz"
			rawInsertSignal(t, s, wf, "sig", "n", tc.payload, tc.nullPayload)

			// The load-bearing safety assertion: no panic on ANY hostile payload.
			var got []Signal
			var err error
			require.NotPanics(t, func() { got, err = s.TakeSignals(wf) },
				"decode of a hostile persisted payload must never panic")

			if tc.wantErr != nil {
				require.Error(t, err)
				require.True(t, errors.Is(err, tc.wantErr),
					"want %v, got %v", tc.wantErr, err)
				return
			}
			require.NoError(t, err)
			require.Len(t, got, 1)
			if tc.wantNil {
				assert.Nil(t, got[0].Payload, "must decode to a nil payload")
			}
			if tc.checkVal != nil {
				tc.checkVal(t, got[0].Payload)
			}
		})
	}
}

// TestSQLiteSignalsAdv_SQLNullPoisonPill is the headline regression for the reproduced defect
// (F-P93-ADV-1). A single externally-written SQL NULL payload must NOT brick the entire workflow
// mailbox: the valid rows around it must still read, and the NULL row must decode to a nil payload.
// Before the fix, one NULL row failed TakeSignals for the whole workflow.
func TestSQLiteSignalsAdv_SQLNullPoisonPill(t *testing.T) {
	s := mustSQLiteAdv(t)
	const wf = "wf-poison"

	rawInsertSignal(t, s, wf, "a-valid", "n", `"ok"`, false)
	rawInsertSignal(t, s, wf, "b-null", "n", "", true) // the poison row: SQL NULL payload
	rawInsertSignal(t, s, wf, "c-valid", "n", `42`, false)

	got, err := s.TakeSignals(wf)
	require.NoError(t, err, "one NULL row must not brick the whole mailbox (poison-pill defended)")
	require.Len(t, got, 3, "all rows readable despite the NULL payload")

	byID := map[string]Signal{}
	for _, g := range got {
		byID[g.ID] = g
	}
	assert.Equal(t, "ok", byID["a-valid"].Payload)
	assert.Nil(t, byID["b-null"].Payload, "SQL NULL decodes to nil payload (DDL contract)")
	iv, cerr := payloadAsInt64(byID["c-valid"].Payload)
	require.NoError(t, cerr)
	assert.Equal(t, int64(42), iv)
}

// TestSQLiteSignalsAdv_F37CapRawOverfill attacks the F37 entry-count bound with RAW over-fill (the
// author's cap test over-DELIVERS; this bypasses deliver and writes rows straight to the table, the
// corrupt-DB threat). AT the cap reads clean; cap+1 is rejected with ErrCorruptData and a nil slice
// (the guard fires BEFORE materializing rows).
func TestSQLiteSignalsAdv_F37CapRawOverfill(t *testing.T) {
	const cap = 4
	orig := signalMailboxCap
	signalMailboxCap = cap
	defer func() { signalMailboxCap = orig }()

	fill := func(t *testing.T, wf string, n int) *SQLiteStore {
		t.Helper()
		s := mustSQLiteAdv(t)
		for i := range n {
			rawInsertSignal(t, s, wf, fmt.Sprintf("s%03d", i), "n", `1`, false)
		}
		return s
	}

	t.Run("exactly_at_cap_reads_clean", func(t *testing.T) {
		s := fill(t, "wf-at", cap)
		got, err := s.TakeSignals("wf-at")
		require.NoError(t, err)
		require.Len(t, got, cap)
	})

	t.Run("cap_plus_one_rejected", func(t *testing.T) {
		s := fill(t, "wf-over", cap+1)
		got, err := s.TakeSignals("wf-over")
		require.Error(t, err)
		require.True(t, errors.Is(err, ErrCorruptData),
			"over-cap must reject as ErrCorruptData, got %v", err)
		require.Nil(t, got, "the guard must fire before materializing any rows")
	})
}

// TestSQLiteSignalsAdv_ConcurrentDeliverNoTear hammers DeliverSignal from many goroutines (distinct
// ids, each with a payload keyed to its id) while TakeSignals runs concurrently. Under -race this
// pins that the s.mu serialization yields NO torn signals (every returned Signal's name+payload
// match ITS id's delivery) and NO over-count. All deliveries land, exactly once each.
func TestSQLiteSignalsAdv_ConcurrentDeliverNoTear(t *testing.T) {
	s := mustSQLiteAdv(t)
	const wf = "wf-conc"
	const n = 200

	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("s%04d", i)
			require.NoError(t, s.DeliverSignal(wf, Signal{ID: id, Name: id, Payload: id}))
		}(i)
	}
	// Concurrent readers racing the writers — must never panic or return a torn row.
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for range 50 {
			got, err := s.TakeSignals(wf)
			require.NoError(t, err)
			for _, g := range got {
				// Each signal was delivered as {id, id, id}: name and payload must equal the id.
				assert.Equal(t, g.ID, g.Name, "torn row: name != id")
				assert.Equal(t, g.ID, g.Payload, "torn row: payload != id")
			}
		}
	}()
	wg.Wait()
	<-readerDone

	got, err := s.TakeSignals(wf)
	require.NoError(t, err)
	require.Len(t, got, n, "every distinct delivery lands exactly once")
	for _, g := range got {
		require.Equal(t, g.ID, g.Name)
		require.Equal(t, g.ID, g.Payload)
	}
}

// TestSQLiteSignalsAdv_LastWriterWinsConcurrentRedeliver races many re-deliveries of the SAME sig_id,
// each carrying a MATCHED (name_k, payload_k) pair. ON CONFLICT DO UPDATE is last-writer-wins; the
// invariant under contention is that the single surviving row is never TORN — its name and payload
// come from ONE delivery, not a mix of two.
func TestSQLiteSignalsAdv_LastWriterWinsConcurrentRedeliver(t *testing.T) {
	s := mustSQLiteAdv(t)
	const wf = "wf-lww"
	const id = "dup"
	const n = 200

	var wg sync.WaitGroup
	wg.Add(n)
	for k := range n {
		go func(k int) {
			defer wg.Done()
			tag := fmt.Sprintf("v%04d", k)
			require.NoError(t, s.DeliverSignal(wf, Signal{ID: id, Name: tag, Payload: tag}))
		}(k)
	}
	wg.Wait()

	got, err := s.TakeSignals(wf)
	require.NoError(t, err)
	require.Len(t, got, 1, "same id collapses to exactly one row regardless of concurrency")
	assert.Equal(t, id, got[0].ID)
	assert.Equal(t, got[0].Name, got[0].Payload,
		"last-writer-wins row must be internally consistent (not torn across two deliveries)")
}

// TestSQLiteSignalsAdv_OrderingDeterministic pins the ORDER BY sig_id contract: TakeSignals returns
// rows sorted lexicographically by sig_id, stably, regardless of delivery order — and idempotently
// across repeated non-destructive takes.
func TestSQLiteSignalsAdv_OrderingDeterministic(t *testing.T) {
	s := mustSQLiteAdv(t)
	const wf = "wf-order"
	// Delivered in a scrambled order, including numeric-looking ids (lexicographic, so "10" < "2").
	ids := []string{"10", "2", "banana", "1", "Apple", "apple", "20"}
	for _, id := range ids {
		require.NoError(t, s.DeliverSignal(wf, Signal{ID: id, Name: "n"}))
	}
	want := append([]string(nil), ids...)
	sort.Strings(want) // Go's sort.Strings is byte-lexicographic, matching SQLite's default BINARY collation.

	for take := range 3 {
		got, err := s.TakeSignals(wf)
		require.NoError(t, err)
		var gotIDs []string
		for _, g := range got {
			gotIDs = append(gotIDs, g.ID)
		}
		assert.Equal(t, want, gotIDs, "TakeSignals must be deterministically sorted by sig_id (take %d)", take)
	}
}

// TestSQLiteSignalsAdv_ValidationGuards confirms validateWorkflowID / validateSignalID fire on the
// SQLite path for all three methods. An empty id and a path-traversal / separator id are rejected
// with ErrValidation (a table row can't traverse a path, but the guard must be uniform across stores
// per the conformance contract) — and, critically, a rejected input NEVER reaches the DB.
func TestSQLiteSignalsAdv_ValidationGuards(t *testing.T) {
	s := mustSQLiteAdv(t)

	t.Run("deliver_empty_workflow_id", func(t *testing.T) {
		err := s.DeliverSignal("", Signal{ID: "x"})
		require.True(t, errors.Is(err, ErrValidation), "got %v", err)
	})
	t.Run("deliver_empty_sig_id", func(t *testing.T) {
		err := s.DeliverSignal("wf", Signal{ID: ""})
		require.True(t, errors.Is(err, ErrValidation), "got %v", err)
	})
	t.Run("deliver_traversal_sig_id", func(t *testing.T) {
		err := s.DeliverSignal("wf", Signal{ID: "../../escape"})
		require.True(t, errors.Is(err, ErrValidation), "got %v", err)
	})
	t.Run("take_empty_workflow_id", func(t *testing.T) {
		_, err := s.TakeSignals("")
		require.True(t, errors.Is(err, ErrValidation), "got %v", err)
	})
	t.Run("ack_empty_workflow_id", func(t *testing.T) {
		err := s.AckSignals("", []string{"x"})
		require.True(t, errors.Is(err, ErrValidation), "got %v", err)
	})
	t.Run("ack_traversal_sig_id", func(t *testing.T) {
		// A valid delivery, then an ack whose id list contains a traversal id — the guard rejects it
		// and, because AckSignals validates BEFORE deleting anything past the bad id, no valid signal
		// is silently dropped by the rejected call.
		require.NoError(t, s.DeliverSignal("wf-ack", Signal{ID: "keep"}))
		err := s.AckSignals("wf-ack", []string{"../../escape"})
		require.True(t, errors.Is(err, ErrValidation), "got %v", err)
		got, terr := s.TakeSignals("wf-ack")
		require.NoError(t, terr)
		require.Len(t, got, 1, "a rejected ack must not have deleted the valid signal")
	})
}

// TestSQLiteSignalsAdv_AckAtomicityAcrossReopen exercises ack + durability interleavings. Deliver 3,
// ack a subset (including one ABSENT id — idempotent 0-row delete), CLOSE (simulated crash), REOPEN,
// and confirm the mailbox is consistent: exactly the un-acked signals survive, and a re-ack after
// reopen is still idempotent.
func TestSQLiteSignalsAdv_AckAtomicityAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ackdur.db")
	const wf = "wf-ackdur"

	s1, err := NewSQLiteStore(path)
	require.NoError(t, err)
	for _, id := range []string{"s1", "s2", "s3"} {
		require.NoError(t, s1.DeliverSignal(wf, Signal{ID: id, Name: "n", Payload: id}))
	}
	// Ack s1 + s3 and an absent id — the absent id is a 0-row delete, not an error.
	require.NoError(t, s1.AckSignals(wf, []string{"s1", "s3", "does-not-exist"}))
	require.NoError(t, s1.Close()) // simulated crash after the ack committed

	s2, err := NewSQLiteStore(path)
	require.NoError(t, err)
	defer s2.Close() //nolint:errcheck // test cleanup
	got, err := s2.TakeSignals(wf)
	require.NoError(t, err)
	require.Len(t, got, 1, "only the un-acked signal survives the ack+reopen")
	assert.Equal(t, "s2", got[0].ID)

	// Re-acking after reopen is idempotent; acking the survivor drains the mailbox.
	require.NoError(t, s2.AckSignals(wf, []string{"s1", "s2"}))
	got, err = s2.TakeSignals(wf)
	require.NoError(t, err)
	require.Empty(t, got, "mailbox drained after acking the survivor")
}

// TestSQLiteSignalsAdv_MailboxOutsideSnapshotUnderConcurrency reinforces the MH37-1 invariant under
// contention: repeated concurrent Save (checkpoint) + DeliverSignal must never let a checkpoint drop
// a delivered signal. The signals table is SEPARATE from the snapshot rows, so no Save touches it.
func TestSQLiteSignalsAdv_MailboxOutsideSnapshotUnderConcurrency(t *testing.T) {
	s := mustSQLiteAdv(t)
	const wf = "wf-snap-conc"
	const n = 100

	var wg sync.WaitGroup
	wg.Add(2)
	// Writer 1: deliver n distinct signals.
	go func() {
		defer wg.Done()
		for i := range n {
			require.NoError(t, s.DeliverSignal(wf, Signal{ID: fmt.Sprintf("s%04d", i), Name: "n"}))
		}
	}()
	// Writer 2: hammer Save (checkpoint) concurrently.
	go func() {
		defer wg.Done()
		for i := range n {
			d := NewWorkflowData(wf)
			d.SetNodeStatus(fmt.Sprintf("node%d", i), Completed)
			require.NoError(t, s.Save(d))
		}
	}()
	wg.Wait()

	got, err := s.TakeSignals(wf)
	require.NoError(t, err)
	require.Len(t, got, n, "no delivered signal is lost to a concurrent checkpoint (mailbox-outside-snapshot)")
}
