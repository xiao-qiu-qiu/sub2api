package admin

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type qualityHistoryFilterRepo struct {
	service.ScheduledTestResultRepository
	filter        service.QualityHistoryFilter
	cursor        int64
	limit         int
	calls, legacy int
}

func (r *qualityHistoryFilterRepo) ListQualityHistory(_ context.Context, cursor int64, limit int) ([]*service.QualityHistoryResult, error) {
	r.legacy++
	r.cursor = cursor
	r.limit = limit
	return []*service.QualityHistoryResult{}, nil
}
func (r *qualityHistoryFilterRepo) ListFilteredQualityHistory(_ context.Context, cursor int64, limit int, f service.QualityHistoryFilter) ([]*service.QualityHistoryResult, error) {
	r.calls++
	r.cursor = cursor
	r.limit = limit
	r.filter = f
	return []*service.QualityHistoryResult{}, nil
}
func TestQualityHistoryFiltersHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		query  string
		code   int
		filter service.QualityHistoryFilter
		legacy bool
	}{
		{query: "", code: 200},
		{query: "?template_id=3&plan_id=42&before_id=1000", code: 200, filter: service.QualityHistoryFilter{TemplateID: 3, PlanID: 42}},
		{query: "?unassigned=true&plan_id=9", code: 200, filter: service.QualityHistoryFilter{Unassigned: true, PlanID: 9}},
		{query: "?unassigned=false", code: 200},
		{query: "?template_id=0", code: 400}, {query: "?template_id=-1", code: 400},
		{query: "?template_id=", code: 400}, {query: "?plan_id=", code: 400},
		{query: "?template_id=1&template_id=2", code: 400},
		{query: "?plan_id=abc", code: 400}, {query: "?unassigned=oops", code: 400},
		{query: "?unassigned=", code: 400}, {query: "?template_id=3&unassigned=true", code: 400},
	} {
		t.Run(tc.query, func(t *testing.T) {
			repo := &qualityHistoryFilterRepo{}
			h := NewScheduledTestHandler(service.NewScheduledTestService(nil, repo))
			router := gin.New()
			router.GET("/history", h.ListQualityHistory)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest("GET", "/history"+tc.query, nil))
			require.Equal(t, tc.code, w.Code, w.Body.String())
			if tc.code != 200 {
				require.Zero(t, repo.calls+repo.legacy)
				return
			}
			require.Equal(t, tc.filter, repo.filter)
			require.Equal(t, 101, repo.limit)
			if tc.legacy {
				require.Equal(t, 1, repo.legacy)
			} else {
				require.Equal(t, 1, repo.calls)
			}
			if tc.filter.TemplateID == 3 {
				require.Equal(t, int64(1000), repo.cursor)
			}
		})
	}
}

// Only an adapter without the new capability may use the legacy empty-filter path.
type legacyOnlyQualityHistoryRepo struct {
	service.ScheduledTestResultRepository
	calls int
}

func (r *legacyOnlyQualityHistoryRepo) ListQualityHistory(context.Context, int64, int) ([]*service.QualityHistoryResult, error) {
	r.calls++
	return []*service.QualityHistoryResult{}, nil
}
func TestQualityHistoryEmptyFilterLegacyAdapterFallback(t *testing.T) {
	repo := &legacyOnlyQualityHistoryRepo{}
	svc := service.NewScheduledTestService(nil, repo)
	_, err := svc.ListFilteredQualityHistory(context.Background(), 0, service.QualityHistoryFilter{})
	require.NoError(t, err)
	require.Equal(t, 1, repo.calls)
	_, err = svc.ListFilteredQualityHistory(context.Background(), 0, service.QualityHistoryFilter{PlanID: 9})
	require.Error(t, err)
	require.Equal(t, 1, repo.calls)
}
