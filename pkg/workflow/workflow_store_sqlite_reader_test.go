package workflow

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSQLiteReader_ClosedWALFidelityAndClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	writer, err := NewSQLiteStore(path, WithMultiProcess())
	require.NoError(t, err)
	t.Cleanup(func() { writer.Close() })
	fixture := fullFixture(t)
	require.NoError(t, writer.Save(fixture))
	queued, err := writer.Enqueue(fixture.GetWorkflowID(), "triage", nil)
	require.NoError(t, err)
	require.True(t, queued)
	spec, err := NewOneshotSchedule("review", "triage", time.Unix(1900000000, 0))
	require.NoError(t, err)
	created, err := writer.CreateSchedule(spec)
	require.NoError(t, err)
	require.True(t, created)
	pending, err := writer.ListPending(0)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	// Literal query characters must not become URI connection parameters.
	literal := filepath.Join(dir, "state?mode=rwc&name=%23.db")
	require.NoError(t, os.Rename(path, literal))
	path = literal
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	_, err = os.Stat(path + "-wal")
	require.ErrorIs(t, err, os.ErrNotExist)

	fb, err := NewFlatBuffersStore(filepath.Join(dir, "oracle"))
	require.NoError(t, err)
	require.NoError(t, fb.Save(fixture))
	oracle, err := fb.Load(fixture.GetWorkflowID())
	require.NoError(t, err)
	want, err := oracle.Snapshot()
	require.NoError(t, err)

	reader, err := OpenSQLiteReader(path)
	require.NoError(t, err)
	t.Cleanup(func() { reader.Close() })
	ids, err := reader.ListWorkflows()
	require.NoError(t, err)
	require.Equal(t, []string{fixture.GetWorkflowID()}, ids)
	gotPending, err := reader.ListPending(pending[0].EnqueuedAt)
	require.NoError(t, err)
	require.Equal(t, pending, gotPending)
	emptyPending, err := reader.ListPending(pending[0].EnqueuedAt - 1)
	require.NoError(t, err)
	require.Empty(t, emptyPending)
	status, err := reader.WorkflowStatus(fixture.GetWorkflowID())
	require.NoError(t, err)
	require.True(t, status.Queued)
	require.Equal(t, "pending", status.State)
	schedules, err := reader.ListSchedules()
	require.NoError(t, err)
	require.Len(t, schedules, 1)
	require.Equal(t, "review", schedules[0].ID)
	require.Equal(t, int64(1900000000000000000), schedules[0].NextFireTime)
	loaded, err := reader.Load(fixture.GetWorkflowID())
	require.NoError(t, err)
	got, err := loaded.Snapshot()
	require.NoError(t, err)
	require.Equal(t, want, got)
	loaded.Set("region", "memory-only")
	require.NoError(t, reader.Close())
	require.NoError(t, reader.Close())
	_, err = reader.Load(fixture.GetWorkflowID())
	require.Error(t, err)
	_, err = reader.ListWorkflows()
	require.Error(t, err)
	_, err = reader.ListPending(0)
	require.Error(t, err)
	_, err = reader.WorkflowStatus(fixture.GetWorkflowID())
	require.Error(t, err)
	_, err = reader.ListSchedules()
	require.Error(t, err)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after, "reader open/read/close must not alter the database")
}

func TestSQLiteReader_RefusesWithoutInitialization(t *testing.T) {
	for _, state := range []string{"missing-parent", "empty", "garbage", "directory", "incompatible"} {
		t.Run(state, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "state.db")
			switch state {
			case "missing-parent":
				path = filepath.Join(dir, "missing", "state.db")
			case "empty":
				require.NoError(t, os.WriteFile(path, nil, 0600))
			case "garbage":
				require.NoError(t, os.WriteFile(path, []byte("not a sqlite database"), 0600))
			case "directory":
				require.NoError(t, os.Mkdir(path, 0700))
			case "incompatible":
				writer, err := NewSQLiteStore(path)
				require.NoError(t, err)
				_, err = writer.db.Exec("ALTER TABLE workflows RENAME COLUMN trigger_cause TO unsupported")
				require.NoError(t, err)
				require.NoError(t, writer.Close())
			}
			before, beforeErr := os.ReadFile(path)
			reader, err := OpenSQLiteReader(path)
			require.Error(t, err)
			require.Nil(t, reader)
			after, afterErr := os.ReadFile(path)
			if beforeErr == nil {
				require.NoError(t, afterErr)
				require.Equal(t, before, after)
			}
			if state == "missing-parent" {
				_, err = os.Stat(filepath.Dir(path))
				require.ErrorIs(t, err, os.ErrNotExist)
			}
		})
	}
}

func TestSQLiteReader_LoadErrorsReleaseSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	writer, err := NewSQLiteStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { writer.Close() })
	require.NoError(t, writer.Save(NewWorkflowData("valid")))
	bad := NewWorkflowData("bad")
	bad.SetNodeStatus("node", Completed)
	require.NoError(t, writer.Save(bad))
	_, err = writer.db.Exec("UPDATE nodes SET status='unknown' WHERE workflow_id='bad'")
	require.NoError(t, err)
	reader, err := OpenSQLiteReader(path)
	require.NoError(t, err)
	t.Cleanup(func() { reader.Close() })
	_, err = reader.Load("")
	require.ErrorIs(t, err, ErrValidation)
	_, err = reader.Load("missing")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = reader.Load("bad")
	require.ErrorIs(t, err, ErrCorruptData)
	valid, err := reader.Load("valid")
	require.NoError(t, err)
	require.Equal(t, "valid", valid.GetWorkflowID())
}

func readerVersion(version int64) *WorkflowData {
	d := NewWorkflowData("versioned")
	d.Set("version", version)
	d.SetRollingBack(version%2 != 0)
	for i := range 192 {
		name := fmt.Sprintf("n%03d", i)
		d.SetNodeStatus(name, Waiting)
		d.SetOutput(name, strconv.FormatInt(version, 10))
		d.SetWait(name, version+1)
	}
	return d
}

func checkReaderVersion(t *testing.T, data *WorkflowData) int64 {
	t.Helper()
	version, ok := Get(data, NewKey[int64]("version"))
	require.True(t, ok)
	require.Equal(t, version%2 != 0, data.IsRollingBack())
	for i := range 192 {
		name := fmt.Sprintf("n%03d", i)
		out, ok := data.GetOutput(name)
		require.True(t, ok)
		require.Equal(t, strconv.FormatInt(version, 10), out, "mixed committed versions")
		fire, ok := data.GetWait(name)
		require.True(t, ok)
		require.Equal(t, version+1, fire, "mixed committed versions")
	}
	return version
}

func TestSQLiteReader_CrossProcessCommittedSnapshots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	seed, err := NewSQLiteStore(path, WithMultiProcess())
	require.NoError(t, err)
	require.NoError(t, seed.Save(readerVersion(0)))
	require.NoError(t, seed.Close())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSQLiteReader_ProcessFixture$")
	cmd.Env = append(os.Environ(), "SQLITE_READER_FIXTURE=writer", "SQLITE_READER_PATH="+path)
	input, err := cmd.StdinPipe()
	require.NoError(t, err)
	output, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { cancel(); input.Close(); cmd.Wait() })
	scanner := bufio.NewScanner(output)
	require.True(t, scanner.Scan())
	require.Equal(t, "ready", scanner.Text())
	reader, err := OpenSQLiteReader(path)
	require.NoError(t, err)
	t.Cleanup(func() { reader.Close() })
	_, err = fmt.Fprintln(input, "churn")
	require.NoError(t, err)
	for range 120 {
		data, err := reader.Load("versioned")
		require.NoError(t, err)
		checkReaderVersion(t, data)
	}
	require.True(t, scanner.Scan())
	require.Equal(t, "committed", scanner.Text())
	data, err := reader.Load("versioned")
	require.NoError(t, err)
	require.Equal(t, int64(80), checkReaderVersion(t, data))
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	walBefore, err := os.ReadFile(path + "-wal")
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	walAfter, err := os.ReadFile(path + "-wal")
	require.NoError(t, err)
	require.Equal(t, before, after, "reader close must not checkpoint")
	require.Equal(t, walBefore, walAfter, "reader close must not alter WAL records")
	_, err = fmt.Fprintln(input, "last")
	require.NoError(t, err)
	require.True(t, scanner.Scan())
	require.Equal(t, "last-committed", scanner.Text(), "reader close must not prevent writer progress")
	require.NoError(t, input.Close())
	require.NoError(t, cmd.Wait())
}

func TestSQLiteReader_HotJournalRefusesRepair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	seed, err := NewSQLiteStore(path)
	require.NoError(t, err)
	d := NewWorkflowData("crash")
	for i := range 100 {
		d.Set(fmt.Sprintf("key-%d", i), "before")
	}
	require.NoError(t, seed.Save(d))
	require.NoError(t, seed.Close())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSQLiteReader_ProcessFixture$")
	cmd.Env = append(os.Environ(), "SQLITE_READER_FIXTURE=hot-journal", "SQLITE_READER_PATH="+path)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	journal, err := os.ReadFile(path + "-journal")
	require.NoError(t, err)
	require.Greater(t, len(journal), 512)
	reader, err := OpenSQLiteReader(path)
	require.Error(t, err, "hot rollback journal requires database writes to recover")
	require.Nil(t, reader)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after)
	journalAfter, err := os.ReadFile(path + "-journal")
	require.NoError(t, err)
	require.Equal(t, journal, journalAfter)
}

func TestSQLiteReader_ProcessFixture(t *testing.T) {
	mode := os.Getenv("SQLITE_READER_FIXTURE")
	if mode == "" {
		return
	}
	path := os.Getenv("SQLITE_READER_PATH")
	if mode == "hot-journal" {
		db, err := sql.Open("sqlite", path)
		require.NoError(t, err)
		_, err = db.Exec("PRAGMA journal_mode=DELETE; PRAGMA cache_size=1; BEGIN IMMEDIATE; UPDATE data_kv SET s_val=zeroblob(8192)")
		require.NoError(t, err)
		os.Exit(0) // intentional crash: leave a genuine hot journal, no rollback or Close
	}
	writer, err := NewSQLiteStore(path, WithMultiProcess())
	require.NoError(t, err)
	defer writer.Close()
	fmt.Println("ready")
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		switch scanner.Text() {
		case "churn":
			for i := int64(1); i <= 80; i++ {
				require.NoError(t, writer.Save(readerVersion(i)))
			}
			fmt.Println("committed")
		case "last":
			require.NoError(t, writer.Save(readerVersion(81)))
			fmt.Println("last-committed")
		}
	}
	require.NoError(t, scanner.Err())
}
