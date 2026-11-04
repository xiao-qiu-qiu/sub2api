//go:build unit

package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func accountPriorityRequest(t *testing.T, router *gin.Engine, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestAccountPriorityDataExportReimport(t *testing.T) {
	for _, priority := range []int{-9999, -1, 0, 9999} {
		t.Run(fmt.Sprint(priority), func(t *testing.T) {
			router, adminSvc := setupAccountDataRouter()
			adminSvc.accounts = []service.Account{{
				ID: 1, Name: "priority-fixture", Platform: service.PlatformOpenAI,
				Type: service.AccountTypeOAuth, Credentials: map[string]any{"access_token": "fixture-token"},
				Concurrency: 3, Priority: priority, Status: service.StatusActive,
			}}
			exported := httptest.NewRecorder()
			router.ServeHTTP(exported, httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/data?include_proxies=false", nil))
			require.Equal(t, http.StatusOK, exported.Code, exported.Body.String())
			var payload struct {
				Data DataPayload `json:"data"`
			}
			require.NoError(t, json.Unmarshal(exported.Body.Bytes(), &payload))
			require.Len(t, payload.Data.Accounts, 1)
			require.Equal(t, priority, payload.Data.Accounts[0].Priority)

			imported := accountPriorityRequest(t, router, "/api/v1/admin/accounts/data", DataImportRequest{Data: payload.Data})
			require.Equal(t, http.StatusOK, imported.Code, imported.Body.String())
			var result struct {
				Data DataImportResult `json:"data"`
			}
			require.NoError(t, json.Unmarshal(imported.Body.Bytes(), &result))
			require.Equal(t, 1, result.Data.AccountCreated)
			require.Zero(t, result.Data.AccountFailed)
			require.Len(t, adminSvc.createdAccounts, 1)
			require.Equal(t, priority, adminSvc.createdAccounts[0].Priority)
		})
	}
}

func TestAccountPriorityDataImportRejectsOutOfRange(t *testing.T) {
	for _, priority := range []int{-10000, 10000} {
		t.Run(fmt.Sprint(priority), func(t *testing.T) {
			router, adminSvc := setupAccountDataRouter()
			response := accountPriorityRequest(t, router, "/api/v1/admin/accounts/data", DataImportRequest{
				Data: DataPayload{Proxies: []DataProxy{}, Accounts: []DataAccount{{
					Name: "priority-fixture", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
					Credentials: map[string]any{"access_token": "fixture-token"}, Priority: priority,
				}}},
			})
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var result struct {
				Data DataImportResult `json:"data"`
			}
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
			require.Equal(t, 1, result.Data.AccountFailed)
			require.Zero(t, result.Data.AccountCreated)
			require.Len(t, result.Data.Errors, 1)
			require.Contains(t, result.Data.Errors[0].Message, "priority")
			require.Empty(t, adminSvc.createdAccounts)
		})
	}
}

func TestAccountPriorityCodexImportCreateAndUpdate(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		for _, value := range []string{"omitted", "null", "-9999", "-1", "0", "9999"} {
			t.Run(fmt.Sprintf("scoped=%t/%s", scoped, value), func(t *testing.T) {
				gin.SetMode(gin.TestMode)
				memory := &scopedCodexMemoryService{codexImportMemoryAdminService: newCodexImportMemoryAdminService(nil)}
				handler := &AccountHandler{adminService: memory}
				router := gin.New()
				if scoped {
					principal := &adminaccess.Principal{KeyID: "priority-import-fixture", Policy: adminaccess.Policy{Rules: []adminaccess.Rule{{
						Permissions: []string{"accounts.create", "accounts.credentials", "accounts.schedule", "accounts.network"},
						GroupIDs:    []int64{2}, AccountTypes: []string{"oauth"},
					}}}}
					router.Use(func(c *gin.Context) {
						c.Request = c.Request.WithContext(adminaccess.WithPrincipal(c.Request.Context(), principal))
					})
					router.Use(handler.ScopedAdminKeyAccounts)
				}
				const path = "/api/v1/admin/accounts/import/codex-session"
				router.POST(path, handler.ImportCodexSession)
				content, err := json.Marshal(map[string]any{"access_token": "priority-access-fixture", "refresh_token": "priority-refresh-fixture"})
				require.NoError(t, err)
				body := map[string]any{"content": string(content), "group_ids": []int64{2}}
				var priority *int
				if value != "omitted" {
					require.NoError(t, json.Unmarshal([]byte(value), &priority))
					body["priority"] = priority
				}
				created := accountPriorityRequest(t, router, path, body)
				require.Equal(t, http.StatusOK, created.Code, created.Body.String())
				require.Contains(t, created.Body.String(), `"created":1`)
				require.Len(t, memory.createdAccounts, 1)
				wantPriority := 50
				if priority != nil {
					wantPriority = *priority
				}
				require.Equal(t, wantPriority, memory.createdAccounts[0].Priority)

				updated := accountPriorityRequest(t, router, path, body)
				require.Equal(t, http.StatusOK, updated.Code, updated.Body.String())
				require.Contains(t, updated.Body.String(), `"updated":1`)
				require.Len(t, memory.updatedAccounts, 1)
				require.Equal(t, priority, memory.updatedAccounts[0].input.Priority)
			})
		}
	}
}

func TestAccountPriorityHandlersRejectInvalidValuesBeforeOAuth(t *testing.T) {
	for _, endpoint := range []string{"codex_import", "scoped_codex_import", "openai_oauth", "openai_pat", "grok_oauth", "grok_sso", "shadow"} {
		for _, value := range []any{-10000, 10000, 1.5, "0"} {
			t.Run(fmt.Sprintf("%s/%v", endpoint, value), func(t *testing.T) {
				gin.SetMode(gin.TestMode)
				stub := newStubAdminService()
				accountHandler := &AccountHandler{adminService: stub}
				openaiHandler := NewOpenAIOAuthHandler(nil, stub, nil, nil)
				grokHandler := NewGrokOAuthHandler(nil, stub, nil, nil)
				router := gin.New()
				body := map[string]any{
					"priority": value, "content": "fixture-token", "group_ids": []int64{2},
					"session_id": "fixture-session", "code": "fixture-code", "state": "fixture-state",
					"access_token": "fixture-token", "sso_token": "fixture-token",
				}
				var handle gin.HandlerFunc
				switch endpoint {
				case "codex_import":
					handle = accountHandler.ImportCodexSession
				case "scoped_codex_import":
					handle = accountHandler.scopedAdminImportCodexSession
				case "openai_oauth":
					handle = openaiHandler.CreateAccountFromOAuth
				case "openai_pat":
					handle = openaiHandler.CreateAccountFromCodexPAT
				case "grok_oauth":
					handle = grokHandler.CreateAccountFromOAuth
				case "grok_sso":
					handle = grokHandler.CreateAccountsFromSSO
				case "shadow":
					handle = openaiHandler.CreateShadow
				}
				router.POST("/accounts/:id", handle)
				response := accountPriorityRequest(t, router, "/accounts/42", body)
				require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
				require.Empty(t, stub.createdAccounts)
				require.Nil(t, stub.lastCreateShadowOptions)
				if _, integer := value.(int); integer {
					require.Contains(t, response.Body.String(), `"reason":"ACCOUNT_PRIORITY_INVALID"`)
				}
			})
		}
	}
}

func TestAccountPriorityShadowPreservesExplicitAndOmittedValues(t *testing.T) {
	for _, value := range []string{"omitted", "null", "-9999", "-1", "0", "9999"} {
		t.Run(value, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			stub := newStubAdminService()
			handler := NewOpenAIOAuthHandler(nil, stub, nil, nil)
			router := gin.New()
			router.POST("/accounts/:id/shadow", handler.CreateShadow)
			body := map[string]any{"name": "priority-shadow"}
			var priority *int
			if value != "omitted" {
				require.NoError(t, json.Unmarshal([]byte(value), &priority))
				body["priority"] = priority
			}
			response := accountPriorityRequest(t, router, "/accounts/42/shadow", body)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			require.NotNil(t, stub.lastCreateShadowOptions)
			require.Equal(t, priority, stub.lastCreateShadowOptions.Priority)
			if priority != nil {
				var payload struct {
					Data struct {
						Priority int `json:"priority"`
					} `json:"data"`
				}
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &payload))
				require.Equal(t, *priority, payload.Data.Priority)
			}
		})
	}
}

func TestAccountPriorityGrokSSOForwardsSignedValues(t *testing.T) {
	for _, priority := range []int{-9999, -1, 0, 9999} {
		t.Run(fmt.Sprint(priority), func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			stub := newStubAdminService()
			oauthService := service.NewGrokOAuthService(nil, &grokOAuthHandlerClient{})
			t.Cleanup(oauthService.Stop)
			handler := NewGrokOAuthHandler(oauthService, stub, nil, nil)
			router := gin.New()
			router.POST("/grok/import-sso", handler.CreateAccountsFromSSO)
			response := accountPriorityRequest(t, router, "/grok/import-sso", map[string]any{
				"sso_token": "priority-fixture", "priority": priority,
			})
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			require.Len(t, stub.createdAccounts, 1, response.Body.String())
			require.Equal(t, priority, stub.createdAccounts[0].Priority)
		})
	}
}
