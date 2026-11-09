package service

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/accountrequest"
	"github.com/stretchr/testify/require"
)

func TestQualityInterruptDoesNotPenalizeScheduler(t *testing.T) {
	cfg := DefaultPrioritySchedulingConfig()
	cfg.Enabled = true
	svc := priorityGateway(cfg, &priorityReaderStub{})
	now := time.Now()
	svc.openaiAccountStats = newOpenAIAccountRuntimeStats()
	svc.openaiAccountStats.now = func() time.Time { return now }
	account := excelAccount()
	ttft := 450
	svc.ReportOpenAIAccountScheduleResult(account, "gpt-6-astra", true, &ttft)
	svc.ReportOpenAIAccountScheduleResult(account, "gpt-6-astra", false, nil, errors.New("upstream error"))
	beforeRate, beforeTTFT, beforeKnown := svc.openaiAccountStats.snapshot(account.ID)
	for range 6 {
		err := fmt.Errorf("forward: %w", accountrequest.ErrQualityDegraded)
		require.False(t, svc.ReportOpenAIAccountScheduleResult(account, "gpt-6-astra", false, nil, err))
	}
	rate, latency, known := svc.openaiAccountStats.snapshot(account.ID)
	require.Equal(t, beforeRate, rate, "stopping six requests must neither penalize nor heal the account")
	require.Equal(t, beforeTTFT, latency)
	require.Equal(t, beforeKnown, known)
	svc.ReportOpenAIAccountScheduleResult(account, "gpt-6-astra", false, nil, errors.New("upstream error"))
	rate, _, _ = svc.openaiAccountStats.snapshot(account.ID)
	require.Greater(t, rate, beforeRate, "real upstream failures still count")
}
