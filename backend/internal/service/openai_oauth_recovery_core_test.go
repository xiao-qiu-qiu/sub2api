//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func snapshotOAuthRecoveryFlight(r *openAIOAuthRecoveryCoordinator, flight *oauthRecoveryFlight) oauthRecoveryFlight {
	r.mu.Lock()
	defer r.mu.Unlock()
	return *flight
}

func lookupOAuthRecoveryFlight(r *openAIOAuthRecoveryCoordinator, accountID int64) *oauthRecoveryFlight {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.flights[accountID]
}

func snapshotOAuthRecoveryBudgetFlights(ctx context.Context, pending bool) []*oauthRecoveryFlight {
	b := recoveryBudget(ctx)
	b.mu.Lock()
	defer b.mu.Unlock()
	if pending {
		return append([]*oauthRecoveryFlight(nil), b.pending...)
	}
	return append([]*oauthRecoveryFlight(nil), b.observed...)
}

func requireOAuthRecoveryFlights(t *testing.T, expected, actual []*oauthRecoveryFlight) {
	t.Helper()
	require.Len(t, actual, len(expected))
	for i := range expected {
		require.Same(t, expected[i], actual[i])
	}
}

func TestOAuthRecoverySharedWindowIncludesLateWaiters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, guard, account := newOAuthRecoveryFixture(t)
		defer svc.oauthRecovery.stop()
		svc.oauthRecovery.pollTime = time.Second
		start := time.Now()
		flight, handled := svc.oauthRecovery.begin(context.Background(), account, oauthRecoveryTest401)
		require.True(t, handled)
		first := make(chan error, 1)
		go func() { first <- svc.oauthRecovery.wait(context.Background(), []*oauthRecoveryFlight{flight}) }()
		synctest.Wait()
		time.Sleep(45 * time.Second)
		lateCtx := WithOpenAIOAuthRecoveryBudget(context.Background())
		late := make(chan error, 1)
		go func() {
			_, err := svc.RecoverOpenAIOAuth401(lateCtx, account, oauthRecoveryTest401)
			late <- err
		}()
		synctest.Wait()
		time.Sleep(14 * time.Second)
		synctest.Wait()
		require.Empty(t, first)
		require.Empty(t, late)
		time.Sleep(time.Second)
		synctest.Wait()
		require.ErrorIs(t, <-first, ErrOAuthRecoveryTimeout)
		require.ErrorIs(t, <-late, ErrOAuthRecoveryTimeout)
		require.Equal(t, time.Minute, time.Since(start))
		require.Zero(t, snapshotOAuthRecoveryFlight(svc.oauthRecovery, flight).waiters)
		require.True(t, svc.oauthRecovery.active(account.ID))
		requireOAuthRecoveryFlights(t, []*oauthRecoveryFlight{flight}, snapshotOAuthRecoveryBudgetFlights(lateCtx, false))
		// An even later client sees an expired window immediately.
		_, err := svc.RecoverOpenAIOAuth401(WithOpenAIOAuthRecoveryBudget(context.Background()), account, oauthRecoveryTest401)
		require.ErrorIs(t, err, ErrOAuthRecoveryTimeout)
		require.Equal(t, time.Minute, time.Since(start))
		guard.complete(OpenAIOAuthReauthStatusSucceeded)
		<-flight.done
		require.NoError(t, snapshotOAuthRecoveryFlight(svc.oauthRecovery, flight).err)
	})
}

func TestOAuthRecoveryQueueLimitAndCanceledWaiterReleasesCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, guard, account := newOAuthRecoveryFixture(t)
		defer svc.oauthRecovery.stop()
		svc.oauthRecovery.pollTime = time.Second
		flight, handled := svc.oauthRecovery.begin(context.Background(), account, oauthRecoveryTest401)
		require.True(t, handled)
		cancels := make([]context.CancelFunc, openAIOAuthRecoveryMaxWaiters)
		results := make(chan error, openAIOAuthRecoveryMaxWaiters+1)
		for i := range cancels {
			ctx, cancel := context.WithCancel(context.Background())
			cancels[i] = cancel
			defer cancel()
			go func() { results <- svc.oauthRecovery.wait(ctx, []*oauthRecoveryFlight{flight, flight}) }()
		}
		synctest.Wait()
		require.Equal(t, openAIOAuthRecoveryMaxWaiters, snapshotOAuthRecoveryFlight(svc.oauthRecovery, flight).waiters)
		fullCtx := WithOpenAIOAuthRecoveryBudget(context.Background())
		_, err := svc.RecoverOpenAIOAuth401(fullCtx, account, oauthRecoveryTest401)
		require.ErrorIs(t, err, ErrOAuthRecoveryQueueFull)
		requireOAuthRecoveryFlights(t, []*oauthRecoveryFlight{flight}, snapshotOAuthRecoveryBudgetFlights(fullCtx, false))
		require.True(t, PrepareOpenAIOAuthRecoveryFallback(fullCtx, err))
		require.True(t, oauthRecoveryAccountExcluded(fullCtx, account.ID))
		require.Equal(t, openAIOAuthRecoveryMaxWaiters, snapshotOAuthRecoveryFlight(svc.oauthRecovery, flight).waiters)
		cancels[0]()
		synctest.Wait()
		require.ErrorIs(t, <-results, context.Canceled)
		require.Equal(t, openAIOAuthRecoveryMaxWaiters-1, snapshotOAuthRecoveryFlight(svc.oauthRecovery, flight).waiters)
		go func() { results <- svc.oauthRecovery.wait(context.Background(), []*oauthRecoveryFlight{flight}) }()
		synctest.Wait()
		require.Equal(t, openAIOAuthRecoveryMaxWaiters, snapshotOAuthRecoveryFlight(svc.oauthRecovery, flight).waiters)
		require.True(t, svc.oauthRecovery.active(account.ID))
		guard.complete(OpenAIOAuthReauthStatusSucceeded)
		synctest.Wait()
		for i := 0; i < openAIOAuthRecoveryMaxWaiters; i++ {
			require.NoError(t, <-results)
		}
		require.Zero(t, snapshotOAuthRecoveryFlight(svc.oauthRecovery, flight).waiters)
		require.Equal(t, 1, guard.creates)
	})
}

func TestOAuthRecoveryPersistentTaskKeepsOriginalDeadline(t *testing.T) {
	for _, queue := range []bool{false, true} {
		t.Run(map[bool]string{false: "pool_discovery", true: "selected_account"}[queue], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, guard, account := newOAuthRecoveryFixture(t)
				defer svc.oauthRecovery.stop()
				svc.oauthRecovery.pollTime = time.Second
				guard.task.CreatedAt = time.Now().Add(-50 * time.Second)
				guard.task.UpdatedAt = time.Now() // Worker progress cannot renew the window.
				guard.repo.accounts[0].Status = StatusError
				ctx := context.WithValue(WithOpenAIOAuthRecoveryBudget(context.Background()), oauthRecoveryDiscoveryKey{}, true)
				if queue {
					require.True(t, svc.QueueOpenAIOAuthRecoveryForAccount(ctx, account.ID))
					require.Len(t, snapshotOAuthRecoveryBudgetFlights(ctx, true), 1)
				}
				start := time.Now()
				groupID := int64(7)
				waited, err := svc.WaitForOpenAIOAuthRecovery(ctx, &groupID, "gpt-test")
				require.True(t, waited)
				require.ErrorIs(t, err, ErrOAuthRecoveryTimeout)
				require.Equal(t, 10*time.Second, time.Since(start))
				require.Zero(t, guard.creates)
				require.True(t, svc.oauthRecovery.active(account.ID))
				require.Len(t, snapshotOAuthRecoveryBudgetFlights(ctx, false), 1)
				require.True(t, PrepareOpenAIOAuthRecoveryFallback(ctx, err))
				require.False(t, svc.QueueOpenAIOAuthRecoveryForAccount(ctx, account.ID))
				require.Empty(t, svc.pendingOpenAIOAuthRecovery(ctx, &groupID, "gpt-test"))
				require.False(t, svc.HasPendingOpenAIOAuthRecovery(ctx, &groupID, "gpt-test"))
			})
		})
	}
}

func TestOAuthRecoveryDiscoveryDoesNotRenewExpiredTask(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, guard, account := newOAuthRecoveryFixture(t)
		defer svc.oauthRecovery.stop()
		svc.oauthRecovery.pollTime = time.Second
		guard.task.CreatedAt = time.Now().Add(-2 * time.Minute)
		ctx := WithOpenAIOAuthRecoveryBudget(context.Background())
		require.True(t, svc.QueueOpenAIOAuthRecoveryForAccount(ctx, account.ID))
		start := time.Now()
		waited, err := svc.WaitForOpenAIOAuthRecovery(ctx, nil, "gpt-test")
		require.True(t, waited)
		require.ErrorIs(t, err, ErrOAuthRecoveryTimeout)
		require.Zero(t, time.Since(start))
		require.Zero(t, guard.creates)
		require.Zero(t, snapshotOAuthRecoveryFlight(svc.oauthRecovery, lookupOAuthRecoveryFlight(svc.oauthRecovery, account.ID)).waiters)
	})
}

func TestOAuthRecoveryLateOld401AndQueueReuseCompletedFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, guard, account := newOAuthRecoveryFixture(t)
		defer svc.oauthRecovery.stop()
		svc.oauthRecovery.pollTime = time.Second
		ctx := WithOpenAIOAuthRecoveryBudget(context.Background())
		flight, handled := svc.oauthRecovery.begin(ctx, account, oauthRecoveryTest401)
		require.True(t, handled)
		guard.complete(OpenAIOAuthReauthStatusFailed)
		synctest.Wait()
		require.ErrorIs(t, snapshotOAuthRecoveryFlight(svc.oauthRecovery, flight).err, ErrOAuthRecoveryFailed)
		time.Sleep(2 * time.Minute)
		lateCtx := WithOpenAIOAuthRecoveryBudget(context.Background())
		reused, handled := svc.oauthRecovery.begin(lateCtx, account, oauthRecoveryTest401)
		require.True(t, handled)
		require.Same(t, flight, reused)
		require.True(t, svc.QueueOpenAIOAuthRecoveryForAccount(lateCtx, account.ID))
		require.True(t, svc.QueueOpenAIOAuthRecoveryForAccount(lateCtx, account.ID))
		requireOAuthRecoveryFlights(t, []*oauthRecoveryFlight{flight}, snapshotOAuthRecoveryBudgetFlights(lateCtx, true))
		waited, err := svc.WaitForOpenAIOAuthRecovery(lateCtx, nil, "gpt-test")
		require.True(t, waited)
		require.ErrorIs(t, err, ErrOAuthRecoveryFailed)
		require.Equal(t, 1, guard.creates)
		// A genuinely different credential is a new round.
		guard.repo.accounts[0].Credentials = map[string]any{"access_token": "different-token", "refresh_token": "different-refresh"}
		guard.task.ID++
		guard.task.Status = OpenAIOAuthReauthStatusRunning
		current, err := guard.repo.GetByID(ctx, account.ID)
		require.NoError(t, err)
		fresh, handled := svc.oauthRecovery.begin(WithOpenAIOAuthRecoveryBudget(context.Background()), current, oauthRecoveryTest401)
		require.True(t, handled)
		require.NotSame(t, flight, fresh)
		require.Equal(t, 2, guard.creates)
	})
}

type oauthRecoveryControlledGuard struct {
	*oauthRecoveryTestGuard
	beforeRequest func()
	beforeVerify  func()
}

func (g *oauthRecoveryControlledGuard) RequestImmediateReauth(ctx context.Context, account *Account) (*OpenAIOAuthReauthTask, error) {
	if g.beforeRequest != nil {
		g.beforeRequest()
	}
	return g.oauthRecoveryTestGuard.RequestImmediateReauth(ctx, account)
}

func (g *oauthRecoveryControlledGuard) VerifyImmediateReauth(ctx context.Context, accountID int64) error {
	if g.beforeVerify != nil {
		g.beforeVerify()
	}
	return g.oauthRecoveryTestGuard.VerifyImmediateReauth(ctx, accountID)
}

func TestOAuthRecoveryBeginDeadlineIncludesTaskCreationAndEarlierDurableTask(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "creation_latency", true: "existing_task"}[existing], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, guard, account := newOAuthRecoveryFixture(t)
				defer svc.oauthRecovery.stop()
				svc.oauthRecovery.pollTime = time.Second
				if existing {
					guard.task.CreatedAt = time.Now().Add(-50 * time.Second)
				}
				release := make(chan struct{})
				svc.oauthRecovery.guard = &oauthRecoveryControlledGuard{oauthRecoveryTestGuard: guard, beforeRequest: func() { <-release }}
				start := time.Now()
				begun := make(chan *oauthRecoveryFlight, 1)
				go func() {
					flight, _ := svc.oauthRecovery.begin(context.Background(), account, oauthRecoveryTest401)
					begun <- flight
				}()
				synctest.Wait()
				flight := lookupOAuthRecoveryFlight(svc.oauthRecovery, account.ID)
				require.NotNil(t, flight)
				result := make(chan error, 1)
				go func() { result <- svc.oauthRecovery.wait(context.Background(), []*oauthRecoveryFlight{flight}) }()
				synctest.Wait()
				time.Sleep(2 * time.Second)
				close(release)
				require.Same(t, flight, <-begun)
				require.ErrorIs(t, <-result, ErrOAuthRecoveryTimeout)
				expected := time.Minute
				if existing {
					expected = 10 * time.Second
				}
				require.Equal(t, expected, time.Since(start))
				require.True(t, svc.oauthRecovery.active(account.ID))
			})
		})
	}
}

type oauthRecoverySnapshotCache struct {
	SchedulerCache
	beforeSet func()
	err       error
}

func (c *oauthRecoverySnapshotCache) SetAccount(context.Context, *Account) error {
	if c.beforeSet != nil {
		c.beforeSet()
	}
	return c.err
}

func TestOAuthRecoveryPublishesOnlyAfterVerificationAndCacheRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, guard, account := newOAuthRecoveryFixture(t)
		defer svc.oauthRecovery.stop()
		svc.oauthRecovery.pollTime = time.Second
		verify := make(chan struct{})
		cache := make(chan struct{})
		svc.oauthRecovery.guard = &oauthRecoveryControlledGuard{oauthRecoveryTestGuard: guard, beforeVerify: func() { <-verify }}
		svc.schedulerSnapshot = &SchedulerSnapshotService{cache: &oauthRecoverySnapshotCache{beforeSet: func() { <-cache }}}
		flight, handled := svc.oauthRecovery.begin(context.Background(), account, oauthRecoveryTest401)
		require.True(t, handled)
		result := make(chan error, 1)
		go func() { result <- svc.oauthRecovery.wait(context.Background(), []*oauthRecoveryFlight{flight}) }()
		guard.complete(OpenAIOAuthReauthStatusSucceeded)
		synctest.Wait()
		require.Empty(t, result, "login success alone must not wake the request")
		close(verify)
		synctest.Wait()
		require.Empty(t, result, "fresh verification still needs the scheduler cache refresh")
		require.Equal(t, 1, guard.verifies)
		close(cache)
		synctest.Wait()
		require.NoError(t, <-result)
		require.Zero(t, snapshotOAuthRecoveryFlight(svc.oauthRecovery, flight).waiters)
	})
}

func TestOAuthRecoveryCacheRefreshFailureDoesNotAllowReplay(t *testing.T) {
	for _, tokenCache := range []bool{false, true} {
		t.Run(map[bool]string{false: "snapshot", true: "token_cache"}[tokenCache], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, guard, account := newOAuthRecoveryFixture(t)
				defer svc.oauthRecovery.stop()
				svc.oauthRecovery.pollTime = time.Second
				cacheErr := errors.New("cache refresh failed")
				if tokenCache {
					cache := newOpenAITokenCacheStub()
					cache.deleteErr = cacheErr
					svc.openAITokenProvider = &OpenAITokenProvider{tokenCache: cache}
				} else {
					svc.schedulerSnapshot = &SchedulerSnapshotService{cache: &oauthRecoverySnapshotCache{err: cacheErr}}
				}
				flight, handled := svc.oauthRecovery.begin(context.Background(), account, oauthRecoveryTest401)
				require.True(t, handled)
				guard.complete(OpenAIOAuthReauthStatusSucceeded)
				err := svc.oauthRecovery.wait(context.Background(), []*oauthRecoveryFlight{flight})
				require.ErrorIs(t, err, ErrOAuthRecoveryFailed)
				require.ErrorIs(t, snapshotOAuthRecoveryFlight(svc.oauthRecovery, flight).err, cacheErr)
			})
		})
	}
}

func TestOAuthRecoveryWaitObservesOnlyCurrentTargets(t *testing.T) {
	svc, _, account := newOAuthRecoveryFixture(t)
	ctx := WithOpenAIOAuthRecoveryBudget(context.Background())
	r := svc.oauthRecovery
	r.mu.Lock()
	first := r.newFlight(account.ID, "first", nil)
	second := r.newFlight(2, "second", nil)
	r.mu.Unlock()
	r.finish(first, nil)
	r.finish(second, ErrOAuthRecoveryFailed)
	require.NoError(t, r.wait(ctx, []*oauthRecoveryFlight{first}))
	err := r.wait(ctx, []*oauthRecoveryFlight{second, second})
	require.ErrorIs(t, err, ErrOAuthRecoveryFailed)
	requireOAuthRecoveryFlights(t, []*oauthRecoveryFlight{second}, snapshotOAuthRecoveryBudgetFlights(ctx, false))
	require.True(t, PrepareOpenAIOAuthRecoveryFallback(ctx, err))
	require.False(t, oauthRecoveryAccountExcluded(ctx, first.accountID))
	require.True(t, oauthRecoveryAccountExcluded(ctx, second.accountID))
}

func TestOAuthRecoveryPoolWaitKeepsOtherTasksUntilTheirOwnDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, _, account := newOAuthRecoveryFixture(t)
		defer svc.oauthRecovery.stop()
		r := svc.oauthRecovery
		r.mu.Lock()
		earlier := r.newFlight(account.ID, "earlier", &OpenAIOAuthReauthTask{CreatedAt: time.Now().Add(-50 * time.Second)})
		later := r.newFlight(2, "later", nil)
		r.mu.Unlock()
		ctx := WithOpenAIOAuthRecoveryBudget(context.Background())
		targets := []*oauthRecoveryFlight{earlier, later, later}
		result := make(chan error, 1)
		go func() { result <- r.wait(ctx, targets) }()
		synctest.Wait()
		require.Equal(t, 1, snapshotOAuthRecoveryFlight(r, earlier).waiters)
		require.Equal(t, 1, snapshotOAuthRecoveryFlight(r, later).waiters)
		time.Sleep(10 * time.Second)
		synctest.Wait()
		require.Zero(t, snapshotOAuthRecoveryFlight(r, earlier).waiters)
		require.Equal(t, 1, snapshotOAuthRecoveryFlight(r, later).waiters)
		require.Empty(t, result, "the other task still has fifty seconds left")
		time.Sleep(5 * time.Second)
		r.finish(later, nil)
		require.NoError(t, <-result)
		require.Zero(t, snapshotOAuthRecoveryFlight(r, later).waiters)
		requireOAuthRecoveryFlights(t, []*oauthRecoveryFlight{earlier, later, later}, targets)
		requireOAuthRecoveryFlights(t, []*oauthRecoveryFlight{earlier, later}, snapshotOAuthRecoveryBudgetFlights(ctx, false))
	})
}

func TestOAuthRecoveryQueueDoesNotCreateWorkOrReuseChangedCredentials(t *testing.T) {
	svc, guard, account := newOAuthRecoveryFixture(t)
	guard.task.Status = OpenAIOAuthReauthStatusFailed
	ctx := WithOpenAIOAuthRecoveryBudget(context.Background())
	require.False(t, svc.QueueOpenAIOAuthRecoveryForAccount(ctx, account.ID))
	require.Zero(t, guard.creates)
	require.Empty(t, snapshotOAuthRecoveryBudgetFlights(ctx, true))
	r := svc.oauthRecovery
	r.mu.Lock()
	flight := r.newFlight(account.ID, account.GetCredential("access_token"), nil)
	r.flights[account.ID] = flight
	r.mu.Unlock()
	r.finish(flight, ErrOAuthRecoveryFailed)
	guard.repo.mu.Lock()
	guard.repo.accounts[0].Credentials = map[string]any{"access_token": "changed-token", "refresh_token": "changed-refresh"}
	guard.repo.mu.Unlock()
	require.False(t, svc.QueueOpenAIOAuthRecoveryForAccount(ctx, account.ID))
	require.Zero(t, guard.creates)
	require.Empty(t, snapshotOAuthRecoveryBudgetFlights(ctx, true))
}

func TestOAuthRecoverySameCredentialsAdoptNewDurableTask(t *testing.T) {
	for _, entry := range []string{"begin", "discover", "queue"} {
		t.Run(entry, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				svc, guard, account := newOAuthRecoveryFixture(t)
				defer svc.oauthRecovery.stop()
				svc.oauthRecovery.pollTime = time.Second
				first, handled := svc.oauthRecovery.begin(context.Background(), account, oauthRecoveryTest401)
				require.True(t, handled)
				guard.complete(OpenAIOAuthReauthStatusFailed)
				<-first.done
				firstState := snapshotOAuthRecoveryFlight(svc.oauthRecovery, first)
				require.ErrorIs(t, firstState.err, ErrOAuthRecoveryFailed)
				join := func(ctx context.Context) *oauthRecoveryFlight {
					switch entry {
					case "begin":
						flight, handled := svc.oauthRecovery.begin(ctx, account, oauthRecoveryTest401)
						require.True(t, handled)
						return flight
					case "discover":
						return svc.oauthRecovery.discover(ctx, account, true)
					default:
						require.True(t, svc.QueueOpenAIOAuthRecoveryForAccount(ctx, account.ID))
						pending := snapshotOAuthRecoveryBudgetFlights(ctx, true)
						require.Len(t, pending, 1)
						return pending[0]
					}
				}
				// No new durable task: late old-token responses retain T1's outcome
				// without asking the worker to start another login.
				time.Sleep(2 * time.Minute)
				require.Same(t, first, join(WithOpenAIOAuthRecoveryBudget(context.Background())))
				require.Equal(t, 1, guard.creates)
				// An administrator/other instance starts T2 with unchanged credentials.
				created := time.Now().Add(-45 * time.Second)
				guard.mu.Lock()
				guard.task.ID++
				guard.task.CreatedAt = created
				guard.task.UpdatedAt = time.Now()
				guard.task.Status = OpenAIOAuthReauthStatusRunning
				guard.mu.Unlock()
				ctx := WithOpenAIOAuthRecoveryBudget(context.Background())
				second := join(ctx)
				require.NotNil(t, second)
				require.NotSame(t, first, second)
				require.Equal(t, first.token, second.token)
				secondState := snapshotOAuthRecoveryFlight(svc.oauthRecovery, second)
				require.Equal(t, firstState.taskID+1, secondState.taskID)
				require.Equal(t, created.Add(OpenAIOAuthRecoveryWait), secondState.deadline)
				late, handled := svc.oauthRecovery.begin(WithOpenAIOAuthRecoveryBudget(context.Background()), account, oauthRecoveryTest401)
				require.True(t, handled)
				require.Same(t, second, late)
				require.Equal(t, 1, guard.creates, "discovering T2 must not request T3")
				start := time.Now()
				var err error
				if entry == "queue" {
					_, err = svc.WaitForOpenAIOAuthRecovery(ctx, nil, "gpt-test")
				} else {
					_, err = svc.RecoverOpenAIOAuth401(ctx, account, oauthRecoveryTest401)
				}
				require.ErrorIs(t, err, ErrOAuthRecoveryTimeout)
				require.Equal(t, 15*time.Second, time.Since(start))
				require.True(t, svc.oauthRecovery.active(account.ID))
				guard.complete(OpenAIOAuthReauthStatusSucceeded)
				<-second.done
				require.NoError(t, snapshotOAuthRecoveryFlight(svc.oauthRecovery, second).err)
				require.Equal(t, 1, guard.verifies)
			})
		})
	}
}

func TestOAuthRecoveryConcurrentDiscoverySharesNewDurableTask(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		svc, guard, account := newOAuthRecoveryFixture(t)
		defer svc.oauthRecovery.stop()
		svc.oauthRecovery.pollTime = time.Second
		first, handled := svc.oauthRecovery.begin(context.Background(), account, oauthRecoveryTest401)
		require.True(t, handled)
		guard.complete(OpenAIOAuthReauthStatusFailed)
		<-first.done
		guard.mu.Lock()
		guard.task.ID++
		guard.task.CreatedAt = time.Now()
		guard.task.Status = OpenAIOAuthReauthStatusRunning
		guard.mu.Unlock()
		const count = 24
		results := make(chan *oauthRecoveryFlight, count)
		for i := 0; i < count; i++ {
			go func() {
				if i%2 == 0 {
					flight, _ := svc.oauthRecovery.begin(context.Background(), account, oauthRecoveryTest401)
					results <- flight
				} else {
					results <- svc.oauthRecovery.discover(context.Background(), account, true)
				}
			}()
		}
		second := <-results
		require.NotNil(t, second)
		require.NotSame(t, first, second)
		for i := 1; i < count; i++ {
			require.Same(t, second, <-results)
		}
		require.Equal(t, snapshotOAuthRecoveryFlight(svc.oauthRecovery, first).taskID+1,
			snapshotOAuthRecoveryFlight(svc.oauthRecovery, second).taskID)
		guard.complete(OpenAIOAuthReauthStatusSucceeded)
		<-second.done
		require.NoError(t, snapshotOAuthRecoveryFlight(svc.oauthRecovery, second).err)
		guard.mu.Lock()
		creates, verifies := guard.creates, guard.verifies
		guard.mu.Unlock()
		require.Equal(t, 1, creates)
		require.Equal(t, 1, verifies, "all discoverers must share the same observer")
	})
}
