//go:build integration

package repository

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestQualityOverviewIntegrationUnknownReasons(t *testing.T) {
	type sample struct {
		status, verdict, message, action string
		routeMissing                     bool
	}
	for _, tc := range []struct {
		name         string
		samples      []sample
		want         []string
		expected     int
		questionKind string
	}{
		{name: "quota", samples: []sample{{"skipped", "unknown", "account_quota_exhausted", "inconclusive", false}}, want: []string{"quota_exhausted"}},
		{name: "quota without committed action", samples: []sample{{"skipped", "unknown", "account_quota_exhausted", "", false}}, want: []string{"quota_exhausted"}},
		{name: "Astra passed Sol HTTP 429", samples: []sample{{"success", "correct", "", "observed", false}, {"failed", "unknown", "HTTP 429 PRIVATE_ERROR_TOKEN", "inconclusive", false}}, want: []string{"rate_limited"}},
		{name: "explicit BPS rate code", samples: []sample{{"failed", "unknown", "Excel BPS upstream failure: rate_limit_exceeded; request was not replayed", "inconclusive", false}}, want: []string{"rate_limited"}},
		{name: "state probe rate code", questionKind: "state_probe", samples: []sample{{"failed", "unknown", "state_probe_inconclusive: rate_limited", "inconclusive", false}}, want: []string{"rate_limited"}},
		{name: "both passed stale", samples: []sample{{"success", "correct", "", "stale_run", false}, {"success", "correct", "", "stale_run", false}}, want: []string{"stale_result"}},
		{name: "earlier stale latest observed", samples: []sample{{"success", "correct", "", "stale_run", false}, {"success", "correct", "", "observed", false}}, want: []string{"stale_result"}},
		{name: "mixed reasons", samples: []sample{{"success", "correct", "", "stale_run", false}, {"skipped", "unknown", "account_quota_exhausted", "inconclusive", false}, {"failed", "unknown", "rate_limit_error", "inconclusive", false}, {"success", "correct", "", "comparison_pending", false}, {"success", "correct", "", "observed", true}}, want: []string{"stale_result", "quota_exhausted", "rate_limited", "route_unverified", "comparison_pending"}},
		{name: "route missing", samples: []sample{{"success", "correct", "", "observed", true}}, want: []string{"route_unverified"}},
		{name: "comparison pending", samples: []sample{{"success", "correct", "", "comparison_pending", false}}, want: []string{"comparison_pending"}},
		{name: "partial round", expected: 2, samples: []sample{{"success", "correct", "", "observed", false}}, want: []string{"incomplete_round"}},
		{name: "stale plus missing sample", expected: 2, samples: []sample{{"success", "correct", "", "stale_run", false}}, want: []string{"stale_result", "incomplete_round"}},
		{name: "HTTP 503 is not degraded", samples: []sample{{"failed", "incorrect", "HTTP 503 PRIVATE_ERROR", "inconclusive", false}}, want: []string{"probe_failed"}},
		{name: "generic terminal error is not rate limit", samples: []sample{{"failed", "unknown", "Excel BPS returned terminal error event PRIVATE_ERROR", "inconclusive", false}}, want: []string{"probe_failed"}},
		{name: "unrelated 429 is not rate limit", samples: []sample{{"failed", "unknown", "terminal error request_id=429", "inconclusive", false}}, want: []string{"probe_failed"}},
		{name: "quota requires exact marker", samples: []sample{{"failed", "unknown", "insufficient_quota", "inconclusive", false}}, want: []string{"probe_failed"}},
		{name: "skipped", samples: []sample{{"skipped", "unknown", "model_unsupported", "inconclusive", false}}, want: []string{"skipped"}},
		{name: "independent transport failure and limit", samples: []sample{{"failed", "unknown", "HTTP 503", "inconclusive", false}, {"failed", "unknown", "rate_limited", "inconclusive", false}}, want: []string{"rate_limited", "probe_failed"}},
		{name: "quota and independent skip", samples: []sample{{"skipped", "unknown", "account_quota_exhausted", "inconclusive", false}, {"skipped", "unknown", "model_unsupported", "inconclusive", false}}, want: []string{"quota_exhausted", "skipped"}},
		{name: "inconclusive answer", questionKind: "random_reasoning", samples: []sample{{"failed", "incorrect", "answer_mismatch", "inconclusive", false}}, want: []string{"inconclusive"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newQualityOverviewFixture(t)
			plan := f.plan(f.account("-reasons"), "account")
			expected := tc.expected
			if expected == 0 {
				expected = len(tc.samples)
			}
			models := []string{"gpt-6-astra", "gpt-6-sol", "test-c", "test-d", "test-e"}[:expected]
			kind := tc.questionKind
			if kind == "" {
				kind = "candy"
			}
			for i, sample := range tc.samples {
				route := "openai:account"
				if sample.routeMissing {
					route = ""
				}
				cfg := &service.PelicanTestConfig{QuestionKind: kind, ParallelCount: 1, ModelIDs: models, TestRoute: route}
				id := f.sample(plan, "reason-round", models[i], sample.status, sample.verdict, sample.message, sample.action, cfg)
				_, err := integrationDB.Exec(`UPDATE scheduled_test_results SET response_text='PRIVATE_ANSWER',
				 quality_judgment=quality_judgment || '{"reason":"PRIVATE_JUDGE_IDENTITY_TOKEN"}'::jsonb,
				 pelican_config=pelican_config || '{"private_token":"PRIVATE_CONFIG_TOKEN"}'::jsonb WHERE id=$1`, id)
				require.NoError(t, err)
			}
			page := f.overview(service.QualityOverviewFilter{})
			require.Len(t, page.Items, 1)
			item := page.Items[0]
			require.Equal(t, "unknown", item.Status)
			require.Nil(t, item.HealthyRate)
			require.Zero(t, item.EvaluatedRounds)
			require.Equal(t, 1, item.TotalRounds)
			require.Len(t, item.History, 1)
			require.Equal(t, tc.want, item.History[0].ReasonCodes)
			if tc.name == "earlier stale latest observed" {
				require.Equal(t, "observed", item.History[0].QualityAction)
			}
			raw, err := json.Marshal(page)
			require.NoError(t, err)
			require.NotContains(t, string(raw), "PRIVATE")
			require.NotContains(t, string(raw), "error_message")
			require.NotContains(t, string(raw), "quality_judgment")
			require.NotContains(t, string(raw), "pelican_config")
		})
	}
}
