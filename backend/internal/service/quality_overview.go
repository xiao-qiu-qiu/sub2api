package service

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const QualityOverviewHistoryLimit = 60

type QualityOverviewFilter struct {
	Page        int
	PageSize    int
	Search      string
	GroupID     *int64
	TestChannel string
	ProbeScope  string
}

func (f *QualityOverviewFilter) Normalize() error {
	if f.Page == 0 {
		f.Page = 1
	}
	if f.PageSize == 0 {
		f.PageSize = 20
	}
	if f.Page < 1 || f.Page > 1000000 || f.PageSize < 1 || f.PageSize > 100 {
		return fmt.Errorf("page must be 1-1000000 and page_size must be 1-100")
	}
	f.Search = strings.TrimSpace(f.Search)
	if utf8.RuneCountInString(f.Search) > 100 || strings.ContainsRune(f.Search, '\x00') {
		return fmt.Errorf("search must be at most 100 characters without NUL")
	}
	if f.GroupID != nil && *f.GroupID < 0 {
		return fmt.Errorf("group_id must be non-negative")
	}
	if f.TestChannel == "" {
		f.TestChannel = "account"
	}
	if f.TestChannel != "account" && f.TestChannel != "bps" {
		return fmt.Errorf("test_channel must be account or bps")
	}
	if f.ProbeScope == "" {
		f.ProbeScope = "all"
	}
	if f.ProbeScope != "all" && f.ProbeScope != "main" && f.ProbeScope != "recovery" {
		return fmt.Errorf("probe_scope must be main, recovery or all")
	}
	return nil
}

type QualityOverviewGroup struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type QualityOverviewPlan struct {
	ID                           int64      `json:"id"`
	Enabled                      bool       `json:"enabled"`
	RunningUntil                 *time.Time `json:"running_until"`
	NextRunAt                    *time.Time `json:"next_run_at"`
	Action                       string     `json:"action"`
	ActionOn                     string     `json:"action_on"`
	ModelID                      string     `json:"model_id"`
	ModelIDs                     []string   `json:"model_ids"`
	TestChannel                  string     `json:"test_channel"`
	AutoRestore                  bool       `json:"auto_restore"`
	RecoveryProbeEnabled         bool       `json:"recovery_probe_enabled"`
	RecoveryProbeIntervalMinutes int        `json:"recovery_probe_interval_minutes"`
	RecoveryConfirmationMinutes  int        `json:"recovery_confirmation_minutes"`
}

type QualityOverviewHistory struct {
	ID            int64     `json:"id"`
	PlanID        int64     `json:"plan_id"`
	StartedAt     time.Time `json:"started_at"`
	Status        string    `json:"status"`
	ModelID       string    `json:"model_id"`
	ModelIDs      []string  `json:"model_ids"`
	QuestionKind  string    `json:"question_kind"`
	QualityAction string    `json:"quality_action"`
}

type QualityOverviewItem struct {
	AccountID     int64                    `json:"account_id"`
	AccountName   string                   `json:"account_name"`
	Platform      string                   `json:"platform"`
	Type          string                   `json:"type"`
	Groups        []QualityOverviewGroup   `json:"groups"`
	Status        string                   `json:"status"`
	HealthyRate   *float64                 `json:"healthy_rate"`
	LastCheckedAt *time.Time               `json:"last_checked_at"`
	History       []QualityOverviewHistory `json:"history"`
	Plans         []QualityOverviewPlan    `json:"plans"`
}

type QualityOverviewPage struct {
	Items    []QualityOverviewItem `json:"items"`
	Total    int64                 `json:"total"`
	Page     int                   `json:"page"`
	PageSize int                   `json:"page_size"`
}

// The repository returns only bounded, aggregated evidence, never answers,
// credentials, prompts, judge identities or account Extra.
type QualityOverviewRoundEvidence struct {
	QualityOverviewHistory
	Samples         int  `json:"samples"`
	ExpectedSamples int  `json:"expected_samples"`
	Passed          int  `json:"passed"`
	Failed          int  `json:"failed"`
	Skipped         int  `json:"skipped"`
	Complete        bool `json:"complete"`
}

type QualityOverviewAccountEvidence struct {
	AccountID int64                          `json:"account_id"`
	Name      string                         `json:"name"`
	Platform  string                         `json:"platform"`
	Type      string                         `json:"type"`
	Groups    []QualityOverviewGroup         `json:"groups"`
	Plans     []QualityOverviewPlan          `json:"plans"`
	Rounds    []QualityOverviewRoundEvidence `json:"rounds"`
}

type QualityOverviewRepository interface {
	ReadQualityOverview(context.Context, QualityOverviewFilter) ([]QualityOverviewAccountEvidence, int64, error)
}

func qualityOverviewRoundStatus(r QualityOverviewRoundEvidence) string {
	if !r.Complete || r.ExpectedSamples < 1 || r.Samples != r.ExpectedSamples {
		return "unknown"
	}
	// A completed wrong answer dominates other inconclusive samples, as in
	// qualityRoundOutcome. Unsupported/skipped models are not passing evidence.
	if r.Failed > 0 {
		return "degraded"
	}
	if r.Passed > 0 && r.Passed+r.Skipped == r.Samples {
		return "healthy"
	}
	return "unknown"
}

// Use an optional read interface on the existing results repository so this
// dashboard needs no changes to quality execution, scheduling or DI providers.
func (s *ScheduledTestService) AccountQualityOverview(ctx context.Context, filter QualityOverviewFilter) (*QualityOverviewPage, error) {
	if err := filter.Normalize(); err != nil {
		return nil, err
	}
	repo, ok := s.resultRepo.(QualityOverviewRepository)
	if !ok {
		return nil, fmt.Errorf("quality overview repository unavailable")
	}
	records, total, err := repo.ReadQualityOverview(ctx, filter)
	if err != nil {
		return nil, err
	}
	page := &QualityOverviewPage{Items: make([]QualityOverviewItem, 0, len(records)), Total: total,
		Page: filter.Page, PageSize: filter.PageSize}
	for _, record := range records {
		item := QualityOverviewItem{AccountID: record.AccountID, AccountName: record.Name, Platform: record.Platform,
			Type: record.Type, Groups: record.Groups, Plans: record.Plans, Status: "untested",
			History: make([]QualityOverviewHistory, 0, len(record.Rounds))}
		if item.Groups == nil {
			item.Groups = []QualityOverviewGroup{}
		}
		if item.Plans == nil {
			item.Plans = []QualityOverviewPlan{}
		}
		for i := range item.Plans {
			if item.Plans[i].ActionOn == "" {
				item.Plans[i].ActionOn = "failed"
			}
			if item.Plans[i].ModelIDs == nil {
				item.Plans[i].ModelIDs = []string{}
			}
		}
		healthy, definite := 0, 0
		for _, round := range record.Rounds {
			history := round.QualityOverviewHistory
			history.Status = qualityOverviewRoundStatus(round)
			if history.ModelIDs == nil {
				history.ModelIDs = []string{}
			}
			item.History = append(item.History, history)
			item.Status, item.LastCheckedAt = history.Status, &item.History[len(item.History)-1].StartedAt
			switch history.Status {
			case "healthy":
				healthy++
				definite++
			case "degraded":
				definite++
			}
		}
		if definite > 0 {
			rate := 100 * float64(healthy) / float64(definite)
			item.HealthyRate = &rate
		}
		page.Items = append(page.Items, item)
	}
	return page, nil
}
