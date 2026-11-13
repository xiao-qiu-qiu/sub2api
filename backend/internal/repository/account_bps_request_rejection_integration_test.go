//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func newBPSRequestRejectionFixture(t *testing.T) (*accountRepository, *service.Account) {
	t.Helper()
	repo := newAccountRepositoryWithSQL(testEntClient(t), integrationDB, NewSchedulerCache(testRedis(t)))
	a := newExcelBPSAutoDisableAccount()
	a.Name = "bps-request-rejection-test"
	require.NoError(t, repo.Create(t.Context(), a))
	t.Cleanup(func() {
		_, err := integrationDB.Exec("DELETE FROM accounts WHERE id=$1", a.ID)
		require.NoError(t, err)
	})
	a, err := repo.GetByID(t.Context(), a.ID)
	require.NoError(t, err)
	return repo, a
}

func TestBPSRequestRejectionLifecycleAndSchedulerVisibility(t *testing.T) {
	ctx := t.Context()
	repo, observed := newBPSRequestRejectionFixture(t)
	var version string
	require.NoError(t, integrationDB.QueryRow("SELECT version()").Scan(&version))
	t.Log(version)
	bucket := service.SchedulerBucket{Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	token, err := repo.schedulerCache.CaptureBucketWriteToken(ctx, bucket)
	require.NoError(t, err)
	require.NoError(t, repo.schedulerCache.SetSnapshot(ctx, bucket, token, []service.Account{*observed}))
	_, hit, err := repo.schedulerCache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	require.True(t, hit)
	changed, err := repo.MarkBPSRequestRejected(service.WithManualAccountTest(ctx), observed, "usage_policy")
	require.NoError(t, err)
	require.False(t, changed)
	changed, err = repo.MarkBPSRequestRejected(ctx, observed, "usage_policy")
	require.NoError(t, err)
	require.True(t, changed)
	require.False(t, observed.IsSchedulable(), "already-held snapshots must be fenced immediately")
	require.True(t, observed.IsSchedulableWithContext(service.WithManualAccountTest(ctx)))
	current, err := repo.GetByID(ctx, observed.ID)
	require.NoError(t, err)
	marker, ok := current.Extra[service.BPSRequestRejectedExtraKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, marker["blocked"])
	require.Equal(t, "usage_policy", marker["reason"])
	require.Len(t, marker, 3)
	_, err = time.Parse(time.RFC3339Nano, marker["blocked_at"].(string))
	require.NoError(t, err)
	require.Nil(t, current.RateLimitResetAt, "indefinite rejection must not fabricate a deadline")
	sticky, err := repo.schedulerCache.GetAccount(ctx, observed.ID)
	require.NoError(t, err)
	require.Equal(t, marker, sticky.Extra[service.BPSRequestRejectedExtraKey])
	snapshot, hit, err := repo.schedulerCache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	require.True(t, hit)
	require.Len(t, snapshot, 1)
	require.Equal(t, marker, snapshot[0].Extra[service.BPSRequestRejectedExtraKey], "warm candidate memo must refresh")
	require.False(t, snapshot[0].IsSchedulable())
	for _, query := range []func(context.Context) ([]service.Account, error){
		repo.ListSchedulable,
		func(ctx context.Context) ([]service.Account, error) {
			return repo.ListSchedulableByPlatform(ctx, service.PlatformOpenAI)
		},
		func(ctx context.Context) ([]service.Account, error) {
			return repo.ListSchedulableByPlatforms(ctx, []string{service.PlatformOpenAI})
		},
		func(ctx context.Context) ([]service.Account, error) {
			return repo.ListSchedulableUngroupedByPlatform(ctx, service.PlatformOpenAI)
		},
		func(ctx context.Context) ([]service.Account, error) {
			return repo.ListSchedulableUngroupedByPlatforms(ctx, []string{service.PlatformOpenAI})
		},
		func(ctx context.Context) ([]service.Account, error) {
			return repo.ListModelAvailabilityCandidates(ctx, nil, []string{service.PlatformOpenAI}, true)
		},
	} {
		accounts, err := query(ctx)
		require.NoError(t, err)
		for _, candidate := range accounts {
			require.NotEqual(t, observed.ID, candidate.ID)
		}
	}
	// Automatic recovery and refreshed credentials must preserve the block.
	require.NoError(t, repo.ClearRateLimit(ctx, observed.ID))
	require.NoError(t, repo.ClearError(ctx, observed.ID))
	require.NoError(t, repo.ClearTempUnschedulable(ctx, observed.ID))
	require.NoError(t, repo.UpdateExtra(ctx, observed.ID, map[string]any{
		service.BPSRequestRejectedExtraKey: nil, service.BPSRequestRejectedGenerationExtraKey: "forged", "unrelated": true,
	}))
	observed.Credentials["access_token"] = "refreshed-test-token"
	require.NoError(t, repo.Update(ctx, observed))
	current, err = repo.GetByID(ctx, observed.ID)
	require.NoError(t, err)
	require.Equal(t, marker, current.Extra[service.BPSRequestRejectedExtraKey])
	require.NotContains(t, current.Extra, service.BPSRequestRejectedGenerationExtraKey)
	beforeReset := *current
	require.NoError(t, repo.SetSchedulable(ctx, current.ID, false))
	isolation := map[string]any{"isolated-model": map[string]any{"reason": "quality_rule:7"}}
	_, err = integrationDB.Exec(
		`UPDATE accounts SET extra=extra || jsonb_build_object($2::text,$3::jsonb) WHERE id=$1`,
		current.ID, service.QualityModelIsolationKey, `{"isolated-model":{"reason":"quality_rule:7"}}`)
	require.NoError(t, err)
	require.NoError(t, repo.ResetBPSRequestRejected(ctx, current.ID))
	current, err = repo.GetByID(ctx, current.ID)
	require.NoError(t, err)
	require.NotContains(t, current.Extra, service.BPSRequestRejectedExtraKey)
	require.NotEmpty(t, current.Extra[service.BPSRequestRejectedGenerationExtraKey])
	require.False(t, current.Schedulable)
	require.Equal(t, isolation, current.Extra[service.QualityModelIsolationKey])
	require.False(t, beforeReset.IsBPSRequestRejected(), "reset must fence old cached blocked snapshots")
	changed, err = repo.MarkBPSRequestRejected(ctx, &beforeReset, "invalid_request")
	require.NoError(t, err)
	require.False(t, changed, "old in-flight errors must not undo reset")
	changed, err = repo.MarkBPSRequestRejected(ctx, current, "private upstream text with credentials")
	require.NoError(t, err)
	require.True(t, changed, "a new rejection after reset must block again")
	current, err = repo.GetByID(ctx, current.ID)
	require.NoError(t, err)
	require.Equal(t, "upstream_rejected", current.Extra[service.BPSRequestRejectedExtraKey].(map[string]any)["reason"])
}

func TestBPSRequestRejectionStaleIdentityAndResetCAS(t *testing.T) {
	for _, mutation := range []struct{ name, query string }{
		{"credentials", `UPDATE accounts SET credentials='{"access_token":"replacement"}' WHERE id=$1`},
		{"platform", `UPDATE accounts SET platform='anthropic' WHERE id=$1`},
		{"type", `UPDATE accounts SET type='apikey' WHERE id=$1`},
		{"deleted", `UPDATE accounts SET deleted_at=NOW() WHERE id=$1`},
		{"account_generation", `UPDATE accounts SET created_at=created_at+interval '1 second' WHERE id=$1`},
		{"managed_connections", `WITH policy AS (INSERT INTO openai_auth_connection_policies(account_id,enabled) VALUES($1,true) RETURNING account_id)
			INSERT INTO openai_auth_connections(id,account_id,kind,label,status)
			SELECT gen_random_uuid(),account_id,'bps','test managed grant','active' FROM policy`},
		{"reset_without_existing_block", ""},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			repo, observed := newBPSRequestRejectionFixture(t)
			if mutation.query == "" {
				require.NoError(t, repo.ResetBPSRequestRejected(t.Context(), observed.ID))
			} else {
				_, err := integrationDB.Exec(mutation.query, observed.ID)
				require.NoError(t, err)
			}
			changed, err := repo.MarkBPSRequestRejected(t.Context(), observed, "usage_policy")
			require.NoError(t, err)
			require.False(t, changed)
			var blocked bool
			require.NoError(t, integrationDB.QueryRow(`SELECT extra ? 'openai_bps_request_rejected' FROM accounts WHERE id=$1`, observed.ID).Scan(&blocked))
			require.False(t, blocked)
		})
	}
}

func TestBPSRequestRejectionAtomicOutboxAndOuterRollback(t *testing.T) {
	repo, observed := newBPSRequestRejectionFixture(t)
	broken := *repo
	broken.sql = &failAtomicSchedulerOutboxSQLExecutor{sqlExecutor: integrationDB}
	changed, err := broken.MarkBPSRequestRejected(t.Context(), observed, "usage_policy")
	require.Error(t, err)
	require.False(t, changed)
	current, err := repo.GetByID(t.Context(), observed.ID)
	require.NoError(t, err)
	require.NotContains(t, current.Extra, service.BPSRequestRejectedExtraKey)
	tx := testEntTx(t)
	changed, err = repo.MarkBPSRequestRejected(dbent.NewTxContext(t.Context(), tx), observed, "usage_policy")
	require.NoError(t, err)
	require.True(t, changed)
	require.True(t, observed.IsSchedulable(), "uncommitted state must not escape into process gates")
	require.NoError(t, tx.Rollback())
	current, err = repo.GetByID(t.Context(), observed.ID)
	require.NoError(t, err)
	require.NotContains(t, current.Extra, service.BPSRequestRejectedExtraKey)
	canceledCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	canceling := *repo
	canceling.sql = &cancelAfterAtomicMutationSQLExecutor{sqlExecutor: integrationDB, cancel: cancel}
	changed, err = canceling.MarkBPSRequestRejected(canceledCtx, observed, "usage_policy")
	require.NoError(t, err)
	require.True(t, changed)
	require.ErrorIs(t, canceledCtx.Err(), context.Canceled)
	sticky, err := repo.schedulerCache.GetAccount(t.Context(), observed.ID)
	require.NoError(t, err)
	require.Equal(t, true, sticky.Extra[service.BPSRequestRejectedExtraKey].(map[string]any)["blocked"])
}

func TestBPSRequestRejectionResetTransactionStartedBeforeMark(t *testing.T) {
	repo, observed := newBPSRequestRejectionFixture(t)
	tx := testEntTx(t)
	_, err := tx.Client().ExecContext(t.Context(), "SELECT CURRENT_TIMESTAMP")
	require.NoError(t, err)
	changed, err := repo.MarkBPSRequestRejected(t.Context(), observed, "usage_policy")
	require.NoError(t, err)
	require.True(t, changed)
	blockedSnapshot, err := repo.GetByID(t.Context(), observed.ID)
	require.NoError(t, err)
	require.NoError(t, repo.ResetBPSRequestRejected(dbent.NewTxContext(t.Context(), tx), observed.ID))
	require.True(t, blockedSnapshot.IsBPSRequestRejected(), "reset is invisible before commit")
	require.NoError(t, tx.Commit())
	require.False(t, blockedSnapshot.IsBPSRequestRejected(), "committing an older reset transaction must clear the newer mark")
	changed, err = repo.MarkBPSRequestRejected(t.Context(), observed, "usage_policy")
	require.NoError(t, err)
	require.False(t, changed)
}

func TestBPSRequestRejectionManagedGrantRevisionAndReset(t *testing.T) {
	repo, observed := newBPSRequestRejectionFixture(t)
	// Runtime grant projection uses OAuth credentials even when the root is an API key account.
	_, err := integrationDB.Exec("UPDATE accounts SET type='apikey' WHERE id=$1", observed.ID)
	require.NoError(t, err)
	_, err = integrationDB.Exec("INSERT INTO openai_auth_connection_policies(account_id,enabled) VALUES($1,true)", observed.ID)
	require.NoError(t, err)
	var connectionID string
	require.NoError(t, integrationDB.QueryRow(`INSERT INTO openai_auth_connections(id,account_id,kind,label,status)
		VALUES(gen_random_uuid(),$1,'bps','test grant','active') RETURNING id::text`, observed.ID).Scan(&connectionID))
	observed.AuthConnectionID, observed.AuthConnectionRevision = connectionID, 1
	observed.Credentials = map[string]any{"access_token": "projected-grant-token"}
	changed, err := repo.MarkBPSRequestRejected(t.Context(), observed, "invalid_request")
	require.NoError(t, err)
	require.True(t, changed, "managed grants use the immutable grant revision, not projected root credentials")
	require.NoError(t, repo.ResetBPSRequestRejected(t.Context(), observed.ID))
	changed, err = repo.MarkBPSRequestRejected(t.Context(), observed, "invalid_request")
	require.NoError(t, err)
	require.False(t, changed, "managed grant errors must obey the same reset fence")
	fresh, err := repo.GetByID(t.Context(), observed.ID)
	require.NoError(t, err)
	fresh.Type = service.AccountTypeOAuth
	fresh.AuthConnectionID, fresh.AuthConnectionRevision = connectionID, 1
	_, err = integrationDB.Exec("UPDATE openai_auth_connections SET revision=revision+1 WHERE id=$1", connectionID)
	require.NoError(t, err)
	changed, err = repo.MarkBPSRequestRejected(t.Context(), fresh, "usage_policy")
	require.NoError(t, err)
	require.False(t, changed)
	fresh.AuthConnectionRevision = 2
	changed, err = repo.MarkBPSRequestRejected(t.Context(), fresh, "usage_policy")
	require.NoError(t, err)
	require.True(t, changed)
	require.NoError(t, repo.ResetBPSRequestRejected(t.Context(), observed.ID))
	fresh, err = repo.GetByID(t.Context(), observed.ID)
	require.NoError(t, err)
	fresh.AuthConnectionID, fresh.AuthConnectionRevision = connectionID, 2
	fresh.Type = service.AccountTypeOAuth
	_, err = integrationDB.Exec("DELETE FROM openai_auth_connection_policies WHERE account_id=$1", observed.ID)
	require.NoError(t, err)
	changed, err = repo.MarkBPSRequestRejected(t.Context(), fresh, "usage_policy")
	require.NoError(t, err)
	require.False(t, changed, "retired managed credentials must not block the restored legacy route")
}

func TestBPSRequestRejectionGroupedCandidatesAndCapacity(t *testing.T) {
	repo, observed := newBPSRequestRejectionFixture(t)
	group := mustCreateGroup(t, testEntClient(t), &service.Group{Name: "bps rejection group", Platform: service.PlatformOpenAI})
	t.Cleanup(func() {
		_, err := integrationDB.Exec("DELETE FROM groups WHERE id=$1", group.ID)
		require.NoError(t, err)
	})
	require.NoError(t, repo.BindGroups(t.Context(), observed.ID, []int64{group.ID}))
	changed, err := repo.MarkBPSRequestRejected(t.Context(), observed, "usage_policy")
	require.NoError(t, err)
	require.True(t, changed)
	for _, query := range []func() ([]service.Account, error){
		func() ([]service.Account, error) { return repo.ListSchedulableByGroupID(t.Context(), group.ID) },
		func() ([]service.Account, error) {
			return repo.ListSchedulableByGroupIDAndPlatform(t.Context(), group.ID, service.PlatformOpenAI)
		},
		func() ([]service.Account, error) {
			return repo.ListSchedulableByGroupIDAndPlatforms(t.Context(), group.ID, []string{service.PlatformOpenAI})
		},
		func() ([]service.Account, error) {
			return repo.ListModelAvailabilityCandidates(t.Context(), &group.ID, []string{service.PlatformOpenAI}, true)
		},
	} {
		accounts, err := query()
		require.NoError(t, err)
		require.Empty(t, accounts)
	}
	capacity, err := repo.ListSchedulableCapacityByGroupIDs(t.Context(), []int64{group.ID})
	require.NoError(t, err)
	require.Empty(t, capacity)
}
