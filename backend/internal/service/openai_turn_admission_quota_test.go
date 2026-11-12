package service

import (
	"context"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func turnAdmissionQuotaAccount(window string, used float64, reset time.Time) *Account {
	a := ticketTestAccount(9918301)
	a.Extra = map[string]any{
		"codex_" + window + "_used_percent": used,
		"codex_" + window + "_reset_at":     reset.UTC().Format(time.RFC3339),
		"codex_usage_updated_at":            time.Now().UTC().Format(time.RFC3339),
	}
	return a
}

func requireTurnQuotaDenied(t *testing.T, got *Account, err error, reason string) {
	t.Helper()
	var denied *OpenAITurnAdmissionError
	require.ErrorAs(t, err, &denied)
	require.Equal(t, reason, denied.Reason)
	require.Nil(t, got)
}

func TestOpenAITurnAdmissionQuotaLatestSnapshot(t *testing.T) {
	for _, window := range []string{"5h", "7d"} {
		for _, phase := range []string{"after_selection", "next_turn"} {
			t.Run(window+"/"+phase, func(t *testing.T) {
				selected := turnAdmissionQuotaAccount(window, 20, time.Now().Add(time.Hour))
				repo := &turnAdmissionRepo{account: cloneOpenAITurnAdmissionAccount(selected)}
				svc := &OpenAIGatewayService{accountRepo: repo, requireLatestTurnAdmission: true}
				if phase == "next_turn" {
					got, err := svc.AdmitOpenAITurn(context.Background(), nil, selected, "gpt-6-astra")
					require.NoError(t, err)
					require.Equal(t, selected.ID, got.ID)
				}
				repo.account = turnAdmissionQuotaAccount(window, 100, time.Now().Add(time.Hour))
				before := repo.reads

				got, err := svc.AdmitOpenAITurn(context.Background(), nil, selected, "gpt-6-astra")

				requireTurnQuotaDenied(t, got, err, "quota_exhausted_"+window)
				require.Equal(t, before+1, repo.reads, "use the authoritative snapshot, not the selected account")
				require.Equal(t, 20.0, selected.Extra["codex_"+window+"_used_percent"])
				require.True(t, repo.account.Schedulable)
				require.Nil(t, repo.account.TempUnschedulableUntil)
				require.Empty(t, selected.AuthConnectionID)
				require.Empty(t, repo.account.AuthConnectionID, "a rejected request cannot bind a connection")
			})
		}
	}
}

func TestOpenAITurnAdmissionQuotaPolicy(t *testing.T) {
	for _, window := range []string{"5h", "7d"} {
		for _, tc := range []struct {
			name                      string
			used, threshold, defaults float64
			disabled, expired         bool
			manual, probe             bool
			reason                    string
		}{
			{name: "expired_window", used: 100, threshold: .75, expired: true},
			{name: "manual_test", used: 100, manual: true},
			{name: "quality_probe_respects_quota", used: 100, probe: true, reason: "quota_exhausted_" + window},
			{name: "account_early_pause", used: 80, threshold: .75, reason: "quota_auto_pause_" + window},
			{name: "global_early_pause", used: 80, defaults: .75, reason: "quota_auto_pause_" + window},
			{name: "below_threshold", used: 74, threshold: .75},
			{name: "disabled_early_pause", used: 80, defaults: .75, disabled: true},
			{name: "disabled_exhaustion", used: 100, threshold: .75, disabled: true, reason: "quota_exhausted_" + window},
		} {
			t.Run(window+"/"+tc.name, func(t *testing.T) {
				reset := time.Now().Add(time.Hour)
				if tc.expired {
					reset = time.Now().Add(-time.Minute)
				}
				selected := ticketTestAccount(9918301)
				latest := turnAdmissionQuotaAccount(window, tc.used, reset)
				latest.Extra["auto_pause_"+window+"_threshold"] = tc.threshold
				latest.Extra["auto_pause_"+window+"_disabled"] = tc.disabled
				repo := &turnAdmissionRepo{account: latest}
				settings := &SettingService{}
				settings.SetOpenAIQuotaAutoPauseSettings(OpsOpenAIAccountQuotaAutoPauseSettings{
					DefaultThreshold5h: tc.defaults, DefaultThreshold7d: tc.defaults,
				})
				svc := &OpenAIGatewayService{accountRepo: repo, settingService: settings, requireLatestTurnAdmission: true}
				ctx := context.Background()
				if tc.manual {
					ctx = WithManualAccountTest(ctx)
				}
				if tc.probe {
					ctx = context.WithValue(ctx, qualityProbeContextKey{}, true)
				}
				before := manualAdmissionAccountState(t, latest)

				// Connection callbacks share this boundary even without gin state.
				got, err := svc.admitOpenAITurnForGroup(ctx, 0, false, selected, "gpt-6-astra")

				if tc.reason != "" {
					requireTurnQuotaDenied(t, got, err, tc.reason)
				} else {
					require.NoError(t, err)
					require.Same(t, latest, got)
				}
				require.Equal(t, 1, repo.reads)
				require.JSONEq(t, before, manualAdmissionAccountState(t, latest), "admission must not modify scheduling or binding state")
			})
		}
	}
}

func TestOpenAITurnAdmissionQuotaCacheKeepsRequestScope(t *testing.T) {
	for _, window := range []string{"5h", "7d"} {
		t.Run(window, func(t *testing.T) {
			selected := ticketTestAccount(9918301)
			latest := turnAdmissionQuotaAccount(window, 100, time.Now().Add(time.Hour))
			repo := &turnAdmissionRepo{account: latest}
			cfg := passthroughLifecycleConfig()
			cfg.Gateway.Scheduling.OpenAITurnAdmissionCacheTTLSeconds = 300
			svc := &OpenAIGatewayService{accountRepo: repo, cfg: cfg, requireLatestTurnAdmission: true}
			manual := WithManualAccountTest(context.Background())

			got, err := svc.AdmitOpenAITurn(manual, nil, selected, "gpt-6-astra")
			require.NoError(t, err)
			require.Equal(t, latest.ID, got.ID)
			got, err = svc.AdmitOpenAITurn(context.Background(), nil, selected, "gpt-6-astra")
			requireTurnQuotaDenied(t, got, err, "quota_exhausted_"+window)
			require.Equal(t, 1, repo.reads, "cached state must still run quota admission for the next request")
			require.Empty(t, got)
			require.Empty(t, selected.AuthConnectionID)
		})
	}
}

func TestOpenAITurnAdmissionQuotaAccountScope(t *testing.T) {
	for _, mode := range []string{"api_key", "managed_aggregate", "managed_api_key_projection", "shadow_parent_exhausted", "shadow_exhausted"} {
		t.Run(mode, func(t *testing.T) {
			latest := turnAdmissionQuotaAccount("7d", 100, time.Now().Add(time.Hour))
			selected := cloneOpenAITurnAdmissionAccount(latest)
			repo := &turnAdmissionRepo{account: latest}
			wantDenied := false
			switch mode {
			case "api_key":
				latest.Type, selected.Type = AccountTypeAPIKey, AccountTypeAPIKey
			case "managed_aggregate", "managed_api_key_projection":
				latest.Extra["openai_connections_managed"] = true
				selected.Extra["openai_connections_managed"] = true
				selected.AuthConnectionID = "synthetic-selected-connection"
				if mode == "managed_api_key_projection" {
					selected.Type = AccountTypeAPIKey
				}
			case "shadow_parent_exhausted", "shadow_exhausted":
				parent := turnAdmissionQuotaAccount("7d", 100, time.Now().Add(time.Hour))
				parent.ID++
				latest.ParentAccountID, selected.ParentAccountID = &parent.ID, &parent.ID
				repo.parent = parent
				if mode == "shadow_parent_exhausted" {
					latest.Extra["codex_7d_used_percent"] = 20.0
				} else {
					parent.Extra["codex_7d_used_percent"] = 20.0
					wantDenied = true
				}
			}
			svc := &OpenAIGatewayService{accountRepo: repo, requireLatestTurnAdmission: true}
			selectedConnection := selected.AuthConnectionID

			got, err := svc.admitOpenAITurnForGroup(context.Background(), 0, false, selected, "gpt-6-astra")

			if wantDenied {
				requireTurnQuotaDenied(t, got, err, "quota_exhausted_7d")
			} else {
				require.NoError(t, err)
				require.Same(t, latest, got)
			}
			require.Equal(t, 1, repo.reads)
			require.Empty(t, latest.AuthConnectionID, "a selected projection cannot bind the aggregate account")
			require.Equal(t, selectedConnection, selected.AuthConnectionID)
		})
	}
}

func TestOpenAITurnAdmissionQuotaManagedProjectionKeepsSnapshotScope(t *testing.T) {
	for _, kind := range []string{"codex", "bps", "siwc", "api_key"} {
		for _, manual := range []bool{false, true} {
			t.Run(kind+"/manual="+strconv.FormatBool(manual), func(t *testing.T) {
				latest := turnAdmissionQuotaAccount("7d", 100, time.Now().Add(time.Hour))
				latest.ID = 42
				latest.Extra["openai_connections_managed"] = true
				latest.Extra["auto_pause_7d_disabled"] = true
				selected := cloneOpenAITurnAdmissionAccount(latest)
				reauth, connections := connectionTypeService(AccountTypeOAuth)
				reauth.accounts = &reauthTestAccountReader{account: latest}
				connections.managed = true
				clientID := openai.ClientID
				if kind == "bps" {
					clientID = openai.ExcelClientID
				} else if kind == "siwc" {
					clientID = "synthetic-siwc-client"
				}
				connection := &OpenAIAuthConnection{
					ID: "c910c45b-79a0-4367-a1dc-60e12210e199", AccountID: latest.ID,
					Kind: kind, ClientID: clientID, Status: "active", RefreshState: "idle",
					Revision: 1, Preferred: true, Capabilities: []string{"responses"},
				}
				creds := map[string]any{
					"access_token": "synthetic-connection-token", "client_id": clientID,
					"expires_at":         strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10),
					"chatgpt_account_id": "synthetic-selected-grant",
				}
				if kind == "api_key" {
					creds = map[string]any{"api_key": "synthetic-connection-key"}
				}
				require.NoError(t, reauth.encryptConnectionCredentials(connection, creds))
				connections.rows = []*OpenAIAuthConnection{connection}
				repo := &turnAdmissionRepo{account: latest}
				svc := &OpenAIGatewayService{accountRepo: repo, excelOAuthReauth: reauth, requireLatestTurnAdmission: true}
				ctx := context.Background()
				if manual {
					ctx = WithManualAccountTest(ctx)
				}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest("POST", "/v1/responses", nil)

				projected, err := svc.AdmitOpenAITurn(ctx, c, selected, "gpt-6-astra")

				require.NoError(t, err, "aggregate usage is not a connection-owned quota observation")
				require.Equal(t, latest.ID, projected.ID)
				require.Equal(t, connection.ID, projected.AuthConnectionID)
				require.Equal(t, int64(1), projected.AuthConnectionRevision)
				require.Equal(t, 100.0, projected.Extra["codex_7d_used_percent"], "projection inherits the aggregate snapshot")
				if kind == "api_key" {
					require.Equal(t, AccountTypeAPIKey, projected.Type)
					require.Equal(t, "synthetic-connection-key", projected.GetCredential("api_key"))
					require.Empty(t, projected.GetCredential("access_token"))
				} else {
					require.Equal(t, AccountTypeOAuth, projected.Type)
					require.Equal(t, "synthetic-connection-token", projected.GetCredential("access_token"))
				}
				require.Empty(t, latest.AuthConnectionID, "projection must not bind or mutate the aggregate")
				require.Empty(t, selected.AuthConnectionID)
				require.Equal(t, 1, repo.reads)
				require.Zero(t, connections.writes)
			})
		}
	}
}

func TestOpenAITurnAdmissionQuotaAutoResetNotification(t *testing.T) {
	for _, window := range []string{"5h", "7d"} {
		t.Run(window, func(t *testing.T) {
			selected := ticketTestAccount(9918302)
			latest := turnAdmissionQuotaAccount(window, 100, time.Now().Add(time.Hour))
			latest.ID = selected.ID
			latest.Extra[OpenAIAutoResetCreditEnabledExtraKey] = true
			repo := &turnAdmissionRepo{account: latest}
			svc := &OpenAIGatewayService{accountRepo: repo, requireLatestTurnAdmission: true}
			reset := &OpenAIQuotaAutoResetService{ctx: context.Background(), queue: make(chan int64, 8)}
			setOpenAIAutoResetNotifier(reset)
			t.Cleanup(func() {
				clearOpenAIAutoResetNotifier(reset)
				openAIAutoResetSchedulerNotifiedAt.Delete(selected.ID)
			})

			_, err := svc.AdmitOpenAITurn(WithManualAccountTest(context.Background()), nil, selected, "gpt-6-astra")
			require.NoError(t, err)
			require.Empty(t, reset.queue, "manual diagnostics must not trigger automatic credit use")
			for range 2 {
				got, err := svc.AdmitOpenAITurn(context.Background(), nil, selected, "gpt-6-astra")
				requireTurnQuotaDenied(t, got, err, "quota_auto_reset_pending_"+window)
			}
			require.Len(t, reset.queue, 1, "repeated admission keeps the notifier's per-account cooldown")
			require.Equal(t, selected.ID, <-reset.queue)
		})
	}
}

func TestOpenAITurnAdmissionQuotaFailureKeepsRecoverableRouting(t *testing.T) {
	for _, tc := range []struct {
		reason string
		keep   bool
	}{
		{"quota_exhausted_5h", true},
		{"quota_exhausted_7d", true},
		{"quota_auto_pause_5h", true},
		{"quota_auto_pause_7d", true},
		{"quota_auto_pause", true},
		{"quota_auto_reset_pending_5h", true},
		{"quota_auto_reset_pending_7d", true},
		{"quota_auto_reset_credit_check_5h", true},
		{"quota_auto_reset_credit_check_7d", true},
		{"account_ineligible", false},
		{"account_binding_changed", false},
		{"group_membership_changed", false},
		{"credential_parent_ineligible", false},
		{"model_runtime_blocked", false},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			const groupID int64 = 44
			const accountID int64 = 9918303
			const responseID = "resp_quota_recoverable"
			const sessionHash = "session-quota-recoverable"
			ctx := context.Background()
			cache := &stubGatewayCache{}
			store := NewOpenAIWSStateStore(cache)
			svc := &OpenAIGatewayService{cache: cache, openaiWSStateStore: store}
			require.NoError(t, store.BindResponseAccount(ctx, groupID, responseID, accountID, time.Hour))
			store.BindResponseConn(responseID, "conn-rejected", time.Hour)
			store.BindSessionTurnState(groupID, sessionHash, "turn-state-rejected", time.Hour)
			store.BindSessionConn(groupID, sessionHash, "conn-rejected", time.Hour)
			group := groupID
			require.NoError(t, svc.setStickySessionAccountID(ctx, &group, sessionHash, accountID, time.Hour))
			require.NoError(t, store.BindResponseAccount(ctx, groupID, "resp_unrelated", accountID+1, time.Hour))
			store.BindResponseConn("resp_unrelated", "conn-unrelated", time.Hour)
			store.BindSessionTurnState(groupID, "session-unrelated", "turn-state-unrelated", time.Hour)
			store.BindSessionConn(groupID, "session-unrelated", "conn-unrelated", time.Hour)
			require.NoError(t, svc.setStickySessionAccountID(ctx, &group, "session-unrelated", accountID+1, time.Hour))
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Set("api_key", &APIKey{GroupID: &group})
			c.Set(openAIWSIngressSessionHashContextKey, sessionHash)
			wrapped := NewOpenAIWSClientCloseError(coderws.StatusTryAgainLater, "account eligibility changed", &OpenAITurnAdmissionError{Reason: tc.reason})

			svc.invalidateOpenAIWSTurnStateAfterAdmissionFailureForRequest(ctx, c,
				[]byte(`{"type":"response.create","previous_response_id":"resp_quota_recoverable"}`), accountID, wrapped)

			want := int64(0)
			if tc.keep {
				want = accountID
			}
			bound, err := store.GetResponseAccount(ctx, groupID, responseID)
			require.NoError(t, err)
			require.Equal(t, want, bound)
			// A different process must see the same retained logical route.
			bound, err = NewOpenAIWSStateStore(cache).GetResponseAccount(ctx, groupID, responseID)
			require.NoError(t, err)
			require.Equal(t, want, bound)
			sticky, err := svc.getStickySessionAccountID(ctx, &group, sessionHash)
			stickyKey := svc.openAISessionCacheKey(sessionHash)
			responseKey := openAIWSResponseAccountCacheKey(responseID)
			if tc.keep {
				require.NoError(t, err)
				require.Equal(t, accountID, cache.sessionBindings[stickyKey])
				require.Zero(t, cache.deletedSessions[stickyKey])
				require.Zero(t, cache.deletedSessions[responseKey])
			} else {
				// This fixture returns "not found" for a deleted sticky key.
				// Assert the deletion itself rather than requiring a nil read error.
				require.NotContains(t, cache.sessionBindings, stickyKey)
				require.NotContains(t, cache.sessionBindings, responseKey)
				require.Equal(t, 1, cache.deletedSessions[stickyKey])
				require.Equal(t, 1, cache.deletedSessions[responseKey])
			}
			require.Equal(t, want, sticky)
			_, ok := store.GetResponseConn(responseID)
			require.False(t, ok, "quota recovery must not reuse the rejected connection")
			_, ok = store.GetSessionTurnState(groupID, sessionHash)
			require.False(t, ok)
			_, ok = store.GetSessionConn(groupID, sessionHash)
			require.False(t, ok)
			unrelated, err := store.GetResponseAccount(ctx, groupID, "resp_unrelated")
			require.NoError(t, err)
			require.Equal(t, accountID+1, unrelated)
			conn, ok := store.GetResponseConn("resp_unrelated")
			require.True(t, ok)
			require.Equal(t, "conn-unrelated", conn)
			turn, ok := store.GetSessionTurnState(groupID, "session-unrelated")
			require.True(t, ok)
			require.Equal(t, "turn-state-unrelated", turn)
			conn, ok = store.GetSessionConn(groupID, "session-unrelated")
			require.True(t, ok)
			require.Equal(t, "conn-unrelated", conn)
			sticky, err = svc.getStickySessionAccountID(ctx, &group, "session-unrelated")
			require.NoError(t, err)
			require.Equal(t, accountID+1, sticky)
		})
	}
}

// Publish independent DB snapshots while a loopback WebSocket is running.
// Never mutate the selected account or the snapshot already read by a turn.
type turnQuotaAdmissionReader struct {
	AccountRepository
	account atomic.Pointer[Account]
	reads   atomic.Int64
}

func (r *turnQuotaAdmissionReader) GetOpenAITurnAdmission(_ context.Context, id int64) (*Account, *Account, error) {
	r.reads.Add(1)
	account := r.account.Load()
	if account == nil || account.ID != id {
		return nil, nil, nil
	}
	return cloneOpenAITurnAdmissionAccount(account), nil, nil
}

func (r *turnQuotaAdmissionReader) GetByID(ctx context.Context, id int64) (*Account, error) {
	account, _, err := r.GetOpenAITurnAdmission(ctx, id)
	return account, err
}

func TestOpenAITurnAdmissionQuotaFailureRouteResumesAfterReset(t *testing.T) {
	for _, window := range []string{"5h", "7d"} {
		t.Run(window, func(t *testing.T) {
			ctx := context.Background()
			groupID := int64(45)
			selected := turnAdmissionQuotaAccount(window, 20, time.Now().Add(time.Hour))
			selected.GroupIDs = []int64{groupID}
			selected.Extra["openai_oauth_responses_websockets_v2_enabled"] = true
			latest := cloneOpenAITurnAdmissionAccount(selected)
			latest.Extra["codex_"+window+"_used_percent"] = 100.0
			latest.Extra["auto_pause_"+window+"_disabled"] = true
			repo := &turnQuotaAdmissionReader{}
			repo.account.Store(latest)
			store := NewOpenAIWSStateStore(nil)
			svc := &OpenAIGatewayService{
				accountRepo: repo, requireLatestTurnAdmission: true, openaiWSStateStore: store,
				cfg: newOpenAIWSV2TestConfig(), concurrencyService: NewConcurrencyService(stubConcurrencyCache{}),
			}
			require.NoError(t, store.BindResponseAccount(ctx, groupID, "resp_quota_resume", selected.ID, time.Hour))
			got, err := svc.AdmitOpenAITurn(ctx, nil, selected, "gpt-6-astra")
			requireTurnQuotaDenied(t, got, err, "quota_exhausted_"+window)
			svc.invalidateOpenAIWSTurnStateAfterAdmissionFailure(ctx, groupID, "session-quota-resume", "resp_quota_resume", selected.ID, err)
			selection, err := svc.SelectAccountByPreviousResponseID(ctx, &groupID, "resp_quota_resume", "gpt-6-astra", nil, false)
			require.NoError(t, err)
			require.Nil(t, selection, "retaining the route must not bypass the active quota gate")
			bound, err := store.GetResponseAccount(ctx, groupID, "resp_quota_resume")
			require.NoError(t, err)
			require.Equal(t, selected.ID, bound)

			reset := cloneOpenAITurnAdmissionAccount(latest)
			reset.Extra["codex_"+window+"_reset_at"] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
			repo.account.Store(reset)
			selection, err = svc.SelectAccountByPreviousResponseID(ctx, &groupID, "resp_quota_resume", "gpt-6-astra", nil, false)
			require.NoError(t, err)
			require.NotNil(t, selection, "the original response/account route must recover after reset")
			if selection.ReleaseFunc != nil {
				defer selection.ReleaseFunc()
			}
			require.Equal(t, selected.ID, selection.Account.ID)
			_, err = svc.AdmitOpenAITurn(ctx, nil, selection.Account, "gpt-6-astra")
			require.NoError(t, err)
		})
	}
}

func TestOpenAITurnAdmissionQuotaWebSocketNextTurn(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModeDedicated, OpenAIWSIngressModePassthrough} {
		for _, window := range []string{"5h", "7d"} {
			t.Run(mode+"/"+window, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				cfg := passthroughLifecycleConfig()
				cfg.Gateway.OpenAIWS.OAuthEnabled = true
				cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
				cfg.Gateway.OpenAIWS.IngressInterTurnIdleTimeoutSeconds = 3
				cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
				cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
				selected := turnAdmissionQuotaAccount(window, 20, time.Now().Add(time.Hour))
				selected.Concurrency = 1
				selected.Extra["openai_oauth_responses_websockets_v2_mode"] = mode
				selected.Extra["auto_pause_"+window+"_disabled"] = true
				repo := &turnQuotaAdmissionReader{}
				repo.account.Store(cloneOpenAITurnAdmissionAccount(selected))
				upstream := newStagedPassthroughConn()
				svc := newPassthroughLifecycleService(cfg, upstream)
				svc.accountRepo, svc.requireLatestTurnAdmission = repo, true
				svc.openaiWSStateStore = NewOpenAIWSStateStore(nil)
				if mode != OpenAIWSIngressModePassthrough {
					pool := newOpenAIWSConnPool(cfg)
					pool.setClientDialerForTest(&stagedPassthroughDialer{conn: &turnAdmissionNativeConn{upstream}})
					svc.openaiWSPool = pool
					defer pool.Close()
				}
				server, done := startPassthroughHookRecordingServer(t, ctx, svc, selected, nil)
				defer server.Close()
				client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
				require.NoError(t, err)
				defer func() { _ = client.CloseNow() }()
				require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-6-astra","input":"synthetic"}`)))
				requirePassthroughUpstreamWrite(t, upstream, time.Second)
				upstream.Send(`{"type":"response.output_text.delta","delta":"started"}`)
				_, frame, err := client.Read(ctx)
				require.NoError(t, err)
				require.Equal(t, "response.output_text.delta", gjson.GetBytes(frame, "type").String())

				latest := cloneOpenAITurnAdmissionAccount(selected)
				latest.Extra["codex_"+window+"_used_percent"] = 100.0
				repo.account.Store(latest)
				upstream.Send(`{"type":"response.completed","response":{"id":"resp_quota_first","model":"gpt-6-astra","usage":{"input_tokens":1,"output_tokens":1}}}`)
				_, frame, err = client.Read(ctx)
				require.NoError(t, err, "the admitted turn must drain after quota becomes exhausted")
				require.Equal(t, "response.completed", gjson.GetBytes(frame, "type").String())
				store := svc.getOpenAIWSStateStore()
				require.NoError(t, store.BindResponseAccount(ctx, 0, "resp_quota_first", selected.ID, time.Hour))
				require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-6-astra","previous_response_id":"resp_quota_first","input":"next"}`)))
				select {
				case payload := <-upstream.writes:
					t.Fatalf("quota-exhausted next turn reached upstream: %s", payload)
				case err := <-done:
					requireTurnQuotaDenied(t, nil, err, "quota_exhausted_"+window)
				case <-ctx.Done():
					t.Fatal("quota admission did not stop the next turn")
				}
				require.Greater(t, repo.reads.Load(), int64(1))
				require.Empty(t, upstream.writes)
				bound, err := store.GetResponseAccount(ctx, 0, "resp_quota_first")
				require.NoError(t, err)
				require.Equal(t, selected.ID, bound, "quota rejection retains logical continuation ownership")
				_, ok := store.GetResponseConn("resp_quota_first")
				require.False(t, ok)
				select {
				case <-upstream.closed:
				default:
					t.Fatal("quota-rejected upstream connection was not closed")
				}
				require.Equal(t, 20.0, selected.Extra["codex_"+window+"_used_percent"])
				require.True(t, latest.Schedulable)
				require.Nil(t, latest.TempUnschedulableUntil)
			})
		}
	}
}

func TestOpenAITurnAdmissionQuotaWebSocketRecoveryRechecks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cfg := passthroughLifecycleConfig()
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.IngressPreviousResponseRecoveryEnabled = true
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	selected := turnAdmissionQuotaAccount("7d", 20, time.Now().Add(time.Hour))
	selected.Concurrency = 1
	selected.Extra["openai_oauth_responses_websockets_v2_mode"] = OpenAIWSIngressModeCtxPool
	repo := &turnQuotaAdmissionReader{}
	repo.account.Store(cloneOpenAITurnAdmissionAccount(selected))
	upstream := newStagedPassthroughConn()
	svc := newPassthroughLifecycleService(cfg, upstream)
	svc.accountRepo, svc.requireLatestTurnAdmission = repo, true
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(&stagedPassthroughDialer{conn: &turnAdmissionNativeConn{upstream}})
	svc.openaiWSPool = pool
	defer pool.Close()
	var beforeTurnCalls atomic.Int64
	hooks := &OpenAIWSIngressHooks{BeforeTurn: func(int) error {
		beforeTurnCalls.Add(1)
		return nil
	}}
	server, done := startPassthroughHookRecordingServer(t, ctx, svc, selected, hooks)
	defer server.Close()
	client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	defer func() { _ = client.CloseNow() }()
	require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-6-astra","input":[{"role":"user","content":"synthetic"}]}`)))
	requirePassthroughUpstreamWrite(t, upstream, time.Second)
	upstream.Send(`{"type":"response.completed","response":{"id":"resp_quota_seed","model":"gpt-6-astra","usage":{"input_tokens":1,"output_tokens":1}}}`)
	_, frame, err := client.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, "response.completed", gjson.GetBytes(frame, "type").String())
	require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-6-astra","previous_response_id":"resp_quota_seed","input":[{"role":"user","content":"next"}]}`)))
	requirePassthroughUpstreamWrite(t, upstream, time.Second)
	latest := cloneOpenAITurnAdmissionAccount(selected)
	latest.Extra["codex_7d_used_percent"] = 100.0
	repo.account.Store(latest)
	upstream.Send(`{"type":"error","error":{"type":"invalid_request_error","code":"previous_response_not_found","message":"Previous response not found."}}`)
	select {
	case payload := <-upstream.writes:
		t.Fatalf("quota-exhausted recovery reached upstream: %s", payload)
	case err := <-done:
		requireTurnQuotaDenied(t, nil, err, "quota_exhausted_7d")
	case <-ctx.Done():
		t.Fatal("recovery admission did not terminate")
	}
	require.Equal(t, int64(2), beforeTurnCalls.Load(), "recovery skips billing hooks but must recheck quota")
	require.Empty(t, upstream.writes)
	require.Equal(t, 20.0, selected.Extra["codex_7d_used_percent"])
}
