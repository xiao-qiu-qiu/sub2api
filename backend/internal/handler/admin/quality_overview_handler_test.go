package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type overviewHandlerResults struct {
	service.ScheduledTestResultRepository
	calls []service.QualityOverviewFilter
}

func (r *overviewHandlerResults) ReadQualityOverview(_ context.Context, f service.QualityOverviewFilter) ([]service.QualityOverviewAccountEvidence, int64, error) {
	r.calls = append(r.calls, f)
	return []service.QualityOverviewAccountEvidence{{AccountID: 42, Name: "No rules"}}, 7, nil
}

func TestQualityOverviewHandlerFilters(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		query  string
		status int
	}{
		{"", 200}, {"?page=2&page_size=10&search=test&group_id=0&test_channel=bps", 200},
		{"?page=0", 400}, {"?page=-1", 400}, {"?page=", 400}, {"?page=1000001", 400},
		{"?page_size=101", 400}, {"?page_size=", 400}, {"?group_id=-1", 400}, {"?group_id=abc", 400},
		{"?group_id=", 400}, {"?test_channel=all", 400}, {"?page=9999999999999999999999999", 400},
	} {
		t.Run(tc.query, func(t *testing.T) {
			repo := &overviewHandlerResults{}
			h := NewScheduledTestHandler(service.NewScheduledTestService(nil, repo))
			r := gin.New()
			r.GET("/admin/account-quality-overview", h.AccountQualityOverview)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/account-quality-overview"+tc.query, nil))
			require.Equal(t, tc.status, w.Code, w.Body.String())
			if tc.status != 200 {
				require.Empty(t, repo.calls)
				return
			}
			require.Len(t, repo.calls, 1)
			var page service.QualityOverviewPage
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page))
			require.EqualValues(t, 7, page.Total)
			require.Equal(t, "untested", page.Items[0].Status)
			if tc.query == "" {
				require.Equal(t, 20, page.PageSize)
				require.Equal(t, "account", repo.calls[0].TestChannel)
			} else {
				require.Equal(t, 2, page.Page)
				require.Equal(t, "bps", repo.calls[0].TestChannel)
				require.Equal(t, "test", repo.calls[0].Search)
				require.EqualValues(t, 0, *repo.calls[0].GroupID)
			}
		})
	}
}
