package dto

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUsageQualityWindowOnlyVisibleToAdmin(t *testing.T) {
	start := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	for _, interrupted := range []bool{false, true} {
		log := &service.UsageLog{
			RequestStartedAt:   &end,
			QualityInterrupted: interrupted,
			QualityWindow:      &service.QualityWindowEvidence{StartAt: start, EndAt: end, Interrupted: interrupted},
		}
		for _, userDTO := range []*UsageLog{UsageLogFromService(log), UsageLogFromServiceForUser(log, true)} {
			raw, err := json.Marshal(userDTO)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "quality_")
			require.NotContains(t, string(raw), "request_started_at")
		}
		raw, err := json.Marshal(UsageLogFromServiceAdmin(log))
		require.NoError(t, err)
		var got map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(raw, &got))
		want, err := json.Marshal(map[string]any{"start_at": start, "end_at": end, "interrupted": interrupted})
		require.NoError(t, err)
		require.JSONEq(t, string(want), string(got["quality_window"]))
		require.NotContains(t, got, "quality_interrupted")
		require.Equal(t, `"2026-11-10T11:00:00Z"`, string(got["request_started_at"]))
	}
}

func TestUsageQualityWindowAdminNullWithoutEvidence(t *testing.T) {
	raw, err := json.Marshal(UsageLogFromServiceAdmin(&service.UsageLog{QualityInterrupted: true}))
	require.NoError(t, err)
	var got map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Equal(t, "null", string(got["quality_window"]))
	require.Equal(t, "null", string(got["request_started_at"]))
}
