package workflow

// SQLiteStore codec helpers (M15 ph66) — the type-collapse + reconstruction that
// makes a decomposed Load byte-identical to the FB/snapshot path. Kept beside the
// store so the fidelity-critical mapping is reviewable on its own.

import (
	"database/sql"
	"fmt"
	"time"
)

// idCol returns the id column name for a decomposed table (the workflows table's PK
// is `id`; the child tables reference it via `workflow_id`).
func idCol(table string) string {
	if table == "workflows" {
		return "id"
	}
	return "workflow_id"
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// unixNanoNow is the workflows.updated_at value. It is NOT part of the fidelity
// surface (createSnapshot does not serialize a timestamp), so it does not affect the
// Snapshot-byte-identity — it exists for the ph69 indexed-visibility query.
func unixNanoNow() int64 { return time.Now().UnixNano() }

// encodeKV maps a data value to (kind, i_val, f_val, s_val), mirroring the FB store's
// typed-vector dispatch EXACTLY so Load reconstructs the identical Go type:
//
//	int/int32/int64 → kvInt (int64)      float32/float64 → kvFloat (float64)
//	bool → kvBool                        string + complex → kvString (JSON/string)
//
// Only the column for the chosen kind is non-nil; the rest are nil (SQLite NULL).
//
// THE ERROR RETURN IS THE (iii-c) WIRING for the default arm's marshal. It takes `key`
// solely so the refusal can name the value it rejected: all three callers walk data with
// a ForEach whose callback returns nothing, so a bare depth error arriving at Save would
// tell a host that some value is too deep and not which. The error is captured beside the
// encode and surfaced at each caller's own fallible frame — see encodeHostValue for why
// the check cannot be hoisted into a pre-pass over data.
func encodeKV(key string, value interface{}) (kind int, iv *int64, fv *float64, sv *string, err error) {
	switch v := value.(type) {
	case int:
		x := int64(v)
		return kvInt, &x, nil, nil, nil
	case int32:
		x := int64(v)
		return kvInt, &x, nil, nil, nil
	case int64:
		return kvInt, &v, nil, nil, nil
	case bool:
		return kvBool, ptrInt(boolToInt(v)), nil, nil, nil
	case float64:
		return kvFloat, nil, &v, nil, nil
	case float32:
		x := float64(v)
		return kvFloat, nil, &x, nil, nil
	case string:
		return kvString, nil, nil, &v, nil
	default:
		// complex → JSON string (fmt fallback on marshal error), matching FB Save —
		// including, now, the same two depth guards around the same marshal. The two
		// stores encode the SAME value into the SAME string and are asserted
		// byte-identical, so a guard on one and not the other would be a divergence
		// this store's fidelity tests would report as corruption.
		s, derr := encodeHostValue(v, fmt.Sprintf("data key %q", key))
		if derr != nil {
			return 0, nil, nil, nil, derr
		}
		return kvString, nil, nil, &s, nil
	}
}

func ptrInt(v int64) *int64 { return &v }

// scanDataKV reconstructs typed data entries onto data, matching the FB reconstruction
// (kvInt → int64, kvBool → bool, kvFloat → float64, kvString → string). A row that
// fails to scan surfaces a typed ErrCorruptData, never a panic.
func scanDataKV(rows *sql.Rows, data *WorkflowData) (err error) {
	defer rows.Close() //nolint:errcheck // read-only; scan errors are the signal
	for rows.Next() {
		var (
			key  string
			kind int
			iv   sql.NullInt64
			fv   sql.NullFloat64
			sv   sql.NullString
		)
		if serr := rows.Scan(&key, &kind, &iv, &fv, &sv); serr != nil {
			return fmt.Errorf("%w: scan data row: %w", ErrCorruptData, serr)
		}
		switch kind {
		case kvInt:
			data.Set(key, iv.Int64)
		case kvBool:
			data.Set(key, iv.Int64 != 0)
		case kvFloat:
			data.Set(key, fv.Float64)
		case kvString:
			data.Set(key, sv.String)
		default:
			return fmt.Errorf("%w: unknown data kind %d for key %q", ErrCorruptData, kind, key)
		}
	}
	if rerr := rows.Err(); rerr != nil {
		return fmt.Errorf("%w: data rows: %w", ErrCorruptData, rerr)
	}
	return nil
}

// scanNodes reconstructs node status + output. output is stored as the raw encoded
// string (mirroring the FB store, which keeps the output string un-decoded), so
// SetOutput receives the same string the FB path yields.
func scanNodes(rows *sql.Rows, data *WorkflowData) error {
	defer rows.Close() //nolint:errcheck // read-only
	for rows.Next() {
		var (
			node      string
			status    string
			output    sql.NullString
			hasOutput int64
		)
		if serr := rows.Scan(&node, &status, &output, &hasOutput); serr != nil {
			return fmt.Errorf("%w: scan node row: %w", ErrCorruptData, serr)
		}
		// '' is the output-only sentinel: the node has an output but no status entry
		// (the FB store keeps outputs/nodeStatus independent). Restore only the output,
		// never inventing a status. A non-empty status must be a known one (corrupt-DB guard).
		if status != "" {
			if !isKnownStatus(NodeStatus(status)) {
				return fmt.Errorf("%w: node %q has unknown status %q", ErrCorruptData, node, status)
			}
			data.SetNodeStatus(node, NodeStatus(status))
		}
		if hasOutput != 0 {
			data.SetOutput(node, decodeOutput(output.String))
		}
	}
	if rerr := rows.Err(); rerr != nil {
		return fmt.Errorf("%w: node rows: %w", ErrCorruptData, rerr)
	}
	return nil
}

// isKnownStatus guards a corrupt/forged DB from injecting a bogus NodeStatus string.
func isKnownStatus(s NodeStatus) bool {
	switch s {
	case Pending, Running, Completed, Failed, Skipped, Waiting, Bypassed, Compensated, CompensationFailed:
		return true
	}
	return false
}

// encodeOutput mirrors the FB store's output serialization: a string passes through;
// anything else is JSON-marshalled (fmt fallback on error). So a decomposed output is
// byte-identical to the full-snapshot output — including, now, the two depth guards,
// which must be identical for the same reason the encoding is.
//
// The error return is the (iii-c) wiring; `node` names the refused value. A string output
// passes through unchecked exactly as before: it is stored verbatim and returned verbatim
// by decodeOutput, so it never passes a JSON decoder and has no nesting one could refuse.
func encodeOutput(node string, output interface{}) (string, error) {
	if v, ok := output.(string); ok {
		return v, nil
	}
	return encodeHostValue(output, fmt.Sprintf("output of node %q", node))
}

// decodeOutput mirrors the FB/JSON Load: the stored output is kept as the raw string,
// NOT decoded (SetOutput receives the same string the FB path yields). Identity.
func decodeOutput(s string) interface{} { return s }

// scanWaits reconstructs durable timer fireAt (M10).
func scanWaits(rows *sql.Rows, data *WorkflowData) error {
	defer rows.Close() //nolint:errcheck // read-only
	for rows.Next() {
		var (
			node   string
			fireAt int64
		)
		if serr := rows.Scan(&node, &fireAt); serr != nil {
			return fmt.Errorf("%w: scan wait row: %w", ErrCorruptData, serr)
		}
		data.SetWait(node, fireAt)
	}
	if rerr := rows.Err(); rerr != nil {
		return fmt.Errorf("%w: wait rows: %w", ErrCorruptData, rerr)
	}
	return nil
}
