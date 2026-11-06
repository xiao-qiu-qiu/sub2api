package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type reincludeHandlerRepo struct {
	service.QualityRuleTemplateRepository
	updates int
}

func (r *reincludeHandlerRepo) Get(context.Context, int64) (*service.QualityRuleTemplate, error) {
	return &service.QualityRuleTemplate{ID: 1, ModelID: "gpt-test", CronExpression: "*/30 * * * *", Enabled: false,
		PlanIDs: []int64{}, ExcludedAccountIDs: []int64{25},
		PelicanConfig: &service.PelicanTestConfig{QuestionKind: service.OpenAICodexStateProbeQuestionKind, ReasoningEffort: "high", ParallelCount: 1,
			Quality: &service.QualityPolicy{Action: "disable_scheduling", AutoRestore: true}}}, nil
}

func (r *reincludeHandlerRepo) Update(_ context.Context, tpl *service.QualityRuleTemplate) (*service.QualityRuleTemplate, error) {
	r.updates++
	return tpl, nil
}

func reincludeHandlerRequest(t *testing.T, body string) (*httptest.ResponseRecorder, *reincludeHandlerRepo) {
	t.Helper()
	repo := &reincludeHandlerRepo{}
	svc := service.ProvideScheduledTestService(nil, nil, repo, nil)
	h := NewScheduledTestHandler(svc)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.PUT("/templates/:id", h.UpdateQualityTemplate)
	req := httptest.NewRequest("PUT", "/templates/1", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w, repo
}

func TestQualityTemplateReincludeHandlerRejectsInvalidIDsBeforeUpdate(t *testing.T) {
	for _, ids := range []string{`[25,0]`, `[-1]`, `[9223372036854775808]`, `[1.5]`, `["25"]`, `[` + strings.Repeat("25,", service.MaxQualityTemplateReincludeAccounts) + `25]`} {
		w, repo := reincludeHandlerRequest(t, `{"cron_expression":"0 * * * *","reinclude_account_ids":`+ids+`}`)
		require.Equal(t, 400, w.Code, w.Body.String())
		require.Zero(t, repo.updates)
	}
}

func TestQualityTemplateReincludeHandlerPassesExplicitIDsAndReportsPause(t *testing.T) {
	w, repo := reincludeHandlerRequest(t, `{"reinclude_account_ids":[25]}`)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Equal(t, 1, repo.updates)
	var result service.QualityTemplateUpdate
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	require.Equal(t, []service.QualityTemplateReincludeResult{{AccountID: 25, Status: "failed", Reason: "template_disabled"}}, result.ReincludeResults)
	require.Equal(t, []int64{25}, result.Template.ExcludedAccountIDs)
	require.Zero(t, result.Failed)
	require.Zero(t, result.Created)

	w, repo = reincludeHandlerRequest(t, `{}`)
	require.Equal(t, 200, w.Code)
	require.Equal(t, 1, repo.updates)
	require.NotContains(t, w.Body.String(), "reinclude_results")
}
