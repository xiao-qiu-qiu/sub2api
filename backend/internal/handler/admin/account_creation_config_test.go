//go:build unit

package admin

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAccountCreationConfigOrdinaryAndBatchCreateForwarded(t *testing.T) {
	for _, batch := range []bool{false, true} {
		svc := newStubAdminService()
		h := NewAccountHandler(svc, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
		r := gin.New()
		fields := map[string]any{"name": "manual", "platform": "openai", "type": "oauth", "credentials": map[string]any{"access_token": "fixture"}, "concurrency": 7, "priority": -30, "load_factor": nil, "rate_multiplier": 0, "group_rate_multiplier": 0, "group_ids": []int64{}, "preserve_creation_config": true}
		var body any = fields
		if batch {
			r.POST("/accounts", h.BatchCreate)
			body = map[string]any{"accounts": []any{fields}}
		} else {
			r.POST("/accounts", h.Create)
		}
		w := accountPriorityRequest(t, r, "/accounts", body)
		require.Equal(t, 200, w.Code, w.Body.String())
		require.Len(t, svc.createdAccounts, 1)
		in := svc.createdAccounts[0]
		require.True(t, in.PreserveCreationConfig)
		require.Equal(t, -30, in.Priority)
		require.Nil(t, in.LoadFactor)
		require.NotNil(t, in.GroupRateMultiplier)
		require.Zero(t, *in.GroupRateMultiplier)
	}
}

func TestAccountCreationConfigCodexImportAndTwoFAForwarded(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		for _, twoFA := range []bool{false, true} {
			memory := &scopedCodexMemoryService{codexImportMemoryAdminService: newCodexImportMemoryAdminService(nil)}
			h := &AccountHandler{adminService: memory}
			r := gin.New()
			if scoped {
				r.Use(func(c *gin.Context) {
					principal := &adminaccess.Principal{Policy: adminaccess.Policy{Rules: []adminaccess.Rule{{
						Permissions: []string{"accounts.create", "accounts.credentials", "accounts.schedule", "accounts.network"},
						GroupIDs:    []int64{2}, AccountTypes: []string{"oauth"}, Platforms: []string{"openai"},
					}}}}
					c.Request = c.Request.WithContext(adminaccess.WithPrincipal(c.Request.Context(), principal))
				})
				r.Use(h.ScopedAdminKeyAccounts)
			}
			const path = "/api/v1/admin/accounts/import/codex-session"
			r.POST(path, h.ImportCodexSession)
			content, err := json.Marshal(buildCodexRefreshImportValue(t, "manual-workspace", "manual-user", "manual-refresh"))
			require.NoError(t, err)
			body := map[string]any{"content": string(content), "preserve_creation_config": true, "initialize_openai_two_fa": twoFA, "group_rate_multiplier": 0, "group_ids": []int64{2}, "concurrency": 7, "priority": -30, "load_factor": nil}
			w := accountPriorityRequest(t, r, path, body)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), `"created":1`)
			require.Len(t, memory.createdAccounts, 1)
			in := memory.createdAccounts[0]
			require.True(t, in.PreserveCreationConfig)
			require.NotNil(t, in.GroupRateMultiplier)
			require.Zero(t, *in.GroupRateMultiplier)
			require.Nil(t, in.LoadFactor)
			require.Equal(t, twoFA, in.InitialOpenAITwoFA)
			w = accountPriorityRequest(t, r, path, body)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), `"updated":1`)
			require.Len(t, memory.updatedAccounts, 1)
			require.Zero(t, *memory.updatedAccounts[0].input.GroupRateMultiplier)
		}
	}
}
