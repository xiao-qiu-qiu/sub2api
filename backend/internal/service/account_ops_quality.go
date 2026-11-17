package service

import (
	"fmt"
	"html"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Capture only completed wrong answers. Shared random questions additionally
// require a passing peer before their original verdict can become an alert.
func setQualityDegradationEvidence(plan *ScheduledTestPlan, results []*ScheduledTestResult) {
	plan.QualityDegradation = nil
	if plan.PelicanConfig == nil || plan.PelicanConfig.Quality == nil {
		return
	}
	now := time.Now()
	var finished time.Time
	models := make([]string, 0)
	for _, result := range results {
		if result == nil || result.Status != "failed" || result.QualityJudgment == nil || result.QualityJudgment.Verdict != "incorrect" ||
			(result.ErrorMessage != "answer_mismatch" && result.ErrorMessage != openAICodexStateDegradedError) ||
			result.StartedAt.IsZero() || result.FinishedAt.IsZero() || result.FinishedAt.Before(result.StartedAt) || result.FinishedAt.After(now) {
			continue
		}
		if plan.PelicanConfig.QuestionKind == QualityRandomQuestionKind {
			cfg := result.PelicanConfig
			if !plan.QualityBatchEvaluated || cfg == nil || cfg.QualityBatch == nil || cfg.QualityBatch.Decision != "peer_passed" {
				continue
			}
		}
		if result.FinishedAt.After(finished) {
			finished = result.FinishedAt
		}
		model := plan.ModelID
		if result.PelicanConfig != nil && result.PelicanConfig.ModelID != "" {
			model = result.PelicanConfig.ModelID
		}
		model = compactQualityNotificationLabel(model, 100)
		if model != "" && !slices.Contains(models, model) {
			models = append(models, model)
		}
	}
	if finished.IsZero() {
		return
	}
	slices.Sort(models)
	source := "answer"
	if isOpenAICodexStateProbePlan(plan.PelicanConfig) {
		source = "state_probe"
	}
	plan.QualityDegradation = &AccountOpsDetails{
		QualityModel:    compactQualityNotificationLabel(strings.Join(models, ", "), 2000),
		QualityRuleName: compactQualityNotificationLabel(plan.PelicanConfig.RuleName, 120),
		QualitySource:   source, ObservedAt: &finished,
	}
}

// QualityDegradationEvent is consumed inside the accepted outcome transaction,
// so stale runs and rolled back account changes never publish notifications.
func QualityDegradationEvent(plan *ScheduledTestPlan, outcome string) *AccountOpsEvent {
	if plan == nil || plan.ID <= 0 || plan.AccountID <= 0 || plan.QualityDegradation == nil ||
		plan.PelicanConfig == nil || plan.PelicanConfig.Quality == nil ||
		(plan.PelicanConfig.QuestionKind == QualityRandomQuestionKind && !plan.QualityBatchEvaluated) {
		return nil
	}
	if outcome != "failed" && !(outcome == "answer_29" && plan.PelicanConfig.Quality.Action == QualityActionOpenAITwoFAAdmission) {
		return nil
	}
	details := *plan.QualityDegradation
	if details.ObservedAt == nil || details.ObservedAt.IsZero() || details.ObservedAt.After(time.Now()) {
		return nil
	}
	return &AccountOpsEvent{
		AccountID: plan.AccountID, AccountName: compactQualityNotificationLabel(plan.AccountName, 120),
		GroupID: "account:" + strconv.FormatInt(plan.AccountID, 10), Kind: "quality_degraded",
		Signal: details.QualitySource, Details: &details,
		FirstSeen: *details.ObservedAt, LastSeen: *details.ObservedAt, Occurrences: 1,
	}
}

func compactQualityNotificationLabel(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	text := []rune(value)
	if len(text) > limit {
		return string(text[:limit]) + "…"
	}
	return value
}

func (s *AccountOpsService) qualityDegradationMessage(e *AccountOpsEvent) accountOpsMessage {
	title := "Sub2API · 账号降智"
	account := compactQualityNotificationLabel(e.AccountName, 120)
	if account == "" {
		account = "未命名账号"
	}
	account = fmt.Sprintf("%s (#%d)", account, e.AccountID)
	model, rule, source := "未记录", "未命名规则", "答题检测"
	observed := e.LastSeen
	if d := e.Details; d != nil {
		if d.QualityModel != "" {
			model = compactQualityNotificationLabel(d.QualityModel, 2000)
		}
		if d.QualityRuleName != "" {
			rule = compactQualityNotificationLabel(d.QualityRuleName, 120)
		}
		if d.QualitySource == "state_probe" {
			source = "状态探针"
		}
		if d.ObservedAt != nil {
			observed = *d.ObservedAt
		}
	}
	stamp := observed.In(s.timezone).Format("2006-01-02 15:04:05")
	fields := [][2]string{{"账号", account}, {"模型", model}, {"规则", rule}, {"检测方式", source}, {"时间", stamp}}
	plain := make([]string, 0, len(fields))
	markdown := "### " + title
	body := "<h2>" + title + "</h2><table style=\"border-collapse:collapse\">"
	for _, field := range fields {
		plain = append(plain, field[0]+"："+field[1])
		markdown += "\n**" + field[0] + "**：" + escapeOpsMarkdown(field[1])
		body += "<tr><th style=\"text-align:left;padding:8px\">" + field[0] + "</th><td style=\"padding:8px\">" + html.EscapeString(field[1]) + "</td></tr>"
	}
	body += "</table>"
	return accountOpsMessage{title: title, robotTitle: title, plain: strings.Join(plain, "\n"), markdown: markdown, html: body,
		color: "red", account: account, balance: "不适用", threshold: "不适用", observedTime: stamp}
}
