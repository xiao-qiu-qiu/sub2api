//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestQualityExactHTTPAnswerCompletionAndRequestPrivacy(t *testing.T) {
	for _, tc := range []struct {
		name, answer, stop, verdict string
		status                      int
		complete                    bool
	}{
		{"correct", "２１．０", "end_turn", "correct", 200, true},
		{"wrong", "29", "end_turn", "incorrect", 200, true},
		{"prose", "I reasoned through the question and got 29.", "end_turn", "unknown", 200, true},
		{"max tokens", "29", "max_tokens", "unknown", 200, true},
		{"broken stream", "29", "end_turn", "unknown", 200, false},
		{"empty", "", "end_turn", "unknown", 200, true},
		{"HTTP failure", "29", "end_turn", "unknown", 400, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"fixture\",\"usage\":{\"input_tokens\":10}}}\n\n" +
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":" + strconv.Quote(tc.answer) + "}}\n\n"
			if tc.complete {
				body += "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":" + strconv.Quote(tc.stop) + "},\"usage\":{\"output_tokens\":2}}\n\n" +
					"data: {\"type\":\"message_stop\"}\n\n"
			}
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: tc.status,
				Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}}
			account := pelicanClaudeOAuthAccount()
			tests := pelicanClaudeTestService(account, upstream)
			cfg := exactQualityConfig("21")
			cfg.Prompt = "Compute 20 + 1."
			runner := &ScheduledTestRunnerService{runPelican: tests.RunPelicanBackground,
				judgeQuality: func(context.Context, int64, *PelicanTestConfig, string) *QualityJudgment {
					t.Error("exact must not invoke an external judge")
					return nil
				}}
			result := runner.runPelicanSample(t.Context(), &ScheduledTestPlan{AccountID: account.ID, ModelID: "claude-opus-5-5", PelicanConfig: cfg})
			require.NotNil(t, result.QualityJudgment)
			require.Equal(t, tc.verdict, result.QualityJudgment.Verdict)
			require.Equal(t, "exact", result.QualityJudgment.Kind)
			require.Len(t, upstream.requests, 1, "only the answer request may be sent")
			require.Equal(t, cfg.Prompt+"\n\n"+qualityExactAnswerContract, gjson.GetBytes(upstream.lastBody, "messages.0.content.0.text").String())
			require.NotContains(t, string(upstream.lastBody), "expected_answer")
			require.NotContains(t, string(upstream.lastBody), "reference_answer")
			if tc.verdict == "unknown" {
				require.Equal(t, "inconclusive", qualityOutcome([]*ScheduledTestResult{result}))
				require.NotEqual(t, "answer_mismatch", result.ErrorMessage)
			}
		})
	}
}
