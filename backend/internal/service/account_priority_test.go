//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestAccountPriorityWritesPreserveSignedValues(t *testing.T) {
	for _, priority := range []int{MinAccountPriority, -1, 0, 1, MaxAccountPriority} {
		t.Run(fmt.Sprint(priority), func(t *testing.T) {
			repo := &accountRepoStubForBulkUpdate{createID: 7}
			svc := &adminServiceImpl{accountRepo: repo}
			created, err := svc.CreateAccount(t.Context(), &CreateAccountInput{
				Name: "priority", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
				Priority: priority, SkipDefaultGroupBind: true,
			})
			require.NoError(t, err)
			require.Equal(t, priority, created.Priority)
			require.Equal(t, priority, repo.createAccount.Priority)

			repo.getByIDAccounts = map[int64]*Account{7: {
				ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Priority: 50,
			}}
			updated, err := svc.UpdateAccount(t.Context(), 7, &UpdateAccountInput{Priority: &priority})
			require.NoError(t, err)
			require.Equal(t, priority, updated.Priority)
			require.Equal(t, priority, repo.updatedAccounts[0].Priority)

			result, err := svc.BulkUpdateAccounts(t.Context(), &BulkUpdateAccountsInput{
				AccountIDs: []int64{7, 8}, Priority: &priority,
			})
			require.NoError(t, err)
			require.Equal(t, []int64{7, 8}, result.SuccessIDs)
			require.NotNil(t, repo.lastBulkUpdate.Priority)
			require.Equal(t, priority, *repo.lastBulkUpdate.Priority)
		})
	}
}

func TestAccountPriorityRejectsInvalidWritesBeforeRepositoryAccess(t *testing.T) {
	for _, priority := range []int{MinAccountPriority - 1, MaxAccountPriority + 1} {
		t.Run(fmt.Sprint(priority), func(t *testing.T) {
			svc := &adminServiceImpl{}
			basic := &AccountService{}
			writes := map[string]func() error{
				"create": func() error {
					_, err := svc.CreateAccount(t.Context(), &CreateAccountInput{Priority: priority})
					return err
				},
				"update": func() error {
					_, err := svc.UpdateAccount(t.Context(), 7, &UpdateAccountInput{Priority: &priority})
					return err
				},
				"bulk": func() error {
					_, err := svc.BulkUpdateAccounts(t.Context(), &BulkUpdateAccountsInput{AccountIDs: []int64{7}, Priority: &priority})
					return err
				},
				"shadow": func() error {
					_, err := svc.CreateShadow(t.Context(), 7, ShadowOptions{Priority: &priority})
					return err
				},
				"basic create": func() error {
					_, err := basic.Create(t.Context(), CreateAccountRequest{Priority: priority})
					return err
				},
				"basic update": func() error {
					_, err := basic.Update(t.Context(), 7, UpdateAccountRequest{Priority: &priority})
					return err
				},
			}
			for name, write := range writes {
				t.Run(name, func(t *testing.T) {
					err := write()
					require.Equal(t, 400, infraerrors.Code(err))
					requireApplicationErrorReason(t, err, "ACCOUNT_PRIORITY_INVALID")
				})
			}
		})
	}
}

func TestAccountPriorityOmittedUpdatePreservesCurrentValue(t *testing.T) {
	repo := &accountRepoStubForBulkUpdate{getByIDAccounts: map[int64]*Account{
		7: {ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Priority: -3},
	}}
	svc := &adminServiceImpl{accountRepo: repo}
	updated, err := svc.UpdateAccount(t.Context(), 7, &UpdateAccountInput{Name: "renamed"})
	require.NoError(t, err)
	require.Equal(t, -3, updated.Priority)
	_, err = svc.BulkUpdateAccounts(t.Context(), &BulkUpdateAccountsInput{AccountIDs: []int64{7}, Name: "renamed"})
	require.NoError(t, err)
	require.Nil(t, repo.lastBulkUpdate.Priority)
}

func TestOAuthAutoConfigSignedPriority(t *testing.T) {
	for _, priority := range []int{MinAccountPriority, -1, 0, MaxAccountPriority} {
		t.Run(fmt.Sprint(priority), func(t *testing.T) {
			cfg := DefaultOAuthAutoConfig()
			cfg.Enabled, cfg.GroupIDs, cfg.Priority = true, []int64{2}, priority
			raw, err := json.Marshal(cfg)
			require.NoError(t, err)
			svc := &adminServiceImpl{
				settingService: NewSettingService(&accountOpsSettingsStub{raw: string(raw)}, nil),
				groupRepo:      autoConfigGroups{group: &Group{ID: 2, Platform: PlatformOpenAI, Status: StatusActive}},
			}
			input := &CreateAccountInput{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Priority: 50}
			require.NoError(t, svc.ApplyOAuthAutoConfig(context.Background(), input))
			require.Equal(t, priority, input.Priority)
		})
	}
	for _, priority := range []int{MinAccountPriority - 1, MaxAccountPriority + 1} {
		cfg := DefaultOAuthAutoConfig()
		cfg.Priority = priority
		require.Equal(t, 400, infraerrors.Code(ValidateOAuthAutoConfig(cfg)))
	}
}

func TestAccountPrioritySignedScheduling(t *testing.T) {
	accounts := []*Account{{ID: 1, Priority: 1}, {ID: 2, Priority: 0}, {ID: 3, Priority: -1}, {ID: 4, Priority: MinAccountPriority}}
	sortAccountsByPriorityAndLastUsed(accounts, false)
	require.Equal(t, []int64{4, 3, 2, 1}, []int64{accounts[0].ID, accounts[1].ID, accounts[2].ID, accounts[3].ID})
	filtered := filterByMinPriority([]accountWithLoad{{account: accounts[3]}, {account: accounts[1]}, {account: accounts[0]}})
	require.Len(t, filtered, 1)
	require.Equal(t, int64(4), filtered[0].account.ID)

	// Exercise the actual advanced scheduler with negative, zero and positive candidates.
	pool := make([]Account, len(accounts))
	for i, account := range accounts {
		pool[i] = *account
		pool[i].Platform, pool[i].Type, pool[i].Status = PlatformOpenAI, AccountTypeAPIKey, StatusActive
		pool[i].Schedulable, pool[i].Concurrency = true, 1
	}
	cfg := newSchedulerTestSubscriptionPriorityConfig()
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Load = 0
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Queue = 0
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: pool},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}
	selected, _, err := svc.SelectAccountWithScheduler(t.Context(), nil, "", "signed-priority", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.Equal(t, int64(4), selected.Account.ID)
	if selected.ReleaseFunc != nil {
		selected.ReleaseFunc()
	}
}

func TestCRSSyncSignedPriority(t *testing.T) {
	for _, collection := range []string{"openaiOAuthAccounts", "openaiResponsesAccounts"} {
		for _, tt := range []struct {
			name     string
			priority *int
			want     int
		}{
			{"omitted", nil, 50},
			{"minimum", ptr(-9999), -9999},
			{"negative", ptr(-1), -1},
			{"zero", ptr(0), 0},
			{"maximum", ptr(9999), 9999},
			{"below minimum", ptr(-10000), 50},
			{"above maximum", ptr(10000), 50},
		} {
			t.Run(collection+"/"+tt.name, func(t *testing.T) {
				repo := newCRSLongContextAccountRepo()
				source := crsOpenAILongContextSource{
					collection: collection, credentials: map[string]any{"access_token": "test", "api_key": "test"}, priority: tt.priority,
				}
				result := runCRSOpenAILongContextSync(t, repo, source)
				require.Len(t, result.Items, 1)
				require.Equal(t, "created", result.Items[0].Action)
				require.Equal(t, tt.want, repo.accounts["crs-openai-1"].Priority)
				repo.accounts["crs-openai-1"].Priority = 50
				result = runCRSOpenAILongContextSync(t, repo, source)
				require.Len(t, result.Items, 1)
				require.Equal(t, "updated", result.Items[0].Action)
				require.Equal(t, tt.want, repo.accounts["crs-openai-1"].Priority)
			})
		}
	}
}
