package service

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestOAuthCreationConfigManualPriorityAndLegacyCompatibility(t *testing.T) {
	for _, preserve := range []bool{false, true} {
		c := DefaultOAuthAutoConfig()
		c.Enabled = true
		c.GroupIDs = []int64{2}
		c.Concurrency = 99
		c.Priority = 50
		c.QualityRule = initialQualityRuleFixture()
		raw, err := json.Marshal(c)
		require.NoError(t, err)
		s := &adminServiceImpl{settingService: NewSettingService(&accountOpsSettingsStub{raw: string(raw)}, nil), groupRepo: autoConfigGroups{group: &Group{ID: 2, Platform: PlatformOpenAI, Status: StatusActive}}}
		rate, groupRate := 0.0, 0.0
		in := &CreateAccountInput{PreserveCreationConfig: preserve, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Priority: -40, Concurrency: 7, RateMultiplier: &rate, GroupRateMultiplier: &groupRate, GroupIDs: []int64{},
			Extra: map[string]any{AccountCostMultiplierExtraKey: 0, "auto_config_initial_revision": "forged", OpenAITwoFAAdmissionExtraKey: "forged"}, Credentials: map[string]any{"model_mapping": map[string]any{"mine": "target"}}}
		require.NoError(t, s.ApplyOAuthAutoConfig(t.Context(), in))
		require.NotNil(t, in.InitialQualityPlan)
		require.NotContains(t, in.Extra, OpenAITwoFAAdmissionExtraKey)
		require.Zero(t, *in.RateMultiplier)
		require.Zero(t, *in.GroupRateMultiplier)
		if preserve {
			require.Equal(t, -40, in.Priority)
			require.Equal(t, 7, in.Concurrency)
			require.Nil(t, in.LoadFactor)
			require.Empty(t, in.GroupIDs)
			require.True(t, in.SkipDefaultGroupBind)
			require.EqualValues(t, 0, in.Extra[AccountCostMultiplierExtraKey])
			require.NotContains(t, in.Extra, "auto_config_initial_revision")
			require.Equal(t, map[string]any{"mine": "target"}, in.Credentials["model_mapping"])
		} else {
			require.Equal(t, c.Priority, in.Priority)
			require.Equal(t, c.Concurrency, in.Concurrency)
			require.Equal(t, c.LoadFactor, *in.LoadFactor)
			require.Equal(t, c.GroupIDs, in.GroupIDs)
			require.Equal(t, c.CostMultiplier, in.Extra[AccountCostMultiplierExtraKey])
			require.Contains(t, in.Extra, "auto_config_initial_revision")
			require.NotEqual(t, map[string]any{"mine": "target"}, in.Credentials["model_mapping"])
		}
	}
}

func TestOAuthCreationConfigPreservesTwoFAAdmission(t *testing.T) {
	c := DefaultOAuthAutoConfig()
	c.Enabled = true
	c.GroupIDs = []int64{2}
	c.OpenAITwoFAAdmission.Enabled = true
	c.OpenAITwoFAAdmission.IsolationGroupID = 44
	c.QualityRule = initialQualityRuleFixture()
	raw, err := json.Marshal(c)
	require.NoError(t, err)
	repo := &autoConfigAccountRepo{}
	s := &adminServiceImpl{accountRepo: repo, settingService: NewSettingService(&accountOpsSettingsStub{raw: string(raw)}, nil), groupRepo: autoConfigGroups{group: &Group{ID: 44, Platform: PlatformOpenAI, Status: StatusActive}}}
	for _, source := range []bool{false, true} {
		lf := 5
		in := &CreateAccountInput{PreserveCreationConfig: true, InitialOpenAITwoFA: source, Name: "manual", Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 7, Priority: -20, LoadFactor: &lf, GroupIDs: []int64{}, Credentials: map[string]any{"access_token": "fixture"}}
		a, err := s.CreateAccount(t.Context(), in)
		require.NoError(t, err)
		require.NotNil(t, a.InitialQualityPlan)
		require.Equal(t, -20, a.Priority)
		if source {
			require.Equal(t, 1, a.Concurrency)
			require.False(t, a.Schedulable)
			require.NotNil(t, a.InitialAdmissionPlan)
			require.Contains(t, a.Extra, OpenAITwoFAAdmissionExtraKey)
		} else {
			require.Equal(t, 7, a.Concurrency)
			require.True(t, a.Schedulable)
			require.Nil(t, a.InitialAdmissionPlan)
		}
	}
}

func TestOAuthCreationConfigObserverCannotEscapeScope(t *testing.T) {
	s := &adminServiceImpl{}
	for _, groups := range [][]int64{nil, {2}, {1, 2}} {
		_, err := s.CreateAccount(WithObserverScope(t.Context(), []int64{1}), &CreateAccountInput{PreserveCreationConfig: true, Platform: PlatformOpenAI, Type: AccountTypeOAuth, GroupIDs: groups})
		require.ErrorIs(t, err, ErrObserverScope)
	}
}

func TestOAuthCreationConfigScopedImportCannotChangeGroupBilling(t *testing.T) {
	ctx := adminaccess.WithPrincipal(t.Context(), &adminaccess.Principal{Policy: adminaccess.Policy{Rules: []adminaccess.Rule{{
		Permissions: []string{"accounts.create", "accounts.credentials", "accounts.schedule"},
		GroupIDs:    []int64{1}, AccountTypes: []string{AccountTypeOAuth}, Platforms: []string{PlatformOpenAI},
	}}}})
	rate := 0.0
	_, err := (&adminServiceImpl{}).ScopedAdminCreateCodex(ctx, &CreateAccountInput{
		PreserveCreationConfig: true, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		GroupIDs: []int64{1}, Credentials: map[string]any{"access_token": "fixture"}, GroupRateMultiplier: &rate,
	})
	require.Equal(t, 403, infraerrors.Code(err))
	require.Equal(t, "SCOPED_CODEX_RATE", infraerrors.Reason(err))
}

func TestOAuthCreationConfigPATManualAndLegacy(t *testing.T) {
	c := DefaultOAuthAutoConfig()
	c.Enabled = true
	c.GroupIDs = []int64{2}
	c.Concurrency = 99
	c.Priority = 50
	c.QualityRule = initialQualityRuleFixture()
	raw, err := json.Marshal(c)
	require.NoError(t, err)
	for _, preserve := range []bool{false, true} {
		s := &adminServiceImpl{accountRepo: &autoConfigAccountRepo{},
			settingService: NewSettingService(&accountOpsSettingsStub{raw: string(raw)}, nil),
			groupRepo:      autoConfigGroups{group: &Group{ID: 2, Platform: PlatformOpenAI, Status: StatusActive}}}
		rate, groupRate := 0.0, 0.0
		a, err := s.CreateAccount(t.Context(), &CreateAccountInput{
			PreserveCreationConfig: preserve, Name: "preset PAT", Platform: PlatformOpenAI, Type: AccountTypeOAuth,
			Concurrency: 7, Priority: -17, RateMultiplier: &rate, GroupRateMultiplier: &groupRate, GroupIDs: []int64{},
			Credentials: map[string]any{"access_token": "at-preset-fixture", "auth_mode": OpenAIAuthModePersonalAccessToken, "model_mapping": map[string]any{"manual": "target"}},
			Extra:       map[string]any{AccountCostMultiplierExtraKey: 0},
		})
		require.NoError(t, err)
		require.True(t, a.IsOpenAIPersonalAccessToken())
		require.False(t, a.IsExcelBPSEnabled())
		require.NotNil(t, a.InitialQualityPlan)
		require.Nil(t, a.InitialAdmissionPlan)
		require.True(t, a.Schedulable)
		require.Zero(t, a.UserGroupRateMultiplier())
		require.Zero(t, a.BillingRateMultiplier())
		if preserve {
			require.Equal(t, -17, a.Priority)
			require.Equal(t, 7, a.Concurrency)
			require.Nil(t, a.LoadFactor)
			require.EqualValues(t, 0, a.Extra[AccountCostMultiplierExtraKey])
			require.Equal(t, map[string]any{"manual": "target"}, a.Credentials["model_mapping"])
			require.NotContains(t, a.Extra, "auto_config_initial_revision")
		} else {
			require.Equal(t, c.Priority, a.Priority)
			require.Equal(t, c.Concurrency, a.Concurrency)
			require.Equal(t, c.LoadFactor, *a.LoadFactor)
			require.Equal(t, c.CostMultiplier, a.Extra[AccountCostMultiplierExtraKey])
			require.NotEqual(t, map[string]any{"manual": "target"}, a.Credentials["model_mapping"])
		}
	}
}
