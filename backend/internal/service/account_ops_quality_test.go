package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func qualityNotificationFixture() (*ScheduledTestPlan, *ScheduledTestResult) {
	finished := time.Now().Add(-time.Second)
	cfg := &PelicanTestConfig{QuestionKind: "candy", RuleName: "主探针", ModelID: "gpt-test", ParallelCount: 1,
		Quality: &QualityPolicy{Action: QualityActionObserveOnly}}
	plan := &ScheduledTestPlan{ID: 7, AccountID: 18, AccountName: "Example", ModelID: "gpt-test", PelicanConfig: cfg}
	result := &ScheduledTestResult{Status: "failed", ErrorMessage: "answer_mismatch", StartedAt: finished.Add(-time.Second), FinishedAt: finished,
		QualityJudgment: &QualityJudgment{Verdict: "incorrect", Reason: "private grading explanation"},
		ResponseText:    "private answer", PelicanConfig: cfg}
	return plan, result
}

func TestQualityDegradationEventRequiresConfirmedEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*ScheduledTestPlan, *ScheduledTestResult)
		want   bool
	}{
		{"wrong answer", func(*ScheduledTestPlan, *ScheduledTestResult) {}, true},
		{"state probe", func(p *ScheduledTestPlan, r *ScheduledTestResult) {
			p.PelicanConfig.QuestionKind = OpenAICodexStateProbeQuestionKind
			r.ErrorMessage = openAICodexStateDegradedError
		}, true},
		{"transport failure", func(_ *ScheduledTestPlan, r *ScheduledTestResult) { r.ErrorMessage = "HTTP 503" }, false},
		{"unknown judgment", func(_ *ScheduledTestPlan, r *ScheduledTestResult) { r.QualityJudgment.Verdict = "unknown" }, false},
		{"no judgment", func(_ *ScheduledTestPlan, r *ScheduledTestResult) { r.QualityJudgment = nil }, false},
		{"successful request", func(_ *ScheduledTestPlan, r *ScheduledTestResult) { r.Status = "success" }, false},
		{"skipped", func(_ *ScheduledTestPlan, r *ScheduledTestResult) { r.Status = "skipped" }, false},
		{"unfinished", func(_ *ScheduledTestPlan, r *ScheduledTestResult) { r.FinishedAt = time.Time{} }, false},
		{"future sample", func(_ *ScheduledTestPlan, r *ScheduledTestResult) { r.FinishedAt = time.Now().Add(time.Hour) }, false},
		{"missing start", func(_ *ScheduledTestPlan, r *ScheduledTestResult) { r.StartedAt = time.Time{} }, false},
		{"invalid timing", func(_ *ScheduledTestPlan, r *ScheduledTestResult) { r.StartedAt = r.FinishedAt.Add(time.Second) }, false},
		{"random pending", func(p *ScheduledTestPlan, r *ScheduledTestResult) {
			p.PelicanConfig.QuestionKind = QualityRandomQuestionKind
			r.PelicanConfig.QualityBatch = &QualityBatchEvidence{Decision: "pending"}
		}, false},
		{"random without passing peer", func(p *ScheduledTestPlan, r *ScheduledTestResult) {
			p.PelicanConfig.QuestionKind = QualityRandomQuestionKind
			p.QualityBatchEvaluated = true
			r.PelicanConfig.QualityBatch = &QualityBatchEvidence{Decision: "no_passing_peer"}
		}, false},
		{"random confirmed", func(p *ScheduledTestPlan, r *ScheduledTestResult) {
			p.PelicanConfig.QuestionKind = QualityRandomQuestionKind
			p.QualityBatchEvaluated = true
			r.PelicanConfig.QualityBatch = &QualityBatchEvidence{Decision: "peer_passed"}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, result := qualityNotificationFixture()
			tc.mutate(plan, result)
			SetQualityProbeFinishedAt(plan, []*ScheduledTestResult{result})
			event := QualityDegradationEvent(plan, "failed")
			if !tc.want {
				require.Nil(t, event)
				return
			}
			require.NotNil(t, event)
			require.Equal(t, "quality_degraded", event.Kind)
			require.Equal(t, "account:18", event.GroupID)
			require.Equal(t, "gpt-test", event.Details.QualityModel)
			require.Equal(t, "主探针", event.Details.QualityRuleName)
			require.Equal(t, result.FinishedAt, *event.Details.ObservedAt)
			serialized, err := json.Marshal(event)
			require.NoError(t, err)
			require.NotContains(t, string(serialized), "private")
			require.Nil(t, QualityDegradationEvent(plan, "inconclusive"))
			require.Nil(t, QualityDegradationEvent(plan, "passed"))
		})
	}
}

func TestQualityDegradationEventTracksFailedModelsAndClearsPriorEvidence(t *testing.T) {
	plan, failed := qualityNotificationFixture()
	copyFailed := *failed
	copyConfig := *failed.PelicanConfig
	copyFailed.PelicanConfig = &copyConfig
	copyConfig.ModelID = "gpt-other"
	passed := *failed
	passedConfig := *failed.PelicanConfig
	passedConfig.ModelID = "gpt-healthy"
	passed.PelicanConfig = &passedConfig
	passed.Status, passed.ErrorMessage = "success", ""
	passed.QualityJudgment = &QualityJudgment{Verdict: "correct"}
	SetQualityProbeFinishedAt(plan, []*ScheduledTestResult{failed, &copyFailed, failed, &passed})
	event := QualityDegradationEvent(plan, "failed")
	require.NotNil(t, event)
	require.Equal(t, "gpt-other, gpt-test", event.Details.QualityModel)
	SetQualityProbeFinishedAt(plan, []*ScheduledTestResult{&passed})
	require.Nil(t, QualityDegradationEvent(plan, "failed"), "a later healthy round cannot reuse old evidence")
	SetQualityProbeFinishedAt(plan, nil)
	require.Nil(t, plan.QualityDegradation)
}

func TestQualityDegradationEventIncludesConfirmedAdmissionWrongAnswer(t *testing.T) {
	plan, result := qualityNotificationFixture()
	plan.PelicanConfig.Quality.Action = QualityActionOpenAITwoFAAdmission
	result.ResponseText = "29"
	SetQualityProbeFinishedAt(plan, []*ScheduledTestResult{result})
	require.Equal(t, "answer_29", qualityRoundOutcome([]*ScheduledTestResult{result}))
	require.NotNil(t, QualityDegradationEvent(plan, "answer_29"))
	plan.PelicanConfig.Quality.Action = QualityActionObserveOnly
	require.Nil(t, QualityDegradationEvent(plan, "answer_29"), "the admission-only outcome cannot authorize other rules")
	plan.PelicanConfig.Quality.Action = QualityActionOpenAITwoFAAdmission
	result.Status, result.ErrorMessage, result.ResponseText = "failed", "HTTP 503", "29"
	SetQualityProbeFinishedAt(plan, []*ScheduledTestResult{result})
	require.Nil(t, QualityDegradationEvent(plan, "answer_29"), "a partial or failed request is not a confirmed wrong answer")
}

func TestAccountOpsQualityNotificationOptInAndScopedSave(t *testing.T) {
	ctx := context.Background()
	settings := &accountOpsSettingsStub{}
	svc := NewAccountOpsService(settings, &accountOpsRepoStub{}, nil)
	cfg := defaultAccountOpsConfig()
	require.False(t, cfg.QualityDegraded)
	cfg.Enabled, cfg.BalanceLow, cfg.WeeklyQuota = true, false, false
	cfg.Recipient = "ops@example.test"
	require.Error(t, ValidateAccountOpsConfig(cfg))
	cfg.QualityDegraded = true
	require.NoError(t, svc.SaveConfig(ctx, cfg))
	require.True(t, svc.currentConfig().Allows("quality_degraded"))
	require.False(t, svc.currentConfig().Allows("balance_low"))
	var input AccountOpsNotificationSettings
	require.NoError(t, json.Unmarshal([]byte(`{"quality_degraded":false}`), &input))
	// Set another enabled type first so switching quality off leaves a valid config.
	input.BalanceLow = opsBool(true)
	got, err := svc.SaveNotificationSettings(ctx, input)
	require.NoError(t, err)
	require.False(t, got.QualityDegraded)
	require.Equal(t, cfg.Recipient, got.Recipient)
	got, err = svc.SaveNotificationSettings(ctx, AccountOpsNotificationSettings{QualityDegraded: opsBool(true)})
	require.NoError(t, err)
	require.True(t, got.QualityDegraded)
	require.True(t, got.BalanceLow)
	got, err = svc.SaveNotificationSettings(ctx, AccountOpsNotificationSettings{EmailName: ptrQualityNotificationString("值班")})
	require.NoError(t, err)
	require.True(t, got.QualityDegraded, "editing a channel name must preserve the quality flag")
}

func ptrQualityNotificationString(value string) *string { return &value }

func TestAccountOpsQualityMessageUsesDetectionTimeAndEscapesNames(t *testing.T) {
	plan, result := qualityNotificationFixture()
	plan.AccountName = "<script>\n账号"
	plan.PelicanConfig.RuleName = "**主探针**"
	plan.PelicanConfig.QuestionKind = OpenAICodexStateProbeQuestionKind
	result.ErrorMessage = openAICodexStateDegradedError
	SetQualityProbeFinishedAt(plan, []*ScheduledTestResult{result})
	event := QualityDegradationEvent(plan, "failed")
	event.GroupName = "共享账号组"
	event.LastSeen = time.Now().Add(time.Hour)
	svc := NewAccountOpsService(&accountOpsSettingsStub{}, &accountOpsRepoStub{}, nil)
	message := svc.notificationMessage(event)
	require.Equal(t, "Sub2API · 账号降智", message.title)
	require.Contains(t, message.plain, "账号：<script> 账号 (#18)")
	require.Contains(t, message.plain, "模型：gpt-test")
	require.Contains(t, message.plain, "检测方式：状态探针")
	require.Contains(t, message.plain, result.FinishedAt.In(svc.timezone).Format("2006-01-02 15:04:05"))
	require.NotContains(t, message.plain, "余额")
	require.NotContains(t, message.plain, "共享账号组")
	require.NotContains(t, message.html, "<script>")
	require.Contains(t, message.html, "&lt;script&gt;")
	require.NotContains(t, message.html, "此提醒基于上游失败响应")
	require.NotContains(t, message.markdown, "：**主探针**")
	for _, part := range []string{message.plain, message.html, message.markdown} {
		require.NotContains(t, part, "private")
	}
	require.Equal(t, 5, len(strings.Split(message.plain, "\n")))
}
