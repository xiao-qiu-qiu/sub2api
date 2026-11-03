package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type tokenGuard401RulesRepo struct {
	service.AccountTokenGuardV2Repository
}

func (*tokenGuard401RulesRepo) RescheduleEnabled(context.Context) error { return nil }

type tokenGuard401RulesSettings struct{ raw string }

func (s *tokenGuard401RulesSettings) GetValue(context.Context, string) (string, error) {
	return s.raw, nil
}
func (s *tokenGuard401RulesSettings) Set(_ context.Context, _, value string) error {
	s.raw = value
	return nil
}

func TestTokenGuardV2SaveRulesPreserves401IntervalForOldClients(t *testing.T) {
	for _, tc := range []struct {
		name, field     string
		status, seconds int
	}{
		{"omitted", "", http.StatusOK, 45},
		{"custom", `,"unauthorized_retry_interval_seconds":15`, http.StatusOK, 15},
		{"zero", `,"unauthorized_retry_interval_seconds":0`, http.StatusBadRequest, 45},
		{"negative", `,"unauthorized_retry_interval_seconds":-1`, http.StatusBadRequest, 45},
		{"too_large", `,"unauthorized_retry_interval_seconds":86401`, http.StatusBadRequest, 45},
		{"fraction", `,"unauthorized_retry_interval_seconds":1.5`, http.StatusBadRequest, 45},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := &tokenGuard401RulesSettings{raw: `{"probe_interval_seconds":1800,"retry_interval_seconds":300,"relogin_cooldown_seconds":1800,"fail_streak_threshold":2,"unauthorized_retry_interval_seconds":45}`}
			svc := service.NewAccountTokenGuardV2Service(&tokenGuard401RulesRepo{}, settings, nil, nil, nil)
			handler := NewAccountTokenGuardV2Handler(svc)
			router := gin.New()
			router.PUT("/rules", handler.SaveRules)
			body := `{"probe_interval_seconds":1800,"retry_interval_seconds":300,"relogin_cooldown_seconds":1800,"fail_streak_threshold":2` + tc.field + `}`
			req := httptest.NewRequest(http.MethodPut, "/rules", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			require.Equal(t, tc.status, response.Code, response.Body.String())
			var saved service.AccountTokenGuardV2Rules
			require.NoError(t, json.Unmarshal([]byte(settings.raw), &saved))
			require.Equal(t, tc.seconds, saved.UnauthorizedRetryIntervalSeconds)
		})
	}
}
