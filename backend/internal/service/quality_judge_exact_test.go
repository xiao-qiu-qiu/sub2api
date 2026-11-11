package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const qualityExactTestFence = "\x60\x60\x60"

func exactQualityConfig(expected string) *PelicanTestConfig {
	return &PelicanTestConfig{QuestionKind: "candy", Prompt: CandyPrompt, ParallelCount: 1, ReasoningEffort: "medium",
		Quality: &QualityPolicy{ExpectedAnswer: expected, Action: "disable_scheduling", Judge: &QualityJudgeConfig{Kind: "exact"}}}
}

func TestQualityExactNumericEquivalenceAndClearMismatch(t *testing.T) {
	for _, tc := range []struct{ name, expected, answer, verdict string }{
		{"integer", "21", "21", "correct"},
		{"decimal", "21", "21.0", "correct"},
		{"fullwidth", "21", "　２１．００　", "correct"},
		{"fullwidth reference", "２１．０", "21", "correct"},
		{"whitespace", "21", " \t21\r\n", "correct"},
		{"signed", "21", "+0021.000", "correct"},
		{"leading zeroes", "21", "00021", "correct"},
		{"decimal not octal", "8", "008", "correct"},
		{"scientific", "21", "2.1e1", "correct"},
		{"decimal point", "21", "21.", "correct"},
		{"fractional decimal", "0.5", ".50", "correct"},
		{"negative", "-21", "－２１．０", "correct"},
		{"zero", "0", "-0.000", "correct"},
		{"precision", "9007199254740993", "9007199254740993.0", "correct"},
		{"precision mismatch", "9007199254740993", "9007199254740992", "incorrect"},
		{"wrong integer", "21", "29", "incorrect"},
		{"wrong decimal", "21", "21.0001", "incorrect"},
		{"fenced", "21", qualityExactTestFence + "json\n21.00\n" + qualityExactTestFence, "correct"},
		{"fenced CRLF", "21", " " + qualityExactTestFence + "text\r\n２１\r\n" + qualityExactTestFence + " ", "correct"},
		{"fenced wrong", "21", qualityExactTestFence + "\n29\n" + qualityExactTestFence, "incorrect"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := (&QualityJudgeService{}).Judge(t.Context(), 1, exactQualityConfig(tc.expected), tc.answer)
			require.Equal(t, tc.verdict, got.Verdict)
			require.Equal(t, "exact", got.Kind)
			require.Equal(t, "1", got.RulesVersion)
			require.Zero(t, got.AccountID)
			require.Zero(t, got.GroupID)
			require.Empty(t, got.ModelID)
		})
	}
}

func TestQualityExactAmbiguousAnswersStayUnknown(t *testing.T) {
	for _, answer := range []string{
		"", " \t\n", "The answer is 29", "答案是29", "29个", "21 29", "21\n29", "21\n因此答案是29",
		"<think>21</think>29", "<html>29</html>", "**29**", "21.\u000021", "NaN", "Infinity", "1/0", "0x15",
		"我先进行分析。" + strings.Repeat("推理", 1000) + "最后答案为29", "null", "true", "\"29\"",
		"{\"answer\":29}", "[29]", "1e1000000000", "1e-1000000000", strings.Repeat("9", 64001),
		qualityExactTestFence + "\n29", qualityExactTestFence + "\n29\n" + qualityExactTestFence + " trailing",
		"prefix " + qualityExactTestFence + "\n29\n" + qualityExactTestFence,
		qualityExactTestFence + "\n21\n" + qualityExactTestFence + "\n" + qualityExactTestFence + "\n29\n" + qualityExactTestFence,
		string([]byte{0xff}),
	} {
		t.Run(answer[:min(len(answer), 45)], func(t *testing.T) {
			got := judgeQualityExact(exactQualityConfig("21"), answer)
			require.Equal(t, "unknown", got.Verdict)
			result := &ScheduledTestResult{}
			applyQualityJudgment(result, got)
			require.Equal(t, "judge_inconclusive", result.ErrorMessage)
			require.Equal(t, "inconclusive", qualityOutcome([]*ScheduledTestResult{result}))
		})
	}
}

func TestQualityExactStructuredAndTextAnswers(t *testing.T) {
	for _, tc := range []struct{ expected, answer, verdict string }{
		{"{\"count\":21,\"first\":\"RRUU\"}", "{\"first\":\"RRUU\",\"count\":21.0}", "correct"},
		{"{\"count\":21,\"first\":\"RRUU\"}", "{\"count\":29,\"first\":\"RRUU\"}", "incorrect"},
		{"{\"count\":21,\"first\":\"RRUU\"}", "{\"count\":29}", "unknown"},
		{"{\"count\":21}", "{\"count\":29,\"extra\":1}", "unknown"},
		{"{\"count\":21}", "{\"count\":21,\"count\":29}", "unknown"},
		{"{\"count\":21}", "{\"count\":29", "unknown"},
		{"{\"count\":21}", "{\"count\":29} trailing", "unknown"},
		{"{\"count\":21}", "{\"count\":null}", "unknown"},
		{"{\"count\":21}", "{\"count\":\"29\"}", "unknown"},
		{"[21,{\"x\":true}]", "[21.00,{\"x\":true}]", "correct"},
		{"[21,{\"x\":true}]", "[21,{\"x\":false}]", "incorrect"},
		{"[21,29]", "[21]", "unknown"},
		{"\"北京\"", "\"北京\"", "correct"},
		{"北京", " 北京 ", "correct"},
		{"北京", "\"上海\"", "incorrect"},
		{"北京", "我推测答案为上海", "unknown"},
		{"true", "false", "incorrect"},
		{"[]", "[]", "correct"},
		{"{}", "{}", "correct"},
	} {
		t.Run(tc.expected+"/"+tc.answer, func(t *testing.T) {
			require.Equal(t, tc.verdict, judgeQualityExact(exactQualityConfig(tc.expected), tc.answer).Verdict)
		})
	}
	deep := strings.Repeat("[", 34) + "21" + strings.Repeat("]", 34)
	require.Equal(t, "unknown", judgeQualityExact(exactQualityConfig("21"), deep).Verdict)
}

func TestQualityExactNeverUsesAnyJudgeNetwork(t *testing.T) {
	for _, answer := range []string{"21", "29", "reasoning then 29", ""} {
		t.Run(answer, func(t *testing.T) {
			svc, cfg, _, _ := qualityJevFixture(t, func(_ *http.Request) (*http.Response, error) {
				t.Fatal("exact must never invoke Jev")
				return nil, nil
			})
			cfg.Quality.Judge = &QualityJudgeConfig{Kind: "exact"}
			svc.request = func(context.Context, int64, string, string) (string, error) {
				t.Fatal("exact must never invoke an LLM judge")
				return "", nil
			}
			// Account/group/slot dependencies remain nil; touching any is a failure.
			require.Equal(t, "exact", svc.Judge(t.Context(), 42, cfg, answer).Kind)
		})
	}
}

func TestQualityExactValidationAndLegacyJudgeSemantics(t *testing.T) {
	plan := pelicanPlan()
	plan.PelicanConfig = exactQualityConfig("21")
	_, err := nextPlanRun(plan, time.Now())
	require.NoError(t, err, "exact requires no group, model or grading prompt")
	for _, change := range []func(*PelicanTestConfig){
		func(cfg *PelicanTestConfig) { cfg.Quality.ExpectedAnswer = " " },
		func(cfg *PelicanTestConfig) { cfg.Quality.ExpectedAnswer = strings.Repeat("x", 4001) },
		func(cfg *PelicanTestConfig) { cfg.Quality.Judge.GroupID = 1 },
		func(cfg *PelicanTestConfig) { cfg.Quality.Judge.ModelID = "unused-model" },
		func(cfg *PelicanTestConfig) { cfg.Quality.Judge.Prompt = "unused-rubric" },
	} {
		plan.PelicanConfig = exactQualityConfig("21")
		change(plan.PelicanConfig)
		_, err := nextPlanRun(plan, time.Now())
		require.Error(t, err)
		require.Equal(t, "unknown", judgeQualityExact(plan.PelicanConfig, "21").Verdict)
	}
	for _, cfg := range []*PelicanTestConfig{nil, {}, {Quality: &QualityPolicy{}}} {
		require.Equal(t, "judge_not_configured", (&QualityJudgeService{}).Judge(t.Context(), 1, cfg, "21").Reason)
	}
	for _, kind := range []string{"", "llm", "jev"} {
		plan.PelicanConfig = exactQualityConfig("21")
		judge := &QualityJudgeConfig{Kind: kind, Prompt: "compare"}
		if kind != "jev" {
			judge.GroupID, judge.ModelID = 9, "selected-judge"
		}
		plan.PelicanConfig.Quality.Judge = judge
		_, err := nextPlanRun(plan, time.Now())
		require.NoError(t, err)
		require.Equal(t, kind, judge.Kind, "validation must not migrate saved judges")
		judge.Prompt = ""
		_, err = nextPlanRun(plan, time.Now())
		require.Error(t, err)
	}
	plan.PelicanConfig = exactQualityConfig("21")
	plan.PelicanConfig.Quality.Judge = nil
	_, err = nextPlanRun(plan, time.Now())
	require.NoError(t, err)
	require.Nil(t, plan.PelicanConfig.Quality.Judge)
}

func TestQualityExactPromptNeverContainsReferenceOrJudgeSettings(t *testing.T) {
	cfg := exactQualityConfig("private-reference-not-in-question")
	cfg.Quality.Judge.Prompt = "private-judge-rubric"
	prompt := intelligenceTestPrompt(cfg)
	require.Equal(t, cfg.Prompt+"\n\n"+qualityExactAnswerContract, prompt)
	require.NotContains(t, prompt, cfg.Quality.ExpectedAnswer)
	require.NotContains(t, prompt, cfg.Quality.Judge.Prompt)
	before := prompt
	cfg.Quality.ExpectedAnswer = "a-different-private-reference"
	require.Equal(t, before, intelligenceTestPrompt(cfg), "reference must not affect test payload")
	for _, model := range []string{"gpt-6-astra", "gpt-6.1-sol", "claude-opus-5-5", "gemini-3-pro"} {
		var payload any
		if strings.HasPrefix(model, "claude-") {
			var err error
			payload, err = createPelicanClaudePayload(model, prompt, "medium")
			require.NoError(t, err)
		} else if strings.HasPrefix(model, "gemini-") {
			payload = json.RawMessage(createGeminiTestPayload(model, prompt))
		} else {
			payload = createPelicanOpenAIPayload(model, false, prompt, "medium")
		}
		body, err := json.Marshal(payload)
		require.NoError(t, err)
		require.NotContains(t, string(body), cfg.Quality.ExpectedAnswer)
		require.NotContains(t, string(body), cfg.Quality.Judge.Prompt)
		require.Contains(t, string(body), qualityExactAnswerContract)
	}
}

func TestQualityExactRunnerKeepsFailedAndTruncatedAnswersUnknown(t *testing.T) {
	for _, tc := range []struct {
		status, answer, message string
		err                     error
	}{
		{"failed", "29", "Generation stream ended before completion", nil},
		{"failed", "21", pelicanErrMaxTokens, nil},
		{"failed", "29", "API returned 400: bad request", nil},
		{"failed", "", "API returned 503: unavailable", nil},
		{"success", "29", "Generation stream ended before completion", nil},
		{"", "", "", errors.New("transport failure")},
	} {
		t.Run(tc.status+tc.message, func(t *testing.T) {
			runner := &ScheduledTestRunnerService{runPelican: func(context.Context, int64, string, *PelicanTestConfig) (*ScheduledTestResult, error) {
				return &ScheduledTestResult{Status: tc.status, ResponseText: tc.answer, ErrorMessage: tc.message}, tc.err
			}}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if strings.Contains(tc.message, "503") {
				run := runner.runPelican
				runner.runPelican = func(ctx context.Context, id int64, model string, cfg *PelicanTestConfig) (*ScheduledTestResult, error) {
					cancel()
					return run(ctx, id, model, cfg)
				}
			}
			result := runner.runPelicanSample(ctx, &ScheduledTestPlan{AccountID: 42, PelicanConfig: exactQualityConfig("21")})
			require.NotNil(t, result.QualityJudgment)
			require.Equal(t, "unknown", result.QualityJudgment.Verdict)
			require.Equal(t, "exact", result.QualityJudgment.Kind)
			require.NotEqual(t, "answer_mismatch", result.ErrorMessage)
			require.Equal(t, "inconclusive", qualityOutcome([]*ScheduledTestResult{result}))
		})
	}
}

func TestQualityExactRunnerGradesEveryModelWithoutJudgeCallback(t *testing.T) {
	for _, tc := range []struct{ answer, outcome string }{{"２１．０", "passed"}, {"29", "failed"}, {"Thinking: 21 or 29", "inconclusive"}} {
		t.Run(tc.outcome, func(t *testing.T) {
			plans, saved := &qualityPlanRepo{}, &pelicanResults{}
			runner := &ScheduledTestRunnerService{planRepo: plans, scheduledSvc: NewScheduledTestService(plans, saved)}
			runner.runPelican = func(_ context.Context, _ int64, model string, _ *PelicanTestConfig) (*ScheduledTestResult, error) {
				answer := "21"
				if model == "gpt-6-sol" {
					answer = tc.answer
				}
				return &ScheduledTestResult{Status: "success", ResponseText: answer, StartedAt: time.Now()}, nil
			}
			runner.judgeQuality = func(context.Context, int64, *PelicanTestConfig, string) *QualityJudgment {
				t.Error("exact runner must not invoke external judge callback")
				return nil
			}
			plan := multiModelQualityPlan(t, "[\"gpt-6-astra\",\"gpt-6-sol\"]")
			plan.PelicanConfig.Quality.Judge = &QualityJudgeConfig{Kind: "exact"}
			require.True(t, runner.runOnePlan(t.Context(), plan))
			require.Equal(t, []string{tc.outcome}, plans.outcomes)
			require.Len(t, saved.results, 2)
			for _, result := range saved.results {
				require.Equal(t, "exact", result.QualityJudgment.Kind)
			}
		})
	}
}

func TestQualityExactRandomQuestionsUseCompleteLocalReferences(t *testing.T) {
	cfg := exactQualityConfig("")
	cfg.QuestionKind, cfg.ParallelCount = QualityRandomQuestionKind, 4
	samples, err := generateQualityRandomSamples(cfg, strings.NewReader("abcdefghABCDEFGH"))
	require.NoError(t, err)
	for _, sample := range samples {
		require.Equal(t, "correct", judgeQualityExact(sample, sample.Quality.ExpectedAnswer).Verdict)
		require.NotContains(t, intelligenceTestPrompt(sample), sample.Quality.ExpectedAnswer)
	}
	require.Empty(t, cfg.Quality.ExpectedAnswer)
}

func TestQualityExactTriggeredCandyKeepsAmbiguousAnswersUnknown(t *testing.T) {
	for _, answer := range []string{"29", "２１．０", "I considered both 21 and 29; the answer is 29."} {
		runner := &ScheduledTestRunnerService{runPelican: func(context.Context, int64, string, *PelicanTestConfig) (*ScheduledTestResult, error) {
			return &ScheduledTestResult{Status: "success", ResponseText: answer, StartedAt: time.Now()}, nil
		}}
		plan := &ScheduledTestPlan{AccountID: 42, TriggerSource: quality5xxSource, PelicanConfig: exactQualityConfig("21")}
		result := runner.runPelicanSample(t.Context(), plan)
		require.Equal(t, "exact", result.QualityJudgment.Kind, "explicit exact must precede legacy candy matching")
		want := "unknown"
		if answer == "29" {
			want = "incorrect"
		}
		if answer == "２１．０" {
			want = "correct"
		}
		require.Equal(t, want, result.QualityJudgment.Verdict)
	}
}
