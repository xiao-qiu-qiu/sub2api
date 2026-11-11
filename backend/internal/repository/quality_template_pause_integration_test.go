//go:build integration

package repository

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// Use real service discovery and PostgreSQL repositories. Only the upstream
// HTTP boundary is controlled; no production or external endpoints are called.
type qualityPauseModelsUpstream struct {
	do    func(*http.Request) (*http.Response, error)
	calls atomic.Int32
}

func (u *qualityPauseModelsUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.calls.Add(1)
	return u.do(req)
}
func (u *qualityPauseModelsUpstream) DoWithTLS(req *http.Request, proxy string, id int64, n int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, id, n)
}

func qualityPauseModelsFixture(t *testing.T, u *qualityPauseModelsUpstream) (*qualityReincludeFixture, *service.ScheduledTestService) {
	t.Helper()
	f := newQualityReincludeFixture(t)
	f.tpl.PelicanConfig.QuestionKind = "candy"
	f.tpl.PelicanConfig.TestChannel = "account"
	f.tpl.PelicanConfig.Prompt = "Synthetic question"
	f.tpl.PelicanConfig.Quality.ExpectedAnswer = "42"
	updated, err := f.svc.UpdateQualityTemplate(f.ctx, f.tpl, f.account)
	require.NoError(t, err)
	require.Equal(t, 1, updated.Created)
	f.tpl = updated.Template
	_, err = integrationDB.ExecContext(f.ctx, `UPDATE accounts SET type='apikey',credentials='{"api_key":"synthetic-test-key","base_url":"https://quality-models.example"}' WHERE id=$1`, f.account)
	require.NoError(t, err)
	accounts := NewAccountRepository(integrationEntClient, integrationDB, nil)
	cfg := &config.Config{}
	gateway := service.NewOpenAIGatewayService(accounts, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, u, nil, nil, nil, nil, nil, nil, nil, nil)
	accountTests := service.NewAccountTestService(accounts, nil, nil, nil, nil, nil, cfg, nil)
	accountTests.SetOpenAIGatewayService(gateway)
	return f, service.ProvideScheduledTestService(f.plans, NewScheduledTestResultRepository(integrationDB), f.repo, accountTests)
}

func TestQualityTemplatePauseIgnoresModelDiscoveryFailure(t *testing.T) {
	upstream := &qualityPauseModelsUpstream{do: func(*http.Request) (*http.Response, error) {
		return nil, errors.New("synthetic model discovery failure")
	}}
	f, svc := qualityPauseModelsFixture(t, upstream)
	// Establish that the discovery path really fails for an enabled update.
	failed, err := svc.UpdateQualityTemplate(f.ctx, f.tpl)
	require.NoError(t, err)
	require.Equal(t, 1, failed.Failed)
	require.Greater(t, upstream.calls.Load(), int32(0))
	plan, err := f.plans.GetByID(f.ctx, f.tpl.PlanIDs[0])
	require.NoError(t, err)
	require.True(t, plan.Enabled)
	before := upstream.calls.Load()
	paused, err := f.repo.Get(f.ctx, f.tpl.ID)
	require.NoError(t, err)
	paused.Enabled = false
	result, err := svc.UpdateQualityTemplate(f.ctx, paused)
	require.NoError(t, err)
	require.Zero(t, result.Failed)
	require.Equal(t, 1, result.Updated)
	require.Equal(t, before, upstream.calls.Load(), "pause must not enter model discovery")
	plan, err = f.plans.GetByID(f.ctx, plan.ID)
	require.NoError(t, err)
	require.False(t, plan.Enabled)
	require.False(t, result.Template.Enabled)
}

func TestQualityTemplatePauseWinsSlowSuccessfulModelDiscovery(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	upstream := &qualityPauseModelsUpstream{do: func(req *http.Request) (*http.Response, error) {
		once.Do(func() { close(entered) })
		select {
		case <-release:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"object":"list","data":[{"id":"gpt-test"}]}`))}, nil
	}}
	f, slowSvc := qualityPauseModelsFixture(t, upstream)
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	old, err := f.repo.Get(f.ctx, f.tpl.ID)
	require.NoError(t, err)
	old.CronExpression = "11 */2 * * *"
	old.PelicanConfig.RuleName = "slow old update"
	type output struct {
		result *service.QualityTemplateUpdate
		err    error
	}
	done := make(chan output, 1)
	go func() { result, err := slowSvc.UpdateQualityTemplate(f.ctx, old); done <- output{result, err} }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("model discovery was not entered")
	}
	latest, err := f.repo.Get(f.ctx, f.tpl.ID)
	require.NoError(t, err)
	latest.Enabled = false
	latest.CronExpression = "9 */3 * * *"
	latest.PelicanConfig.RuleName = "new pause wins"
	// This service uses the same production repositories; pausing needs no gateway.
	pauseResult, err := f.svc.UpdateQualityTemplate(f.ctx, latest)
	require.NoError(t, err)
	require.Zero(t, pauseResult.Failed)
	plan, err := f.plans.GetByID(f.ctx, f.tpl.PlanIDs[0])
	require.NoError(t, err)
	require.False(t, plan.Enabled)
	require.Equal(t, latest.CronExpression, plan.CronExpression)
	unblock()
	select {
	case out := <-done:
		require.NoError(t, out.err)
		require.Equal(t, 1, out.result.Failed)
		require.False(t, out.result.Template.Enabled)
	case <-time.After(5 * time.Second):
		t.Fatal("old update did not finish")
	}
	plan, err = f.plans.GetByID(f.ctx, plan.ID)
	require.NoError(t, err)
	require.False(t, plan.Enabled)
	require.Equal(t, latest.CronExpression, plan.CronExpression)
	require.Equal(t, "new pause wins", plan.PelicanConfig.RuleName)
	// A direct per-account edit cannot resurrect a child of a paused template.
	plan.Enabled = true
	_, err = f.plans.Update(f.ctx, plan)
	require.Error(t, err)
}

func TestQualityTemplatePauseFencesClaimAndInFlightAction(t *testing.T) {
	for _, action := range []string{"disable_scheduling", service.QualityActionObserveOnly} {
		t.Run(action, func(t *testing.T) {
			f := newQualityReincludeFixture(t)
			f.tpl.PelicanConfig.Quality.Action = action
			updated, err := f.svc.UpdateQualityTemplate(f.ctx, f.tpl, f.account)
			require.NoError(t, err)
			plan, err := f.plans.GetByID(f.ctx, updated.Template.PlanIDs[0])
			require.NoError(t, err)
			require.NoError(t, f.plans.TriggerQuality(f.ctx, plan.ID))
			plan, err = f.plans.GetByID(f.ctx, plan.ID)
			require.NoError(t, err)
			now := time.Now().Add(time.Second)
			until := now.Add(time.Minute)
			ok, err := f.plans.ClaimPelican(f.ctx, plan, now, until, now.Add(time.Hour))
			require.NoError(t, err)
			require.True(t, ok)
			// Pause only the template row: the database fence is atomic even if
			// application-level propagation is delayed or fails afterwards.
			_, err = integrationDB.ExecContext(f.ctx, `UPDATE quality_rule_templates SET enabled=false WHERE id=$1`, f.tpl.ID)
			require.NoError(t, err)
			current, err := f.plans.GetByID(f.ctx, plan.ID)
			require.NoError(t, err)
			require.False(t, current.Enabled)
			got, err := f.plans.ApplyQualityOutcome(f.ctx, plan, until, "failed")
			require.NoError(t, err)
			require.Equal(t, "stale_run", got)
			var schedulable bool
			require.NoError(t, integrationDB.QueryRowContext(f.ctx, `SELECT schedulable FROM accounts WHERE id=$1`, f.account).Scan(&schedulable))
			require.True(t, schedulable)
			// Simulate an old child flag drifting true; claim must consult the template.
			_, err = integrationDB.ExecContext(f.ctx, `UPDATE scheduled_test_plans SET enabled=true,running_until=NULL,next_run_at=NOW(),updated_at=clock_timestamp() WHERE id=$1`, plan.ID)
			require.NoError(t, err)
			current, err = f.plans.GetByID(f.ctx, plan.ID)
			require.NoError(t, err)
			ok, err = f.plans.ClaimPelican(f.ctx, current, now, until, now.Add(time.Hour))
			require.NoError(t, err)
			require.False(t, ok)
		})
	}
}
