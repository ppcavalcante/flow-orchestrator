package workflow

// SQLiteStore codec helpers (M15 ph66) — the type-collapse + reconstruction that
// makes a decomposed Load byte-identical to the FB/snapshot path. Kept beside the
// store so the fidelity-critical mapping is reviewable on its own.

import (
	"database/sql"
	"encoding/json"
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
func encodeKV(value interface{}) (kind int, iv *int64, fv *float64, sv *string) {
	switch v := value.(type) {
	case int:
		x := int64(v)
		return kvInt, &x, nil, nil
	case int32:
		x := int64(v)
		return kvInt, &x, nil, nil
	case int64:
		return kvInt, &v, nil, nil
	case bool:
		return kvBool, ptrInt(boolToInt(v)), nil, nil
	case float64:
		return kvFloat, nil, &v, nil
	case float32:
		x := float64(v)
		return kvFloat, nil, &x, nil
	case string:
		return kvString, nil, nil, &v
	default:
		// complex → JSON string (fmt fallback on marshal error), matching FB Save.
		var s string
		if b, err := json.Marshal(v); err == nil {
			s = string(b)
		} else {
			s = fmt.Sprintf("%v", v)
		}
		return kvString, nil, nil, &s
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
// byte-identical to the full-snapshot output.
func encodeOutput(output interface{}) string {
	if v, ok := output.(string); ok {
		return v
	}
	if b, err := json.Marshal(output); err == nil {
		return string(b)
	}
	return fmt.Sprintf("%v", output)
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
