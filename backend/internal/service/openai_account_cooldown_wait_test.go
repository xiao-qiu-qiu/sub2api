//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// Keep the existing scheduler doubles, adding synchronization for overlapping
// requests and an acquisition hook for a 429 racing with account selection.
type excelBPSStickyBindings struct {
	*schedulerTestGatewayCache
	mu     sync.Mutex
	reads  atomic.Int32
	onRead func(int32)
}

func (c *excelBPSStickyBindings) GetSessionAccountID(ctx context.Context, group int64, hash string) (int64, error) {
	if c.onRead != nil {
		c.onRead(c.reads.Add(1))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.schedulerTestGatewayCache.GetSessionAccountID(ctx, group, hash)
}

func (c *excelBPSStickyBindings) SetSessionAccountID(ctx context.Context, group int64, hash string, id int64, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.schedulerTestGatewayCache.SetSessionAccountID(ctx, group, hash, id, ttl)
}

func (c *excelBPSStickyBindings) DeleteSessionAccountID(ctx context.Context, group int64, hash string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.schedulerTestGatewayCache.DeleteSessionAccountID(ctx, group, hash)
}

type excelBPSStickySlots struct {
	schedulerTestConcurrencyCache
	mu                 sync.Mutex
	acquired, released map[int64]int
	onAcquire          func(int64)
}

func (c *excelBPSStickySlots) AcquireAccountSlot(ctx context.Context, id int64, max int, request string) (bool, error) {
	ok, err := c.schedulerTestConcurrencyCache.AcquireAccountSlot(ctx, id, max, request)
	if ok && err == nil {
		c.mu.Lock()
		c.acquired[id]++
		c.mu.Unlock()
		if c.onAcquire != nil {
			c.onAcquire(id)
		}
	}
	return ok, err
}

func (c *excelBPSStickySlots) ReleaseAccountSlot(_ context.Context, id int64, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.released[id]++
	return nil
}

func (c *excelBPSStickySlots) counts(id int64) (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.acquired[id], c.released[id]
}

type excelBPSStickyFixture struct {
	svc      *OpenAIGatewayService
	repo     *schedulerTestOpenAIAccountRepo
	slots    *excelBPSStickySlots
	bindings *excelBPSStickyBindings
	group    int64
	session  string
}

func newExcelBPSStickyFixture(t *testing.T, advanced bool) *excelBPSStickyFixture {
	t.Helper()
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	f := &excelBPSStickyFixture{group: 101301, session: "bps-cooldown"}
	accounts := make([]Account, 0, 2)
	for _, id := range []int64{1, 2} {
		accounts = append(accounts, Account{
			ID: id, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
			Schedulable: true, Concurrency: 2, GroupIDs: []int64{f.group},
			Credentials: map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-chatgpt"},
			Extra:       map[string]any{"openai_excel_bps": true, "openai_excel_bps_models": []any{"gpt-6-astra"}},
		})
	}
	f.repo = &schedulerTestOpenAIAccountRepo{accounts: accounts}
	f.slots = &excelBPSStickySlots{acquired: map[int64]int{}, released: map[int64]int{}}
	f.bindings = &excelBPSStickyBindings{schedulerTestGatewayCache: &schedulerTestGatewayCache{
		sessionBindings: map[string]int64{"openai:" + f.session: 1},
	}}
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	f.svc = &OpenAIGatewayService{
		accountRepo: f.repo, cache: f.bindings, cfg: cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService(strconv.FormatBool(advanced)),
		concurrencyService: NewConcurrencyService(f.slots),
	}
	require.Equal(t, advanced, f.svc.isOpenAIAdvancedSchedulerEnabled(t.Context()))
	return f
}

func (f *excelBPSStickyFixture) coolFor(seconds string) {
	f.svc.coolDownExcelBPS(context.Background(), &f.repo.accounts[0], seconds)
}

func (f *excelBPSStickyFixture) owner(t *testing.T) int64 {
	t.Helper()
	id, err := f.bindings.GetSessionAccountID(t.Context(), f.group, "openai:"+f.session)
	require.NoError(t, err)
	return id
}

func (f *excelBPSStickyFixture) assertNoHeldSlots(t *testing.T) {
	t.Helper()
	for _, id := range []int64{1, 2} {
		acquired, released := f.slots.counts(id)
		require.Equal(t, acquired, released, "account %d retains a generation slot", id)
	}
}

type excelBPSStickyResult struct {
	accountID int64
	err       error
	at        time.Time
}

// Mirror the ingress contract: selection returns an interruption before it
// waits; retry selection with the SAME request context after the waiter.
func (f *excelBPSStickyFixture) start(ctx context.Context, model string) <-chan excelBPSStickyResult {
	done := make(chan excelBPSStickyResult, 1)
	go func() {
		for {
			if ctx.Err() != nil {
				done <- excelBPSStickyResult{err: context.Cause(ctx), at: time.Now()}
				return
			}
			selection, _, err := f.svc.SelectAccountWithScheduler(ctx, &f.group, "", f.session, model, nil, OpenAIUpstreamTransportAny, false)
			if errors.Is(err, ErrOpenAIAccountCooldownAdmissionInterrupted) {
				_, err = f.svc.WaitForOpenAIAccountCooldown(ctx)
				if err == nil {
					continue
				}
			}
			result := excelBPSStickyResult{err: err, at: time.Now()}
			if selection != nil {
				if selection.Account != nil {
					result.accountID = selection.Account.ID
				}
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
			}
			done <- result
			return
		}
	}()
	return done
}

func requireExcelBPSStickyPending(t *testing.T, done <-chan excelBPSStickyResult) {
	t.Helper()
	synctest.Wait()
	select {
	case result := <-done:
		t.Fatalf("selection returned while cooldown budget remained: %+v", result)
	default:
	}
}

func TestExcelBPSStickyShortCooldownKeepsOwner(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		t.Run(fmt.Sprintf("advanced=%t", advanced), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newExcelBPSStickyFixture(t, advanced)
				f.coolFor("10")
				ctx, cancel := context.WithCancel(WithOpenAIAccountCooldownBudget(t.Context()))
				defer cancel()
				started := time.Now()
				done := f.start(ctx, "gpt-6-astra")
				requireExcelBPSStickyPending(t, done)
				time.Sleep(9 * time.Second)
				requireExcelBPSStickyPending(t, done)
				require.EqualValues(t, 1, f.owner(t))
				f.assertNoHeldSlots(t)
				result := <-done
				require.NoError(t, result.err)
				require.EqualValues(t, 1, result.accountID)
				require.Equal(t, 10*time.Second, result.at.Sub(started))
				require.EqualValues(t, 1, f.owner(t))
				acquired, _ := f.slots.counts(2)
				require.Zero(t, acquired, "healthy backup must not bypass the sticky wait")
				f.assertNoHeldSlots(t)
			})
		})
	}
}

func TestExcelBPSStickyBudgetDoesNotReset(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		for _, repeated := range []bool{false, true} {
			t.Run(fmt.Sprintf("advanced=%t/repeated_selection=%t", advanced, repeated), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					f := newExcelBPSStickyFixture(t, advanced)
					f.coolFor("10")
					ctx, cancel := context.WithCancel(WithOpenAIAccountCooldownBudget(t.Context()))
					defer cancel()
					started := time.Now()
					done := f.start(ctx, "gpt-6-astra")
					requireExcelBPSStickyPending(t, done)
					if repeated {
						first := <-done
						require.NoError(t, first.err)
						require.EqualValues(t, 1, first.accountID)
						time.Sleep(5 * time.Second)
						f.coolFor("60")
						require.Same(t, accountCooldownBudget(ctx), accountCooldownBudget(WithOpenAIAccountCooldownBudget(ctx)))
						done = f.start(ctx, "gpt-6-astra")
					} else {
						time.Sleep(5 * time.Second)
						f.coolFor("60")
					}
					time.Sleep(time.Until(started.Add(29 * time.Second)))
					requireExcelBPSStickyPending(t, done)
					require.EqualValues(t, 1, f.owner(t))
					f.assertNoHeldSlots(t)
					result := <-done
					require.NoError(t, result.err)
					require.Equal(t, 30*time.Second, result.at.Sub(started))
					require.EqualValues(t, 2, result.accountID)
					require.EqualValues(t, 2, f.owner(t), "normal rebinding is allowed after the wait budget expires")
					require.Positive(t, f.svc.excelBPSCooldownRemaining(&f.repo.accounts[0], "gpt-6-astra"), "wait cap must not shorten the account cooldown")
					f.assertNoHeldSlots(t)
				})
			})
		}
	}
}

func TestExcelBPSStickyCancellationAndEarlyDeadline(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		for _, deadline := range []bool{false, true} {
			t.Run(fmt.Sprintf("advanced=%t/deadline=%t", advanced, deadline), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					f := newExcelBPSStickyFixture(t, advanced)
					f.coolFor("60")
					ctx := WithOpenAIAccountCooldownBudget(t.Context())
					var cancel context.CancelFunc
					wantErr := context.Canceled
					if deadline {
						ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
						wantErr = context.DeadlineExceeded
					} else {
						ctx, cancel = context.WithCancel(ctx)
					}
					defer cancel()
					started := time.Now()
					done := f.start(ctx, "gpt-6-astra")
					requireExcelBPSStickyPending(t, done)
					time.Sleep(5 * time.Second)
					if !deadline {
						cancel()
					}
					result := <-done
					require.ErrorIs(t, result.err, wantErr)
					require.Equal(t, 5*time.Second, result.at.Sub(started))
					require.Zero(t, result.accountID)
					require.EqualValues(t, 1, f.owner(t))
					for _, id := range []int64{1, 2} {
						acquired, _ := f.slots.counts(id)
						require.Zero(t, acquired)
					}
					require.Equal(t, 55*time.Second, f.svc.excelBPSCooldownRemaining(&f.repo.accounts[0], "gpt-6-astra"))
				})
			})
		}
	}
}

func TestExcelBPSStickyIndependentRequestBudgets(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		t.Run(fmt.Sprintf("advanced=%t", advanced), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newExcelBPSStickyFixture(t, advanced)
				f.coolFor("120")
				firstCtx, cancelFirst := context.WithCancel(WithOpenAIAccountCooldownBudget(t.Context()))
				defer cancelFirst()
				started := time.Now()
				first := f.start(firstCtx, "gpt-6-astra")
				requireExcelBPSStickyPending(t, first)
				time.Sleep(10 * time.Second)
				secondCtx, cancelSecond := context.WithCancel(WithOpenAIAccountCooldownBudget(t.Context()))
				defer cancelSecond()
				require.NotSame(t, accountCooldownBudget(firstCtx), accountCooldownBudget(secondCtx))
				second := f.start(secondCtx, "gpt-6-astra")
				requireExcelBPSStickyPending(t, second)
				time.Sleep(19 * time.Second)
				requireExcelBPSStickyPending(t, first)
				requireExcelBPSStickyPending(t, second)
				require.EqualValues(t, 1, f.owner(t))
				f.assertNoHeldSlots(t)
				firstResult := <-first
				require.NoError(t, firstResult.err)
				require.EqualValues(t, 2, firstResult.accountID)
				require.Equal(t, 30*time.Second, firstResult.at.Sub(started))
				time.Sleep(9 * time.Second)
				requireExcelBPSStickyPending(t, second)
				secondResult := <-second
				require.NoError(t, secondResult.err)
				require.EqualValues(t, 2, secondResult.accountID)
				require.Equal(t, 40*time.Second, secondResult.at.Sub(started))
				f.assertNoHeldSlots(t)
			})
		})
	}
}

func TestExcelBPSStickyYoungerWaiterKeepsOwnerAfterSharedRebind(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		t.Run(fmt.Sprintf("advanced=%t", advanced), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newExcelBPSStickyFixture(t, advanced)
				f.coolFor("35")
				firstCtx, cancelFirst := context.WithCancel(WithOpenAIAccountCooldownBudget(t.Context()))
				defer cancelFirst()
				started := time.Now()
				first := f.start(firstCtx, "gpt-6-astra")
				requireExcelBPSStickyPending(t, first)

				time.Sleep(10 * time.Second)
				secondCtx, cancelSecond := context.WithCancel(WithOpenAIAccountCooldownBudget(t.Context()))
				defer cancelSecond()
				second := f.start(secondCtx, "gpt-6-astra")
				requireExcelBPSStickyPending(t, second)
				time.Sleep(19 * time.Second)
				requireExcelBPSStickyPending(t, first)
				requireExcelBPSStickyPending(t, second)
				require.EqualValues(t, 1, f.owner(t))
				f.assertNoHeldSlots(t)

				firstResult := <-first
				require.NoError(t, firstResult.err)
				require.Equal(t, 30*time.Second, firstResult.at.Sub(started))
				require.EqualValues(t, 2, firstResult.accountID, "R1 may fail over after its own budget expires")
				require.EqualValues(t, 2, f.owner(t), "R1 updates the shared binding to B")
				requireExcelBPSStickyPending(t, second)
				f.assertNoHeldSlots(t)

				time.Sleep(4 * time.Second)
				requireExcelBPSStickyPending(t, second)
				require.EqualValues(t, 2, f.owner(t))
				secondResult := <-second
				require.NoError(t, secondResult.err)
				require.Equal(t, 35*time.Second, secondResult.at.Sub(started))
				require.EqualValues(t, 1, secondResult.accountID, "R2 must resume A after 25s, within its own 30s budget")
				require.False(t, accountCooldownBudget(secondCtx).expired())
				require.EqualValues(t, 2, f.owner(t), "R2 selection must leave the newer shared binding intact")

				owner, err := f.svc.getStickySessionAccountID(secondCtx, &f.group, f.session)
				require.NoError(t, err)
				require.EqualValues(t, 1, owner, "R2 retains its request-local original owner")
				owner, err = f.svc.getStickySessionAccountID(firstCtx, &f.group, f.session)
				require.NoError(t, err)
				require.EqualValues(t, 2, owner, "expired R1 reads the shared binding normally")
				require.NoError(t, f.svc.BindStickySessionAfterProfitAdmission(secondCtx, &f.group, f.session, secondResult.accountID))
				require.EqualValues(t, 2, f.owner(t), "R2 post-admission binding must not overwrite R1 with A")
				require.NoError(t, f.svc.deleteStickySessionAccountID(secondCtx, &f.group, f.session))
				require.EqualValues(t, 2, f.owner(t), "R2 invalidation must not delete R1 binding")
				f.assertNoHeldSlots(t)
			})
		})
	}
}

func TestExcelBPSStickyNativeDoesNotWait(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		for _, disabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("advanced=%t/bps_disabled=%t", advanced, disabled), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					f := newExcelBPSStickyFixture(t, advanced)
					f.coolFor("120")
					model := "gpt-5.1"
					if disabled {
						model = "gpt-6-astra"
						for i := range f.repo.accounts {
							f.repo.accounts[i].Extra["openai_excel_bps"] = false
						}
					}
					ctx := WithOpenAIAccountCooldownBudget(t.Context())
					started := time.Now()
					result := <-f.start(ctx, model)
					require.NoError(t, result.err)
					require.EqualValues(t, 1, result.accountID)
					require.Equal(t, started, result.at)
					require.True(t, accountCooldownBudget(ctx).deadline.IsZero())
					require.EqualValues(t, 1, f.owner(t))
					f.assertNoHeldSlots(t)
				})
			})
		}
	}
}

func TestExcelBPSStickyRecoveredOwnerBypassesSoftEscape(t *testing.T) {
	for _, strategy := range []string{"ttft", "weighted"} {
		t.Run(strategy, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newExcelBPSStickyFixture(t, true)
				if strategy == "ttft" {
					f.svc.cfg.Gateway.OpenAIScheduler.StickyEscapeEnabled = true
					f.svc.cfg.Gateway.OpenAIScheduler.StickyEscapeTTFTMs = 1000
					f.svc.cfg.Gateway.OpenAIScheduler.StickyEscapeErrorRate = .5
					f.svc.openaiAccountStats = newOpenAIAccountRuntimeStats()
					slow := 20000
					f.svc.openaiAccountStats.report(1, true, &slow)
				} else {
					f.svc.rateLimitService = newOpenAIAdvancedSchedulerRateLimitService("true", "true")
					f.svc.cfg.Gateway.OpenAIWS.LBTopK = 1
					f.svc.cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Priority = 1
					f.svc.cfg.Gateway.OpenAIWS.SchedulerScoreWeights.SessionSticky = .001
					f.repo.accounts[0].Priority = 100
					require.True(t, f.svc.isOpenAIAdvancedSchedulerStickyWeightedEnabled(t.Context()))
				}
				// Prove this fixture would really leave the owner without a wait.
				baseline := <-f.start(WithOpenAIAccountCooldownBudget(t.Context()), "gpt-6-astra")
				require.NoError(t, baseline.err)
				require.EqualValues(t, 2, baseline.accountID, "fixture must exercise an active soft escape")
				require.NoError(t, f.bindings.SetSessionAccountID(t.Context(), f.group, "openai:"+f.session, 1, time.Hour))
				f.coolFor("10")
				ctx, cancel := context.WithCancel(WithOpenAIAccountCooldownBudget(t.Context()))
				defer cancel()
				started := time.Now()
				done := f.start(ctx, "gpt-6-astra")
				requireExcelBPSStickyPending(t, done)
				recovered := <-done
				require.NoError(t, recovered.err)
				require.Equal(t, 10*time.Second, recovered.at.Sub(started))
				require.EqualValues(t, 1, recovered.accountID)
				require.EqualValues(t, 1, f.owner(t))
				require.True(t, f.svc.openAIStickyWaitProtected(ctx, 1))
				time.Sleep(20 * time.Second)
				require.False(t, f.svc.openAIStickyWaitProtected(ctx, 1), "protection expires with the original request budget")
				f.assertNoHeldSlots(t)
			})
		})
	}
}

func TestExcelBPSStickyRecoveredOwnerStillHonorsHardFilters(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		t.Run(fmt.Sprintf("advanced=%t", advanced), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newExcelBPSStickyFixture(t, advanced)
				f.coolFor("10")
				ctx := WithOpenAIAccountCooldownBudget(t.Context())
				selection, _, err := f.svc.SelectAccountWithScheduler(ctx, &f.group, "", f.session, "gpt-6-astra", nil, OpenAIUpstreamTransportAny, false)
				require.ErrorIs(t, err, ErrOpenAIAccountCooldownAdmissionInterrupted)
				require.Nil(t, selection)
				waited, err := f.svc.WaitForOpenAIAccountCooldown(ctx)
				require.True(t, waited)
				require.NoError(t, err)
				require.True(t, f.svc.openAIStickyWaitProtected(ctx, 1))
				f.repo.accounts[0].Schedulable = false
				result := <-f.start(ctx, "gpt-6-astra")
				require.NoError(t, result.err)
				require.EqualValues(t, 2, result.accountID, "wait protection must not override a manual scheduling stop")
				require.EqualValues(t, 2, f.owner(t))
				f.assertNoHeldSlots(t)
			})
		})
	}
}

func TestExcelBPSStickyConcurrent429PreservesBindingAndReleasesSlot(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		for _, duringAcquire := range []bool{false, true} {
			t.Run(fmt.Sprintf("advanced=%t/during_acquire=%t", advanced, duringAcquire), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					f := newExcelBPSStickyFixture(t, advanced)
					trigger, cooled := make(chan struct{}), make(chan struct{})
					go func() { <-trigger; f.coolFor("10"); close(cooled) }()
					var once sync.Once
					interrupt := func() { once.Do(func() { close(trigger); <-cooled }) }
					defer interrupt()
					if duringAcquire {
						f.slots.onAcquire = func(id int64) {
							if id == 1 {
								interrupt()
							}
						}
					} else {
						f.bindings.onRead = func(n int32) {
							if n == 2 {
								interrupt()
							}
						}
					}
					ctx := WithOpenAIAccountCooldownBudget(t.Context())
					selection, decision, err := f.svc.SelectAccountWithScheduler(ctx, &f.group, "", f.session, "gpt-6-astra", nil, OpenAIUpstreamTransportAny, false)
					if selection != nil && selection.ReleaseFunc != nil {
						defer selection.ReleaseFunc()
					}
					require.ErrorIs(t, err, ErrOpenAIAccountCooldownAdmissionInterrupted)
					require.Nil(t, selection)
					require.Zero(t, decision.SelectedAccountID)
					require.Empty(t, decision.SelectedAccountType)
					require.False(t, decision.StickySessionHit)
					require.False(t, decision.StickyPreviousHit)
					require.EqualValues(t, 1, f.owner(t), "a racing cooldown must prevent deletion or replacement of the original binding")
					selectedID := int64(2)
					if duringAcquire {
						selectedID = 1
					}
					acquired, released := f.slots.counts(selectedID)
					require.Positive(t, acquired, "exercise an actual reserved slot")
					require.Equal(t, acquired, released, "interrupted selection must release its reserved slot")
					f.assertNoHeldSlots(t)
					started := time.Now()
					waited, err := f.svc.WaitForOpenAIAccountCooldown(ctx)
					require.True(t, waited)
					require.NoError(t, err)
					require.Equal(t, 10*time.Second, time.Since(started))
					result := <-f.start(ctx, "gpt-6-astra")
					require.NoError(t, result.err)
					require.EqualValues(t, 1, result.accountID)
					f.assertNoHeldSlots(t)
				})
			})
		}
	}
}
