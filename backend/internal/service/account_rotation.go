package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

var ErrAccountRotationUnavailable = errors.New("account is draining or resting")

// AccountRotationPolicy uses seconds for work/rest and milliseconds for recovery
// pacing. The revision identifies the shared policy across gateway processes.
type AccountRotationPolicy struct {
	Mode                                                        string
	WorkSeconds, RestMinSeconds, RestMaxSeconds, StaggerPercent int
	EarlyRestEnabled                                            bool
	ScheduledHoldEnabled                                        bool
	EarlyRestWindowSeconds, EarlyRestConcurrencyThreshold       int
	EarlyRestSeconds                                            int
	RecoveryMinIntervalMS, RecoveryMaxIntervalMS                int
	Revision                                                    string
}

type AccountRotationState struct {
	AutomaticEnabled        bool   `json:"automatic_enabled"`
	LastManualRestAt        int64  `json:"last_manual_rest_at"`
	LastAutomaticRestAt     int64  `json:"last_automatic_rest_at"`
	LastAutomaticRestReason string `json:"last_automatic_rest_reason"`
	Phase                   string `json:"phase"`
	StartedAt               int64  `json:"started_at"`
	DrainAt                 int64  `json:"drain_at"`
	RestUntil               int64  `json:"rest_until"`
	IdleSince               int64  `json:"idle_since"`
	RestSeconds             int    `json:"rest_seconds"`
	Reason                  string `json:"reason,omitempty"`
	Hold                    bool   `json:"hold"`
	WaitingCount            int    `json:"waiting_count"`
	EarlyEligibleAt         int64  `json:"early_eligible_at,omitempty"`
	Recovering              bool   `json:"recovering"`
	RetryAfterMS            int64  `json:"retry_after_ms,omitempty"`
}

type AccountRotationCache interface {
	CheckAccountRotation(context.Context, int64, AccountRotationPolicy) (*AccountRotationState, error)
	AcquireAccountSlotWithRotation(context.Context, int64, int, string, AccountRotationPolicy, bool, string) (bool, error)
	RefreshAccountRotationSlot(context.Context, int64, string) error
}

// Hold support is optional so other concurrency authorities can keep serving
// normally when automatic rotation is disabled.
type AccountRotationHoldCache interface {
	AccountRotationCache
	SetAccountRotationHold(context.Context, int64, int, AccountRotationPolicy) (*AccountRotationState, error)
	CancelAccountRotationHold(context.Context, int64, AccountRotationPolicy) (*AccountRotationState, error)
	RefreshAccountRotationWaiter(context.Context, int64, string) error
	RemoveAccountRotationWaiter(context.Context, int64, string) error
	AcquireAccountSlotWithRotationState(context.Context, int64, int, string, AccountRotationPolicy, bool, string) (bool, *AccountRotationState, error)
}

var ErrAccountRotationUnsupported = infraerrors.BadRequest("ACCOUNT_ROTATION_UNSUPPORTED", "account rest requires Redis concurrency authority")

type accountRotationSettingsProvider interface {
	GetAccountRotationSettings(context.Context) (*AccountRotationSettings, error)
}

type accountRotationStickyKey struct{}
type accountRotationTrackingKey struct{}
type accountRotationWaitContextKey struct{}

// Ordinary capacity waits keep their own deadline. Only a confirmed HOLD may
// wait longer, and it must still stop when the original request is canceled.
func WithAccountRotationWaitContext(queueCtx, requestCtx context.Context) context.Context {
	return context.WithValue(queueCtx, accountRotationWaitContextKey{}, requestCtx)
}

type accountRotationHoldContext struct {
	context.Context
	request context.Context
}

func (c accountRotationHoldContext) Deadline() (time.Time, bool) { return c.request.Deadline() }
func (c accountRotationHoldContext) Done() <-chan struct{}       { return c.request.Done() }
func (c accountRotationHoldContext) Err() error                  { return c.request.Err() }

func accountRotationHoldingContext(ctx context.Context) context.Context {
	if request, ok := ctx.Value(accountRotationWaitContextKey{}).(context.Context); ok {
		// Preserve scheduler proof and tracing values, while removing only the
		// ordinary queue's deadline/cancellation. The request remains the owner.
		return accountRotationHoldContext{Context: context.WithoutCancel(ctx), request: request}
	}
	return ctx
}

func (s *ConcurrencyService) AccountRotationContext(ctx context.Context) context.Context {
	policy, err := s.accountRotationPolicy(ctx)
	if err == nil && policy != nil {
		return context.WithValue(ctx, accountRotationTrackingKey{}, true)
	}
	return ctx
}

func HasAccountRotationTracking(ctx context.Context) bool {
	v, _ := ctx.Value(accountRotationTrackingKey{}).(bool)
	return v
}

// Only server-side lookups of an existing binding may grant this proof. A
// session hash supplied by a client, or a newly created binding, is not proof.
func WithAccountRotationSticky(ctx context.Context, accountIDs ...int64) context.Context {
	ids := make(map[int64]bool)
	if existing, ok := ctx.Value(accountRotationStickyKey{}).(map[int64]bool); ok {
		for id := range existing {
			ids[id] = true
		}
	}
	for _, id := range accountIDs {
		if id > 0 {
			ids[id] = true
		}
	}
	return context.WithValue(ctx, accountRotationStickyKey{}, ids)
}

func AccountRotationSticky(ctx context.Context, accountID int64) bool {
	ids, _ := ctx.Value(accountRotationStickyKey{}).(map[int64]bool)
	return ids[accountID]
}

func ContextWithAccountRotationSelection(ctx context.Context, account *Account) context.Context {
	if account != nil && account.rotationTracked {
		ctx = context.WithValue(ctx, accountRotationTrackingKey{}, true)
	}
	if account != nil && account.rotationSticky {
		return WithAccountRotationSticky(ctx, account.ID)
	}
	return ctx
}

// MarkAccountRotationSelection copies per-request proof without mutating an
// account shared by a scheduler snapshot or another request.
func MarkAccountRotationSelection(ctx context.Context, account *Account) *Account {
	if account == nil {
		return nil
	}
	copy := *account
	copy.rotationSticky = AccountRotationSticky(ctx, account.ID)
	copy.rotationTracked = HasAccountRotationTracking(ctx)
	return &copy
}

// Auxiliary gateway requests (token counting, model metadata, cached contents)
// normally bypass generation limits. When rotation is enabled they still need
// a real account slot, so they cannot wake a resting account or hide activity.
func (s *ConcurrencyService) AcquireAccountRotationOnly(ctx context.Context, account *Account) (func(), error) {
	policy, err := s.accountRotationPolicy(ctx)
	if err != nil {
		return nil, err
	}
	if policy == nil {
		return func() {}, nil
	}
	if account == nil {
		return nil, ErrAccountNotFound
	}
	ctx = ContextWithAccountRotationSelection(ctx, account)
	result, err := s.acquireAccountRotationSlot(ctx, account.ID, 0, *policy)
	if err != nil {
		return nil, err
	}
	if !result.Acquired {
		return nil, ErrAccountRotationUnavailable
	}
	return result.ReleaseFunc, nil
}

func (s *ConcurrencyService) SetAccountRotation(settings accountRotationSettingsProvider, accounts AccountRepository) {
	s.rotationSettings = settings
	s.rotationAccounts = accounts
}

func (s *ConcurrencyService) accountRotationPolicy(ctx context.Context) (*AccountRotationPolicy, error) {
	if s == nil || s.rotationSettings == nil {
		return nil, nil
	}
	settings, err := s.rotationSettings.GetAccountRotationSettings(ctx)
	if err != nil {
		return nil, err
	}
	if settings == nil {
		return nil, nil
	}
	_, holdSupported := s.cache.(AccountRotationHoldCache)
	if !settings.Enabled && !holdSupported {
		return nil, nil
	}
	if _, ok := s.cache.(AccountRotationCache); !ok {
		return nil, errors.New("account rotation requires Redis concurrency authority")
	}
	policy := &AccountRotationPolicy{
		Mode: settings.Mode, WorkSeconds: settings.WorkMinutes * 60,
		RestMinSeconds: settings.RestMinMinutes * 60, RestMaxSeconds: settings.RestMaxMinutes * 60,
		StaggerPercent: settings.StaggerPercent, Revision: settings.Revision,
		EarlyRestEnabled:              settings.Enabled && settings.EarlyRestEnabled,
		ScheduledHoldEnabled:          settings.ScheduledHoldEnabled,
		EarlyRestWindowSeconds:        settings.EarlyRestWindowMinutes * 60,
		EarlyRestConcurrencyThreshold: settings.EarlyRestConcurrencyThreshold,
		EarlyRestSeconds:              settings.EarlyRestSeconds,
		RecoveryMinIntervalMS:         settings.RecoveryMinIntervalMS,
		RecoveryMaxIntervalMS:         settings.RecoveryMaxIntervalMS,
	}
	if !settings.Enabled {
		// Keep real requests visible for a manual rest, including unlimited
		// accounts, without starting an automatic work window.
		policy.WorkSeconds = 0
	}
	if policy.Revision == "" {
		policy.Revision = fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%+v", *policy))))
	}
	return policy, nil
}

func rotationStateAllows(state *AccountRotationState, mode string, sticky bool) bool {
	if state == nil || state.Hold {
		// Hold accounts remain selectable. Their acquisition waits on the same
		// account rather than sending the connection to a fallback account.
		return true
	}
	switch state.Phase {
	case "resting":
		return false
	case "draining":
		return mode == "sticky" && sticky
	default:
		return true
	}
}

func (s *ConcurrencyService) AccountRotationAllowed(ctx context.Context, account *Account, sticky bool) bool {
	if account == nil {
		return false
	}
	policy, err := s.accountRotationPolicy(ctx)
	if err != nil {
		return false
	}
	if policy == nil {
		return true
	}
	cache, ok := s.cache.(AccountRotationCache)
	if !ok {
		return false
	}
	state, err := cache.CheckAccountRotation(ctx, account.ID, *policy)
	return err == nil && rotationStateAllows(state, policy.Mode, sticky)
}

// Filtering is advisory; acquisition repeats the decision atomically with the
// real slot reservation. No persistent schedulable/status flags are changed.
func (s *ConcurrencyService) FilterAccountRotation(ctx context.Context, accounts []Account) ([]Account, error) {
	policy, err := s.accountRotationPolicy(ctx)
	if err != nil {
		return nil, err
	}
	if policy == nil {
		return accounts, nil
	}
	ids := make([]int64, len(accounts))
	for i := range accounts {
		ids[i] = accounts[i].ID
	}
	states, err := s.accountRotationStates(ctx, ids, *policy)
	if err != nil {
		return nil, err
	}
	filtered := make([]Account, 0, len(accounts))
	for i := range accounts {
		a := &accounts[i]
		if rotationStateAllows(states[a.ID], policy.Mode, AccountRotationSticky(ctx, a.ID)) {
			filtered = append(filtered, *a)
		}
	}
	return filtered, nil
}

func (s *ConcurrencyService) accountRotationStates(ctx context.Context, ids []int64, policy AccountRotationPolicy) (map[int64]*AccountRotationState, error) {
	if batch, ok := s.cache.(interface {
		CheckAccountRotationBatch(context.Context, []int64, AccountRotationPolicy) (map[int64]*AccountRotationState, error)
	}); ok {
		return batch.CheckAccountRotationBatch(ctx, ids, policy)
	}
	cache, ok := s.cache.(AccountRotationCache)
	if !ok {
		return nil, ErrAccountRotationUnsupported
	}
	states := make(map[int64]*AccountRotationState, len(ids))
	for _, id := range ids {
		state, err := cache.CheckAccountRotation(ctx, id, policy)
		if err != nil {
			return nil, err
		}
		states[id] = state
	}
	return states, nil
}

func (s *ConcurrencyService) GetAccountRotationStates(ctx context.Context, ids []int64) (map[int64]*AccountRotationState, error) {
	policy, err := s.accountRotationPolicy(ctx)
	if err != nil {
		return nil, err
	}
	if policy == nil {
		return map[int64]*AccountRotationState{}, nil
	}
	states, err := s.accountRotationStates(ctx, ids, *policy)
	if err == nil {
		for id, state := range states {
			if state != nil {
				state.AutomaticEnabled = policy.WorkSeconds > 0
			}
			if policy.WorkSeconds == 0 && (state == nil || (!state.Hold && !state.Recovering && state.WaitingCount == 0 &&
				state.LastManualRestAt == 0 && state.LastAutomaticRestAt == 0)) {
				delete(states, id)
			}
		}
	}
	return states, err
}

func (s *ConcurrencyService) StartAccountManualRest(ctx context.Context, accountID int64, minutes int) (*AccountRotationState, error) {
	if accountID <= 0 || minutes < 1 || minutes > 120 {
		return nil, infraerrors.BadRequest("INVALID_ACCOUNT_REST", "rest minutes must be between 1 and 120")
	}
	return s.StartAccountManualRestSeconds(ctx, accountID, minutes*60)
}

func (s *ConcurrencyService) StartAccountManualRestSeconds(ctx context.Context, accountID int64, seconds int) (*AccountRotationState, error) {
	if accountID <= 0 || seconds < 1 || seconds > 7200 {
		return nil, infraerrors.BadRequest("INVALID_ACCOUNT_REST", "rest seconds must be between 1 and 7200")
	}
	cache, policy, err := s.accountRotationHoldPolicy(ctx)
	if err != nil {
		return nil, err
	}
	return cache.SetAccountRotationHold(ctx, accountID, seconds, *policy)
}

func (s *ConcurrencyService) CancelAccountManualRest(ctx context.Context, accountID int64) (*AccountRotationState, error) {
	cache, policy, err := s.accountRotationHoldPolicy(ctx)
	if err != nil {
		return nil, err
	}
	return cache.CancelAccountRotationHold(ctx, accountID, *policy)
}

func (s *ConcurrencyService) accountRotationHoldPolicy(ctx context.Context) (AccountRotationHoldCache, *AccountRotationPolicy, error) {
	if s == nil {
		return nil, nil, ErrAccountRotationUnsupported
	}
	cache, ok := s.cache.(AccountRotationHoldCache)
	if !ok {
		return nil, nil, ErrAccountRotationUnsupported
	}
	policy, err := s.accountRotationPolicy(ctx)
	if err != nil {
		return nil, nil, err
	}
	if policy == nil {
		return nil, nil, ErrAccountRotationUnsupported
	}
	return cache, policy, nil
}

type accountRotationPool struct {
	name    string
	expires time.Time
}

func (s *ConcurrencyService) accountRotationPool(ctx context.Context, id int64) (string, error) {
	if item, ok := s.rotationPools.Load(id); ok {
		if pool, ok := item.(accountRotationPool); ok && time.Now().Before(pool.expires) {
			return pool.name, nil
		}
	}
	if s.rotationAccounts == nil {
		return "default", nil
	}
	account, err := s.rotationAccounts.GetByID(ctx, id)
	if err != nil {
		return "", err
	}
	if account == nil {
		return "", ErrAccountNotFound
	}
	groups := append([]int64(nil), account.GroupIDs...)
	sort.Slice(groups, func(i, j int) bool { return groups[i] < groups[j] })
	// Identical platform/group membership forms a pool of likely substitutes.
	name := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s:%v", account.Platform, groups))))
	s.rotationPools.Store(id, accountRotationPool{name, time.Now().Add(time.Minute)})
	return name, nil
}

func (s *ConcurrencyService) acquireAccountRotationSlot(ctx context.Context, accountID int64, maxConcurrency int, policy AccountRotationPolicy) (*AcquireResult, error) {
	cache := s.cache.(AccountRotationCache)
	pool, err := s.accountRotationPool(ctx, accountID)
	if err != nil {
		return nil, err
	}
	requestID := generateRequestID()
	waiting := false
	holdCache, supportsHold := cache.(AccountRotationHoldCache)
	defer func() {
		if waiting {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := holdCache.RemoveAccountRotationWaiter(cleanupCtx, accountID, requestID); err != nil {
				logger.LegacyPrintf("service.concurrency", "account rest waiter cleanup failed for %d: %v", accountID, err)
			}
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var acquired bool
		var state *AccountRotationState
		if supportsHold {
			acquired, state, err = holdCache.AcquireAccountSlotWithRotationState(ctx, accountID, maxConcurrency, requestID, policy, AccountRotationSticky(ctx, accountID), pool)
		} else {
			acquired, err = cache.AcquireAccountSlotWithRotation(ctx, accountID, maxConcurrency, requestID, policy, AccountRotationSticky(ctx, accountID), pool)
			if err == nil && !acquired {
				state, err = cache.CheckAccountRotation(ctx, accountID, policy)
			}
		}
		if err != nil {
			return nil, err
		}
		if acquired {
			break
		}
		if supportsHold && ((state != nil && (state.Hold || state.Recovering)) || waiting) {
			if !waiting {
				ctx = accountRotationHoldingContext(ctx)
			}
			// Waiters are leases of their own. They never occupy a real account
			// slot, keep a work window alive, or move the rest deadline.
			waiting = true
			if err := holdCache.RefreshAccountRotationWaiter(ctx, accountID, requestID); err != nil {
				return nil, err
			}
			// Redis reserves the next admission time atomically. Sleeping here
			// only avoids polling; independent per-request jitter would still
			// allow a burst across processes. Cap sleeps to renew waiter leases.
			delay := time.Second
			if state != nil && state.Recovering && state.RetryAfterMS > 0 {
				delay = min(time.Duration(state.RetryAfterMS)*time.Millisecond, 20*time.Second)
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
			// Do not restore stale settings while a request waits for minutes.
			next, err := s.accountRotationPolicy(ctx)
			if err != nil {
				return nil, err
			}
			if next != nil {
				policy = *next
			}
			continue
		}
		if !rotationStateAllows(state, policy.Mode, AccountRotationSticky(ctx, accountID)) {
			return nil, ErrAccountRotationUnavailable
		}
		return &AcquireResult{}, nil
	}
	// Long generations remain visible until the forwarding owner releases them.
	// Request cancellation alone must not stop renewal before upstream drains.
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				refreshCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err := cache.RefreshAccountRotationSlot(refreshCtx, accountID, requestID)
				cancel()
				if err != nil {
					logger.LegacyPrintf("service.concurrency", "account rotation slot renewal failed for %d: %v", accountID, err)
				}
			}
		}
	}()
	release := sync.OnceFunc(func() {
		close(stop)
		<-done
		bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.cache.ReleaseAccountSlot(bg, accountID, requestID); err != nil {
			logger.LegacyPrintf("service.concurrency", "account rotation slot release failed for %d: %v", accountID, err)
		}
	})
	return &AcquireResult{Acquired: true, RequestID: requestID, ReleaseFunc: release}, nil
}
