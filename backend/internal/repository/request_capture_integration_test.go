//go:build integration

package repository

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/requestcapture"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRequestCaptureSQLLifecycle(t *testing.T) {
	ctx := context.Background()
	store := &requestcapture.SQLStore{DB: integrationDB}
	dir := t.TempDir()
	config := requestcapture.Config{Enabled: true, QuotaMiB: 1, RetentionDays: 7, IncludeDistillation: true, DistillationMinOutputTokens: 3000, DistillationNonStreamOnly: true}
	m, err := requestcapture.New(store, dir, config)
	require.NoError(t, err)
	defer m.Close()
	task, err := m.Create(ctx, requestcapture.CreateTask{TargetType: "user", TargetID: 1, DurationMinutes: 1}, "synthetic fixture")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.DeleteTask(ctx, m.InstanceID(), task.ID)) })
	begin := func(requestID string) *requestcapture.Session {
		var s *requestcapture.Session
		require.Eventually(t, func() bool {
			s = m.Begin(requestcapture.Meta{UserID: 1, RequestID: requestID})
			return s != nil
		}, time.Second, time.Millisecond)
		return s
	}
	s := begin("capture-sql-fixture")
	s.ClientRequest([]byte(`{"api_key":"DO-NOT-PERSIST","input":"PRIVATE-PAYLOAD"}`), "application/json", nil)
	s.Finish(403)
	require.Eventually(t, func() bool { return m.Stats().ActiveRequests == 0 }, 5*time.Second, 10*time.Millisecond)
	rows, err := m.Records(ctx, task.ID, "capture-sql-fixture", true, 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Empty(t, rows[0].Parts, "lists must omit large metadata arrays")
	record, err := m.Record(ctx, task.ID, rows[0].ID)
	require.NoError(t, err)
	require.Len(t, record.Parts, 1)
	require.True(t, record.IsError)
	text, _, _, err := m.Preview(ctx, task.ID, record.ID, record.Parts[0].Name, 0)
	require.NoError(t, err)
	require.Contains(t, text, "PRIVATE-PAYLOAD")
	require.NotContains(t, text, "DO-NOT-PERSIST")
	var metadata string
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT data::text FROM request_capture_records WHERE id=$1", record.ID).Scan(&metadata))
	require.False(t, strings.Contains(metadata, "PRIVATE-PAYLOAD"))
	require.False(t, strings.Contains(metadata, "DO-NOT-PERSIST"))
	_, err = store.Task(ctx, "another-instance", task.ID)
	require.Error(t, err)
	// Ordinary successful traffic leaves neither an index nor body files.
	success := begin("successful-request")
	success.ClientRequest([]byte("{\"input\":\"SUCCESS_MUST_DISAPPEAR\"}"), "application/json", nil)
	success.Finish(200)
	require.Eventually(t, func() bool { return m.Stats().ActiveRequests == 0 }, 5*time.Second, time.Millisecond)
	raw, err := store.Records(ctx, task.ID, "successful-request", false, 10, 0)
	require.NoError(t, err)
	require.Empty(t, raw)
	suspected := begin("suspected-request")
	suspected.ClientRequest([]byte(`{"stream":false}`), "application/json", nil)
	response := suspected.NewStream("client_response", 0, 0, "application/json", nil)
	_, err = response.Write([]byte(`{"usage":{"completion_tokens":3000,"prompt_tokens_details":{"cached_tokens":0}}}`))
	require.NoError(t, err)
	require.NoError(t, response.Close())
	suspected.Finish(200)
	require.Eventually(t, func() bool { return m.Stats().ActiveRequests == 0 }, 5*time.Second, time.Millisecond)
	rows, err = store.Records(ctx, task.ID, "suspected-request", false, 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	suspectedID := rows[0].ID
	require.True(t, rows[0].IsSuspectedDistillation)
	require.False(t, rows[0].IsError)
	require.Equal(t, &requestcapture.DistillationEvidence{OutputTokens: 3000}, rows[0].DistillationEvidence)
	rows, err = store.Records(ctx, task.ID, "suspected-request", true, 10, 0)
	require.NoError(t, err)
	require.Empty(t, rows)
	// Old success rows and unfinalized retained rows are hidden and purged on restart.
	now := time.Now().UTC()
	for _, hidden := range []*requestcapture.Record{
		{ID: uuid.NewString(), TaskID: task.ID, InstanceID: m.InstanceID(), CreatedAt: now, FinishedAt: &now},
		{ID: uuid.NewString(), TaskID: task.ID, InstanceID: m.InstanceID(), CreatedAt: now, IsError: true},
		{ID: uuid.NewString(), TaskID: task.ID, InstanceID: m.InstanceID(), CreatedAt: now, IsSuspectedDistillation: true},
	} {
		require.NoError(t, store.SaveRecord(ctx, hidden))
		visible, err := m.Records(ctx, task.ID, "", false, 10, 0)
		require.NoError(t, err)
		require.Len(t, visible, 2)
		_, err = m.Record(ctx, task.ID, hidden.ID)
		require.ErrorIs(t, err, requestcapture.ErrNotFound)
	}
	m.Close()
	config.IncludeDistillation = false
	reopened, err := requestcapture.New(store, dir, config)
	require.NoError(t, err)
	defer reopened.Close()
	var rowCount int
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT count(*) FROM request_capture_records WHERE task_id=$1", task.ID).Scan(&rowCount))
	require.Equal(t, 2, rowCount)
	retained, err := reopened.Record(ctx, task.ID, suspectedID)
	require.NoError(t, err)
	require.True(t, retained.IsSuspectedDistillation)
	require.NoError(t, reopened.Delete(ctx, task.ID))
	rows, err = store.Records(ctx, task.ID, "", false, 10, 0)
	require.NoError(t, err)
	require.Empty(t, rows)
	require.Zero(t, reopened.Stats().UsedBytes)
}
