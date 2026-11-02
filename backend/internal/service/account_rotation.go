package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

var ErrAccountRotationUnavailable = errors.New("account is draining or resting")

// AccountRotationPolicy uses seconds and a persisted settings revision so all
// gateway processes agree on the same work cycle after a configuration change.
type AccountRotationPolicy struct {
	Mode                                                        string
	WorkSeconds, RestMinSeconds, RestMaxSeconds, StaggerPercent int
	Revision                                                    string
}

type AccountRotationState struct {
	Phase       string `json:"phase"`
	StartedAt   int64  `json:"started_at"`
	DrainAt     int64  `json:"drain_at"`
	RestUntil   int64  `json:"rest_until"`
	IdleSince   int64  `json:"idle_since"`
	RestSeconds int    `json:"rest_seconds"`
}

type AccountRotationCache interface {
	CheckAccountRotation(context.Context, int64, AccountRotationPolicy) (*AccountRotationState, error)
	AcquireAccountSlotWithRotation(context.Context, int64, int, string, AccountRotationPolicy, bool, string) (bool, error)
	RefreshAccountRotationSlot(context.Context, int64, string) error
}

type accountRotationSettingsProvider interface {
	GetAccountRotationSettings(context.Context) (*AccountRotationSettings, error)
}

type accountRotationStickyKey struct{}
type accountRotationTrackingKey struct{}

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
	if settings == nil || !settings.Enabled {
		return nil, nil
	}
	if _, ok := s.cache.(AccountRotationCache); !ok {
		return nil, errors.New("account rotation requires Redis concurrency authority")
	}
	policy := &AccountRotationPolicy{
		Mode: settings.Mode, WorkSeconds: settings.WorkMinutes * 60,
		RestMinSeconds: settings.RestMinMinutes * 60, RestMaxSeconds: settings.RestMaxMinutes * 60,
		StaggerPercent: settings.StaggerPercent, Revision: settings.Revision,
	}
	if policy.Revision == "" {
		policy.Revision = fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%+v", *policy))))
	}
	return policy, nil
}

func rotationStateAllows(state *AccountRotationState, mode string, sticky bool) bool {
	if state == nil {
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
	state, err := s.cache.(AccountRotationCache).CheckAccountRotation(ctx, account.ID, *policy)
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
	states := make(map[int64]*AccountRotationState, len(ids))
	for _, id := range ids {
		state, err := s.cache.(AccountRotationCache).CheckAccountRotation(ctx, id, policy)
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
	return s.accountRotationStates(ctx, ids, *policy)
}

type accountRotationPool struct {
	name    string
	expires time.Time
}

func (s *ConcurrencyService) accountRotationPool(ctx context.Context, id int64) (string, error) {
	if item, ok := s.rotationPools.Load(id); ok {
		if pool := item.(accountRotationPool); time.Now().Before(pool.expires) {
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
	acquired, err := cache.AcquireAccountSlotWithRotation(ctx, accountID, maxConcurrency, requestID, policy, AccountRotationSticky(ctx, accountID), pool)
	if err != nil {
		return nil, err
	}
	if !acquired {
		state, err := cache.CheckAccountRotation(ctx, accountID, policy)
		if err != nil {
			return nil, err
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
