package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func responsesTestWire(events ...string) string {
	return "data: " + strings.Join(events, "\n\ndata: ") + "\n\n"
}

func responsesTestMessage(id, text string) string {
	return fmt.Sprintf(`{"type":"message","id":%q,"role":"assistant","status":"completed","content":[{"type":"output_text","text":%q}]}`, id, text)
}

func responsesTestFinal(items ...string) string {
	return `{"type":"response.completed","response":{"id":"resp_test","status":"completed","output":[` + strings.Join(items, ",") + `]}}`
}

func responsesTestDelta(output, content int, id, text string) string {
	return fmt.Sprintf(`{"type":"response.output_text.delta","output_index":%d,"content_index":%d,"item_id":%q,"delta":%q}`, output, content, id, text)
}

func responsesTestDone(output, content int, id, text string) string {
	return fmt.Sprintf(`{"type":"response.output_text.done","output_index":%d,"content_index":%d,"item_id":%q,"text":%q}`, output, content, id, text)
}

func TestResponsesGatewayAccountTestAnswer(t *testing.T) {
	final21 := responsesTestFinal(responsesTestMessage("msg_0", "21个。"))
	delta21 := responsesTestDelta(0, 0, "msg_0", "21个。")
	done21 := responsesTestDone(0, 0, "msg_0", "21个。")
	for _, tc := range []struct {
		name   string
		events []string
		want   string
		err    string
	}{
		{"fragmented 21", []string{responsesTestDelta(0, 0, "msg_0", "2"), responsesTestDelta(0, 0, "msg_0", "1"), responsesTestDelta(0, 0, "msg_0", "个。"), done21, final21}, "21个。", ""},
		{"fragmented 29", []string{responsesTestDelta(0, 0, "msg_0", "2"), responsesTestDelta(0, 0, "msg_0", "9"), responsesTestFinal(responsesTestMessage("msg_0", "29"))}, "29", ""},
		{"consistent incorrect answer remains judgeable", []string{responsesTestDelta(0, 0, "msg_0", "23"), responsesTestFinal(responsesTestMessage("msg_0", "23"))}, "23", ""},
		{"delta 23 final 21", []string{responsesTestDelta(0, 0, "msg_0", "23个。"), done21, final21}, "", "inconsistent"},
		{"delta 27 final 21 without done", []string{responsesTestDelta(0, 0, "msg_0", "27个。"), final21}, "", "inconsistent"},
		{"done conflicts with matching delta and final", []string{delta21, responsesTestDone(0, 0, "msg_0", "29"), final21}, "", "inconsistent"},
		{"done only conflicts with final", []string{responsesTestDone(0, 0, "msg_0", "29"), final21}, "", "inconsistent"},
		{"final only", []string{final21}, "21个。", ""},
		{"done only with final", []string{done21, final21}, "21个。", ""},
		{"delta without done with final", []string{delta21, final21}, "21个。", ""},
		{"missing terminal", []string{delta21, done21}, "", "before completion"},
		{"done sentinel is not completion", []string{delta21, done21, "[DONE]"}, "", "before completion"},
		{"missing final output", []string{delta21, `{"type":"response.completed","response":{"status":"completed"}}`}, "", "no completed final output"},
		{"delta absent from final", []string{delta21, responsesTestFinal()}, "", "text indices"},
		{"empty final", []string{responsesTestFinal()}, "", "empty output"},
		{"duplicate delta", []string{delta21, delta21, final21}, "", "inconsistent"},
		{"mixed items", []string{responsesTestDelta(0, 0, "msg_0", "2"), responsesTestDelta(1, 0, "msg_1", "3个。"), final21}, "", "text indices"},
		{"wrong item identity", []string{responsesTestDelta(0, 0, "other", "21个。"), final21}, "", "identities"},
		{"item changes identity", []string{`{"type":"response.output_item.added","output_index":0,"item":{"id":"other","type":"message"}}`, delta21, final21}, "", "identities"},
		{"identity reused across final messages", []string{responsesTestFinal(responsesTestMessage("same", "2"), responsesTestMessage("same", "1"))}, "", "reuses"},
		{"multiple messages interleaved", []string{responsesTestDelta(1, 0, "msg_1", "1"), responsesTestDelta(0, 0, "msg_0", "2"), responsesTestFinal(responsesTestMessage("msg_0", "2"), responsesTestMessage("msg_1", "1"))}, "21", ""},
		{"multiple content parts interleaved", []string{responsesTestDelta(0, 1, "msg_0", "1"), responsesTestDelta(0, 0, "msg_0", "2"), responsesTestFinal(`{"id":"msg_0","type":"message","role":"assistant","content":[{"type":"output_text","text":"2"},{"type":"output_text","text":"1"}]}`)}, "21", ""},
		{"reasoning item before message", []string{responsesTestDelta(1, 0, "msg_0", "21个。"), responsesTestFinal(`{"type":"reasoning","id":"rs_0"}`, responsesTestMessage("msg_0", "21个。"))}, "21个。", ""},
		{"legacy unindexed single text", []string{`{"type":"response.output_text.delta","delta":"21个。"}`, final21}, "21个。", ""},
		{"unindexed text ambiguous across messages", []string{`{"type":"response.output_text.delta","delta":"21"}`, responsesTestFinal(responsesTestMessage("msg_0", "2"), responsesTestMessage("msg_1", "1"))}, "", "ambiguous"},
		{"item id resolves omitted index", []string{`{"type":"response.output_text.delta","item_id":"msg_1","content_index":0,"delta":"1"}`, responsesTestFinal(responsesTestMessage("msg_0", "2"), responsesTestMessage("msg_1", "1"))}, "21", ""},
		{"explicit identity resolves later unindexed done", []string{delta21, `{"type":"response.output_text.done","item_id":"msg_0","text":"21个。"}`, responsesTestFinal(responsesTestMessage("", "21个。"))}, "21个。", ""},
		{"unknown id without index", []string{`{"type":"response.output_text.delta","item_id":"other","delta":"21个。"}`, final21}, "", "unrecognized"},
		{"wrong content index", []string{responsesTestDelta(0, 1, "msg_0", "21个。"), final21}, "", "text indices"},
		{"negative index", []string{responsesTestDelta(-1, 0, "msg_0", "21个。"), final21}, "", "invalid output index"},
		{"fractional index", []string{`{"type":"response.output_text.delta","output_index":0.5,"delta":"21个。"}`, final21}, "", "invalid SSE"},
		{"missing delta string", []string{`{"type":"response.output_text.delta","output_index":0}`, final21}, "", "without text"},
		{"malformed event", []string{`{"type":"response.output_text.delta",`, final21}, "", "invalid SSE"},
		{"malformed final text", []string{responsesTestFinal(`{"type":"message","content":[{"type":"output_text"}]}`)}, "", "without text"},
		{"unfinished final response", []string{`{"type":"response.completed","response":{"status":"incomplete","output":[]}}`}, "", "no completed final output"},
		{"unfinished final message", []string{responsesTestFinal(`{"type":"message","status":"incomplete","content":[{"type":"output_text","text":"21"}]}`)}, "", "unfinished"},
		{"non assistant final message", []string{responsesTestFinal(`{"type":"message","role":"user","content":[{"type":"output_text","text":"21"}]}`)}, "", "non-assistant"},
		{"failed event", []string{delta21, `{"type":"response.failed"}`}, "", "response.failed"},
		{"incomplete event", []string{delta21, `{"type":"response.incomplete"}`}, "", "response.incomplete"},
		{"mismatched response identity", []string{`{"type":"response.created","response":{"id":"other"}}`, final21}, "", "response identities"},
		{"delta after text done", []string{done21, delta21, final21}, "", "after text completion"},
		{"duplicate text done", []string{done21, done21, final21}, "", "repeats"},
		{"event after terminal", []string{final21, delta21}, "", "after completion"},
		{"duplicate terminal", []string{final21, final21}, "", "after completion"},
		{"event after sentinel", []string{final21, "[DONE]", delta21}, "", "after the stream terminator"},
		{"sentinel after terminal", []string{delta21, final21, "[DONE]"}, "21个。", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := responsesGatewayAccountTestAnswer(responsesTestWire(tc.events...))
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
				require.Empty(t, got, "unverified text must not be judged as an answer")
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.want, got)
			}
		})
	}
}

func TestResponsesGatewayAccountTestAnswerSSEFraming(t *testing.T) {
	wire := ": keepalive\r\nevent: response.completed\r\ndata:{\r\ndata: \"type\":\"response.completed\",\r\ndata: \"response\":{\"status\":\"completed\",\"output\":[" + responsesTestMessage("msg_0", "21") + "]}}"
	got, err := responsesGatewayAccountTestAnswer(wire)
	require.NoError(t, err)
	require.Equal(t, "21", got)
}

func responsesAccountTestHTTP(wire string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}
}

func TestExcelBPSPelicanReasoningEffort(t *testing.T) {
	for _, tc := range []struct {
		name, effort, want string
		pelican            bool
	}{
		{"ordinary default", "", "medium", false},
		{"pelican default", "", "medium", true},
		{"pelican low", "low", "low", true},
		{"pelican high", "high", "high", true},
		{"pelican xhigh", "xhigh", "xhigh", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := excelAccount()
			upstream := &httpUpstreamRecorder{resp: responsesAccountTestHTTP(responsesTestWire(responsesTestFinal(responsesTestMessage("msg_0", "21"))))}
			svc := &AccountTestService{accountRepo: &stateProbeAccountRepo{account: account}, openaiGatewayService: openAIClientToolsTestService(upstream)}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)
			var err error
			if tc.pelican {
				err = svc.TestPelicanAccountConnection(c, account.ID, "gpt-6.1-sol", "Reply 21", tc.effort)
			} else {
				err = svc.TestAccountConnection(c, account.ID, "gpt-6.1-sol", "Reply 21", AccountTestModeDefault)
			}
			require.NoError(t, err)
			require.Len(t, upstream.requests, 1)
			require.Equal(t, tc.want, gjson.GetBytes(upstream.lastBody, "reasoning_effort").String())
			answer, message := parsePelicanOutput(recorder.Body.String())
			require.Empty(t, message)
			require.Equal(t, "21", answer)
		})
	}
}

func TestExcelBPSBackgroundRejectsInconsistentAnswers(t *testing.T) {
	for _, tc := range []struct {
		name, delta, final, want, status string
	}{
		{"consistent answer", "21个。", "21个。", "21个。", "success"},
		{"incorrect provisional answer", "23个。", "21个。", "", "failed"},
		{"incorrect final answer", "21个。", "27个。", "", "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := responsesTestWire(responsesTestDelta(0, 0, "msg_0", tc.delta), responsesTestFinal(responsesTestMessage("msg_0", tc.final)))
			upstream := &httpUpstreamRecorder{resp: responsesAccountTestHTTP(wire)}
			account := excelAccount()
			svc := &AccountTestService{accountRepo: &stateProbeAccountRepo{account: account}, openaiGatewayService: openAIClientToolsTestService(upstream)}
			result, err := svc.RunPelicanBackground(t.Context(), account.ID, "gpt-6.1-sol", &PelicanTestConfig{QuestionKind: "candy", Prompt: CandyPrompt, ReasoningEffort: "high", ParallelCount: 1})
			require.NoError(t, err)
			require.Equal(t, tc.status, result.Status)
			require.Equal(t, tc.want, result.ResponseText)
			if tc.status == "failed" {
				require.Contains(t, result.ErrorMessage, "inconsistent")
			}
		})
	}
}

func responsesTestRefusalCases() []struct{ name, wire string } {
	const refusalPart = `{"type":"refusal","refusal":"Cannot answer"}`
	refusalMessage := `{"type":"message","id":"msg_refusal","role":"assistant","content":[` + refusalPart + `]}`
	mixedMessage := `{"type":"message","id":"msg_0","role":"assistant","content":[{"type":"output_text","text":"29"},` + refusalPart + `]}`
	final29 := responsesTestFinal(responsesTestMessage("msg_0", "29"))
	return []struct{ name, wire string }{
		{"pure final refusal", responsesTestWire(responsesTestFinal(refusalMessage))},
		{"text and refusal in one final message", responsesTestWire(responsesTestFinal(mixedMessage))},
		{"refusal before text in one final message", responsesTestWire(responsesTestFinal(`{"type":"message","content":[` + refusalPart + `,{"type":"output_text","text":"29"}]}`))},
		{"refusal in later final message", responsesTestWire(responsesTestFinal(responsesTestMessage("msg_0", "29"), refusalMessage))},
		{"refusal in earlier final message", responsesTestWire(responsesTestFinal(refusalMessage, responsesTestMessage("msg_0", "29")))},
		{"delta text with mixed final refusal", responsesTestWire(responsesTestDelta(0, 0, "msg_0", "29"), responsesTestFinal(mixedMessage))},
		{"refusal delta despite text final", responsesTestWire(responsesTestDelta(0, 0, "msg_0", "29"), `{"type":"response.refusal.delta","output_index":0,"content_index":1,"delta":"Cannot answer"}`, final29)},
		{"refusal done despite text final", responsesTestWire(`{"type":"response.refusal.done","refusal":"Cannot answer"}`, final29)},
		{"refusal without final", responsesTestWire(`{"type":"response.refusal.delta","delta":"Cannot answer"}`)},
		{"refusal content part added", responsesTestWire(`{"type":"response.content_part.added","part":`+refusalPart+`}`, final29)},
		{"refusal content part done", responsesTestWire(`{"type":"response.content_part.done","part":`+refusalPart+`}`, final29)},
		{"refusal item added", responsesTestWire(`{"type":"response.output_item.added","output_index":0,"item":`+refusalMessage+`}`, final29)},
		{"refusal item done", responsesTestWire(`{"type":"response.output_item.done","output_index":0,"item":`+refusalMessage+`}`, final29)},
	}
}

func TestResponsesGatewayAccountTestAnswerRefusals(t *testing.T) {
	for _, tc := range responsesTestRefusalCases() {
		t.Run(tc.name, func(t *testing.T) {
			answer, err := responsesGatewayAccountTestAnswer(tc.wire)
			require.ErrorIs(t, err, errResponsesTestRefused)
			require.ErrorContains(t, err, pelicanErrRefused)
			require.ErrorContains(t, err, "refusal")
			require.Empty(t, answer)
		})
	}
}

func TestExcelBPSPelicanRefusalCannotFailQuality(t *testing.T) {
	for _, tc := range responsesTestRefusalCases() {
		for _, exact := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/exact=%t", tc.name, exact), func(t *testing.T) {
				account := excelAccount()
				upstream := &httpUpstreamRecorder{resp: responsesAccountTestHTTP(tc.wire)}
				svc := &AccountTestService{accountRepo: &stateProbeAccountRepo{account: account}, openaiGatewayService: openAIClientToolsTestService(upstream)}
				judgeCalled := false
				runner := &ScheduledTestRunnerService{
					runPelican: svc.RunPelicanBackground,
					judgeQuality: func(context.Context, int64, *PelicanTestConfig, string) *QualityJudgment {
						judgeCalled = true
						return &QualityJudgment{Verdict: "incorrect"}
					},
				}
				cfg := &PelicanTestConfig{QuestionKind: "candy", Prompt: CandyPrompt, ReasoningEffort: "high", ParallelCount: 1,
					Quality: &QualityPolicy{Action: QualityActionMoveGroups, ExpectedAnswer: "21", MoveGroupIDs: []int64{99}}}
				if exact {
					cfg.Quality.Judge = &QualityJudgeConfig{Kind: "exact"}
				}
				result := runner.runPelicanSample(t.Context(), &ScheduledTestPlan{AccountID: account.ID, ModelID: "gpt-6.1-sol", TriggerSource: quality5xxSource, PelicanConfig: cfg})
				require.Equal(t, "failed", result.Status)
				require.Empty(t, result.ResponseText)
				require.Contains(t, result.ErrorMessage, pelicanErrRefused)
				require.NotEqual(t, "answer_mismatch", result.ErrorMessage)
				require.False(t, judgeCalled)
				if exact {
					require.NotNil(t, result.QualityJudgment)
					require.Equal(t, "unknown", result.QualityJudgment.Verdict)
				} else {
					require.Nil(t, result.QualityJudgment)
				}
				require.Equal(t, "inconclusive", qualityRoundOutcome([]*ScheduledTestResult{result}), "refusal must not trigger failure-based group moves")
			})
		}
	}
}
