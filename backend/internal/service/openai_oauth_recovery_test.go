//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type oauthRecoveryTestRepo struct {
	AccountRepository
	mu       sync.Mutex
	accounts []Account
	errors   int
}

func (r *oauthRecoveryTestRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, a := range r.accounts {
		if a.ID == id {
			return &a, nil
		}
	}
	return nil, errors.New("missing account")
}

func (r *oauthRecoveryTestRepo) ListAllWithFilters(_ context.Context, _, _, _, _ string, groupID int64, _ string) ([]Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Account
	for _, a := range r.accounts {
		if groupID == 0 {
			out = append(out, a)
			continue
		}
		for _, id := range a.GroupIDs {
			if id == groupID {
				out = append(out, a)
			}
		}
	}
	return out, nil
}

func (r *oauthRecoveryTestRepo) SetError(_ context.Context, _ int64, _ string) error {
	r.mu.Lock()
	r.errors++
	r.mu.Unlock()
	return nil
}

type oauthRecoveryTestGuard struct {
	mu         sync.Mutex
	repo       *oauthRecoveryTestRepo
	task       OpenAIOAuthReauthTask
	creates    int
	verifies   int
	seenToken  string
	requestErr error
	verifyErr  error
}

func (g *oauthRecoveryTestGuard) RequestImmediateReauth(_ context.Context, account *Account) (*OpenAIOAuthReauthTask, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.creates++
	g.seenToken = account.GetCredential("access_token")
	if g.requestErr != nil {
		return nil, g.requestErr
	}
	g.repo.mu.Lock()
	for i := range g.repo.accounts {
		if g.repo.accounts[i].ID == account.ID {
			if g.repo.accounts[i].GetCredential("access_token") != g.seenToken {
				g.repo.mu.Unlock()
				return nil, nil
			}
			g.repo.accounts[i].Status = StatusError
		}
	}
	g.repo.mu.Unlock()
	task := g.task
	return &task, nil
}

func (g *oauthRecoveryTestGuard) GetImmediateReauthTask(_ context.Context, accountID int64) (*OpenAIOAuthReauthTask, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if accountID != g.task.AccountID || g.task.Status == OpenAIOAuthReauthStatusSucceeded || g.task.Status == OpenAIOAuthReauthStatusFailed {
		return nil, nil
	}
	task := g.task
	return &task, nil
}

func (g *oauthRecoveryTestGuard) GetImmediateReauthTaskByID(_ context.Context, _ int64) (*OpenAIOAuthReauthTask, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	task := g.task
	return &task, nil
}

func (g *oauthRecoveryTestGuard) VerifyImmediateReauth(context.Context, int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.verifies++
	return g.verifyErr
}

func (g *oauthRecoveryTestGuard) complete(status string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if status == OpenAIOAuthReauthStatusSucceeded {
		g.repo.mu.Lock()
		for i := range g.repo.accounts {
			if g.repo.accounts[i].ID == g.task.AccountID {
				g.repo.accounts[i].Status = StatusActive
				g.repo.accounts[i].Credentials = map[string]any{"access_token": "new-test-token", "refresh_token": "new-test-refresh"}
			}
		}
		g.repo.mu.Unlock()
	}
	g.task.Status = status
}

func newOAuthRecoveryFixture(t *testing.T) (*OpenAIGatewayService, *oauthRecoveryTestGuard, *Account) {
	t.Helper()
	account := Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true,
		GroupIDs: []int64{7}, Credentials: map[string]any{"access_token": "old-test-token", "refresh_token": "test-refresh"}}
	repo := &oauthRecoveryTestRepo{accounts: []Account{account}}
	guard := &oauthRecoveryTestGuard{repo: repo, task: OpenAIOAuthReauthTask{ID: 20, AccountID: 1, Status: OpenAIOAuthReauthStatusRunning}}
	rl := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	svc := &OpenAIGatewayService{accountRepo: repo, rateLimitService: rl}
	svc.SetOpenAIOAuthRecovery(guard)
	svc.oauthRecovery.pollTime = time.Millisecond
	t.Cleanup(svc.oauthRecovery.stop)
	return svc, guard, &account
}

var oauthRecoveryTest401 = []byte(`{"error":{"code":"token_revoked"}}`)

func TestOAuthRecoveryConcurrent401AndNewRequestsShareOneTask(t *testing.T) {
	svc, guard, account := newOAuthRecoveryFixture(t)
	const count = 24
	results := make(chan error, count)
	for i := 0; i < count; i++ {
		go func() {
			ctx := WithOpenAIOAuthRecoveryBudget(context.Background())
			if !svc.handleOpenAIAccountUpstreamError(ctx, account, 401, http.Header{}, oauthRecoveryTest401) {
				results <- errors.New("401 was not handled")
				return
			}
			handled, err := svc.RecoverOpenAIOAuth401(ctx, account, oauthRecoveryTest401)
			if !handled {
				err = errors.New("request did not join recovery")
			}
			results <- err
		}()
	}
	require.Eventually(t, func() bool { return svc.oauthRecovery.active(account.ID) }, time.Second, time.Millisecond)
	newRequest := make(chan error, 1)
	go func() {
		groupID := int64(7)
		waited, err := svc.WaitForOpenAIOAuthRecovery(WithOpenAIOAuthRecoveryBudget(context.Background()), &groupID, "gpt-test")
		if !waited {
			err = errors.New("new request did not wait")
		}
		newRequest <- err
	}()
	select {
	case err := <-results:
		t.Fatalf("request escaped before recovery: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	guard.complete(OpenAIOAuthReauthStatusSucceeded)
	for i := 0; i < count; i++ {
		require.NoError(t, <-results)
	}
	require.NoError(t, <-newRequest)
	guard.mu.Lock()
	require.Equal(t, 1, guard.creates)
	require.Equal(t, 1, guard.verifies)
	guard.mu.Unlock()
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account), "no late generic auth block may outlive recovery")
	guard.repo.mu.Lock()
	require.Zero(t, guard.repo.errors)
	guard.repo.mu.Unlock()
}

func TestOAuthRecoveryTimeoutAndCancellationDoNotCancelLogin(t *testing.T) {
	require.Equal(t, time.Minute, OpenAIOAuthRecoveryWait)
	for _, cancelClient := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "disconnect"}[cancelClient], func(t *testing.T) {
			svc, guard, account := newOAuthRecoveryFixture(t)
			svc.oauthRecovery.waitTime = 25 * time.Millisecond
			ctx, cancel := context.WithCancel(WithOpenAIOAuthRecoveryBudget(context.Background()))
			defer cancel()
			flight, started := svc.oauthRecovery.begin(ctx, account, oauthRecoveryTest401)
			require.True(t, started)
			if cancelClient {
				cancel()
			}
			handled, err := svc.RecoverOpenAIOAuth401(ctx, account, oauthRecoveryTest401)
			require.True(t, handled)
			if cancelClient {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.ErrorIs(t, err, ErrOAuthRecoveryTimeout)
			}
			require.True(t, svc.oauthRecovery.active(account.ID))
			guard.complete(OpenAIOAuthReauthStatusSucceeded)
			// The shared window has expired, so a new request cannot wait for
			// another minute. It may consume the outcome once verification ends.
			select {
			case <-flight.done:
			case <-time.After(time.Second):
				t.Fatal("recovery observer did not finish")
			}
			freshCtx := WithOpenAIOAuthRecoveryBudget(context.Background())
			_, err = svc.RecoverOpenAIOAuth401(freshCtx, account, oauthRecoveryTest401)
			require.NoError(t, err)
		})
	}
}

func TestOAuthRecoveryRejectsFailedLoginAndFailedFreshProbe(t *testing.T) {
	for _, verifyFail := range []bool{false, true} {
		t.Run(map[bool]string{false: "login", true: "probe"}[verifyFail], func(t *testing.T) {
			svc, guard, account := newOAuthRecoveryFixture(t)
			if verifyFail {
				guard.verifyErr = errors.New("credential rejected")
			}
			ctx := WithOpenAIOAuthRecoveryBudget(context.Background())
			_, handled := svc.oauthRecovery.begin(ctx, account, oauthRecoveryTest401)
			require.True(t, handled)
			status := OpenAIOAuthReauthStatusFailed
			if verifyFail {
				status = OpenAIOAuthReauthStatusSucceeded
			}
			guard.complete(status)
			_, err := svc.RecoverOpenAIOAuth401(ctx, account, oauthRecoveryTest401)
			require.ErrorIs(t, err, ErrOAuthRecoveryFailed)
		})
	}
}

func TestOAuthRecoveryDoesNotRepeatAfterSuccessfulRetry(t *testing.T) {
	svc, guard, account := newOAuthRecoveryFixture(t)
	ctx := WithOpenAIOAuthRecoveryBudget(context.Background())
	_, handled := svc.oauthRecovery.begin(ctx, account, oauthRecoveryTest401)
	require.True(t, handled)
	guard.complete(OpenAIOAuthReauthStatusSucceeded)
	_, err := svc.RecoverOpenAIOAuth401(ctx, account, oauthRecoveryTest401)
	require.NoError(t, err)
	current, _ := guard.repo.GetByID(ctx, account.ID)
	_, handled = svc.oauthRecovery.begin(ctx, current, oauthRecoveryTest401)
	require.False(t, handled)
	handled, err = svc.RecoverOpenAIOAuth401(ctx, current, oauthRecoveryTest401)
	require.True(t, handled)
	require.ErrorIs(t, err, ErrOAuthRecoveryFailed)
	guard.mu.Lock()
	require.Equal(t, 1, guard.creates)
	guard.mu.Unlock()
}

func TestOAuthRecoveryGroupIsolationAndRestartDiscovery(t *testing.T) {
	svc, guard, account := newOAuthRecoveryFixture(t)
	// A durable task already exists, but this process has no local flight.
	guard.repo.mu.Lock()
	guard.repo.accounts[0].Status = StatusError
	guard.repo.mu.Unlock()
	ctx := context.WithValue(WithOpenAIOAuthRecoveryBudget(context.Background()), oauthRecoveryDiscoveryKey{}, true)
	unrelated := int64(9)
	waited, err := svc.WaitForOpenAIOAuthRecovery(ctx, &unrelated, "gpt-test")
	require.NoError(t, err)
	require.False(t, waited)
	group := int64(7)
	result := make(chan error, 1)
	go func() {
		waited, err := svc.WaitForOpenAIOAuthRecovery(ctx, &group, "gpt-test")
		if !waited {
			err = errors.New("durable task not found")
		}
		result <- err
	}()
	require.Eventually(t, func() bool { return svc.oauthRecovery.active(account.ID) }, time.Second, time.Millisecond)
	guard.complete(OpenAIOAuthReauthStatusSucceeded)
	require.NoError(t, <-result)
	guard.mu.Lock()
	require.Zero(t, guard.creates)
	guard.mu.Unlock()
}

func TestOAuthRecoveryBudgetIsCumulativeAndHealthyAlternativeDoesNotWait(t *testing.T) {
	svc, guard, account := newOAuthRecoveryFixture(t)
	ctx := WithOpenAIOAuthRecoveryBudget(context.Background())
	_, handled := svc.oauthRecovery.begin(ctx, account, oauthRecoveryTest401)
	require.True(t, handled)
	healthy := *account
	healthy.ID = 2
	guard.repo.mu.Lock()
	guard.repo.accounts = append(guard.repo.accounts, healthy)
	guard.repo.mu.Unlock()
	groupID := int64(7)
	waited, err := svc.WaitForOpenAIOAuthRecovery(ctx, &groupID, "gpt-test")
	require.NoError(t, err)
	require.False(t, waited)
	b := recoveryBudget(ctx)
	first, cancel1 := b.waitContext(ctx, time.Second)
	second, cancel2 := b.waitContext(ctx, time.Hour)
	defer cancel1()
	defer cancel2()
	d1, _ := first.Deadline()
	d2, _ := second.Deadline()
	require.Equal(t, d1, d2, "later waits must not renew the minute")
}

func TestOAuthRecoverySchedulerMissWaitsOnlyForEligibleCandidates(t *testing.T) {
	svc, guard, account := newOAuthRecoveryFixture(t)
	ctx := WithOpenAIOAuthRecoveryBudget(context.Background())
	_, handled := svc.oauthRecovery.begin(ctx, account, oauthRecoveryTest401)
	require.True(t, handled)
	// This active account cannot serve Responses. Its presence must not hide
	// the only eligible OAuth account while that account is logging in.
	guard.repo.mu.Lock()
	guard.repo.accounts = append(guard.repo.accounts, Account{
		ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, GroupIDs: []int64{7},
		Extra: map[string]any{"openai_responses_supported": false},
	})
	guard.repo.mu.Unlock()
	ctx = context.WithValue(ctx, oauthRecoveryDiscoveryKey{}, true)
	groupID := int64(7)
	responses := oauthRecoverySelection{transport: OpenAIUpstreamTransportAny, capability: OpenAIEndpointCapabilityResponses}
	requestCtx := context.WithValue(ctx, oauthRecoverySelectionKey{}, responses)
	require.Len(t, svc.pendingOpenAIOAuthRecovery(requestCtx, &groupID, "gpt-test"), 1)

	excluded := responses
	excluded.excluded = map[int64]struct{}{account.ID: {}}
	require.Empty(t, svc.pendingOpenAIOAuthRecovery(context.WithValue(ctx, oauthRecoverySelectionKey{}, excluded), &groupID, "gpt-test"))
	embeddings := responses
	embeddings.capability = OpenAIEndpointCapabilityEmbeddings
	require.Empty(t, svc.pendingOpenAIOAuthRecovery(context.WithValue(ctx, oauthRecoverySelectionKey{}, embeddings), &groupID, "gpt-test"))
	privacyCtx := context.WithValue(requestCtx, openAIGroupPrivacyRequirementContextKey{}, openAIGroupPrivacyRequirement{groupID: groupID, required: true})
	require.Empty(t, svc.pendingOpenAIOAuthRecovery(privacyCtx, &groupID, "gpt-test"))
	guard.repo.mu.Lock()
	guard.repo.accounts[0].Extra = map[string]any{"openai_compact_mode": OpenAICompactModeForceOff}
	guard.repo.mu.Unlock()
	compact := responses
	compact.requireCompact = true
	require.Empty(t, svc.pendingOpenAIOAuthRecovery(context.WithValue(ctx, oauthRecoverySelectionKey{}, compact), &groupID, "gpt-test"))
	preAdmissionCtx := WithOpenAIForwardModel(WithOpenAIOAuthRecoveryBudget(context.Background()), "gpt-test", true)
	require.Empty(t, svc.pendingOpenAIOAuthRecovery(preAdmissionCtx, &groupID, "gpt-test"))

	guard.complete(OpenAIOAuthReauthStatusSucceeded)
	waited, err := svc.WaitForOpenAIOAuthRecovery(requestCtx, &groupID, "gpt-test")
	require.NoError(t, err)
	// The watcher may already have completed, in which case normal selection
	// proceeds immediately. Either path must leave the account ready to serve.
	if !waited {
		require.Eventually(t, func() bool { return !svc.oauthRecovery.active(account.ID) }, time.Second, time.Millisecond)
	}
	fresh, err := guard.repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.True(t, fresh.IsSchedulable())
}

func TestOAuthRecoveryKeepsFailureObservedBeforeQueueCleanup(t *testing.T) {
	svc, guard, account := newOAuthRecoveryFixture(t)
	ctx := WithOpenAIOAuthRecoveryBudget(context.Background())
	flight, handled := svc.oauthRecovery.begin(ctx, account, oauthRecoveryTest401)
	require.True(t, handled)
	groupID := int64(7)
	require.True(t, svc.HasPendingOpenAIOAuthRecovery(ctx, &groupID, "gpt-test"))
	// The worker finishes while the request fences its old queue entry. The
	// subsequent recovery wait must report that failure, even with no live job.
	guard.complete(OpenAIOAuthReauthStatusFailed)
	select {
	case <-flight.done:
	case <-time.After(time.Second):
		t.Fatal("recovery observer did not finish")
	}
	require.False(t, svc.oauthRecovery.active(account.ID))
	require.True(t, svc.HasPendingOpenAIOAuthRecovery(ctx, &groupID, "gpt-test"))
	waited, err := svc.WaitForOpenAIOAuthRecovery(ctx, &groupID, "gpt-test")
	require.True(t, waited)
	require.ErrorIs(t, err, ErrOAuthRecoveryFailed)
}

func TestOAuthRecoveryPolicyAndBackgroundProbesDoNotTriggerLogin(t *testing.T) {
	svc, guard, account := newOAuthRecoveryFixture(t)
	for _, body := range [][]byte{
		[]byte(`{"error":{"code":"account_deactivated"}}`),
		[]byte(`{"error":{"code":"phone_verification_required"}}`),
	} {
		_, handled := svc.oauthRecovery.begin(context.Background(), account, body)
		require.False(t, handled)
	}
	// An ordinary inspection retains its threshold and never enters event mode.
	require.True(t, svc.rateLimitService.HandleUpstreamError(context.Background(), account, 401, nil, oauthRecoveryTest401))
	guard.mu.Lock()
	require.Zero(t, guard.creates)
	guard.mu.Unlock()
}

func TestOAuthRecoveryFreshModelsProbeBypassesCache(t *testing.T) {
	_, calls := newCodexModelsOAuthCacheServer(t, `{"models":[{"slug":"test-model"}]}`)
	svc := &OpenAIGatewayService{}
	account := newCodexModelsTestAccount()
	_, err := svc.FetchOpenAIModelsList(context.Background(), account)
	require.NoError(t, err)
	_, err = svc.FetchOpenAIModelsList(context.Background(), account)
	require.NoError(t, err)
	require.EqualValues(t, 1, calls.Load())
	_, err = svc.FetchOpenAIRecoveryModelsList(context.Background(), account)
	require.NoError(t, err)
	require.EqualValues(t, 2, calls.Load(), "recovery must make a fresh upstream request")
}

func TestOAuthRecoveryUsesActualCachedTokenAndClearsItBeforeReplay(t *testing.T) {
	for _, staleCache := range []bool{false, true} {
		t.Run(map[bool]string{false: "refreshed_token", true: "obsolete_cached_token"}[staleCache], func(t *testing.T) {
			svc, guard, snapshot := newOAuthRecoveryFixture(t)
			currentToken := "refreshed-test-token"
			guard.repo.mu.Lock()
			guard.repo.accounts[0].Credentials = map[string]any{"access_token": currentToken, "refresh_token": "refreshed-test-refresh"}
			guard.repo.mu.Unlock()
			cachedToken := currentToken
			if staleCache {
				cachedToken = snapshot.GetCredential("access_token")
			}
			cache := newOpenAITokenCacheStub()
			cache.tokens[OpenAITokenCacheKey(snapshot)] = cachedToken
			svc.openAITokenProvider = &OpenAITokenProvider{tokenCache: cache, accountRepo: guard.repo}
			ctx := WithOpenAIOAuthRecoveryBudget(context.Background())
			token, _, err := svc.GetAccessToken(ctx, snapshot)
			require.NoError(t, err)
			require.Equal(t, cachedToken, token)
			require.True(t, svc.handleOpenAIAccountUpstreamError(ctx, snapshot, 401, nil, oauthRecoveryTest401))
			guard.mu.Lock()
			require.Equal(t, cachedToken, guard.seenToken)
			guard.mu.Unlock()
			if !staleCache {
				guard.complete(OpenAIOAuthReauthStatusSucceeded)
			}
			handled, err := svc.RecoverOpenAIOAuth401(ctx, snapshot, oauthRecoveryTest401)
			require.True(t, handled)
			require.NoError(t, err)
			cached, err := cache.GetAccessToken(ctx, OpenAITokenCacheKey(snapshot))
			require.NoError(t, err)
			require.Empty(t, cached)
		})
	}
}

func TestOAuthRecoveryExactlyOneMinuteAndFreshTurnBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, guard, account := newOAuthRecoveryFixture(t)
		defer svc.oauthRecovery.stop()
		ctx := WithOpenAIOAuthRecoveryBudget(context.Background())
		flight, started := svc.oauthRecovery.begin(ctx, account, oauthRecoveryTest401)
		require.True(t, started)
		start := time.Now()
		_, err := svc.RecoverOpenAIOAuth401(ctx, account, oauthRecoveryTest401)
		require.ErrorIs(t, err, ErrOAuthRecoveryTimeout)
		require.Equal(t, time.Minute, time.Since(start))
		guard.complete(OpenAIOAuthReauthStatusSucceeded)
		synctest.Wait()
		<-flight.done
		// A ready flight does not make an expired request eligible again.
		_, err = svc.RecoverOpenAIOAuth401(ctx, account, oauthRecoveryTest401)
		require.ErrorIs(t, err, ErrOAuthRecoveryTimeout)
		ctx = NewOpenAIOAuthRecoveryBudget(ctx)
		_, err = svc.RecoverOpenAIOAuth401(ctx, account, oauthRecoveryTest401)
		require.NoError(t, err)
	})
}

func TestOAuthRecoveryAPIKeyQueueFencesBeforeLeavingAdmission(t *testing.T) {
	for _, outcome := range []APIKeyQueueOutcome{APIKeyQueueOutcomeWaiting, APIKeyQueueOutcomeAcquired, APIKeyQueueOutcomeTimeout} {
		t.Run(strconv.Itoa(int(outcome)), func(t *testing.T) {
			recovering := false
			cache := &apiKeyQueueCacheStub{outcomes: []APIKeyQueueOutcome{outcome}, advanceHook: func() { recovering = true }}
			svc := queueServiceWithStub(APIKeyQueuePolicy{MaxWaiting: 5, Timeout: time.Second}, cache)
			ctx, _ := admissionOwnerContext(t)
			ctx = WithOAuthRecoveryAdmissionCheck(ctx, func(context.Context) bool { return recovering })
			reservation, err := svc.ReserveAPIKeySlotWithWait(ctx, 5, 1)
			require.Nil(t, reservation)
			require.ErrorIs(t, err, ErrOAuthRecoveryAdmissionInterrupted)
			require.NoError(t, ctx.Err(), "recovery interruption must leave the client context usable")
			require.Len(t, cache.abortedIDs, 1)
			require.Equal(t, cache.advanceIDs[0], cache.abortedIDs[0])
			if outcome == APIKeyQueueOutcomeAcquired {
				require.Equal(t, cache.advanceIDs, cache.releasedIDs)
			} else {
				require.True(t, cache.abortRemove[0])
			}
		})
	}
}

func TestOAuthRecoveryAPIKeyQueueFenceFailureCannotReadmit(t *testing.T) {
	recovering := false
	cache := &apiKeyQueueCacheStub{abortErr: errors.New("redis unavailable"), advanceHook: func() { recovering = true }}
	svc := queueServiceWithStub(APIKeyQueuePolicy{MaxWaiting: 5, Timeout: time.Second}, cache)
	ctx, _ := admissionOwnerContext(t)
	ctx = WithOAuthRecoveryAdmissionCheck(ctx, func(context.Context) bool { return recovering })
	reservation, err := svc.ReserveAPIKeySlotWithWait(ctx, 5, 1)
	require.Nil(t, reservation)
	require.False(t, errors.Is(err, ErrOAuthRecoveryAdmissionInterrupted))
	require.True(t, IsAPIKeyQueueErrorKind(err, APIKeyQueueErrorUnavailable))
}
