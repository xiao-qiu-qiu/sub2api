package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"time"
)

const OpenAIOAuthRecoveryWait = time.Minute

var (
	ErrOAuthRecoveryTimeout              = errors.New("upstream authentication recovery timed out")
	ErrOAuthRecoveryFailed               = errors.New("upstream authentication recovery failed")
	ErrOAuthRecoveryAdmissionInterrupted = errors.New("leave concurrency admission to wait for OAuth recovery")
)

// OpenAIOAuthRecoveryService keeps request waiting separate from durable login
// ownership. Disconnecting a waiter never cancels the account's login task.
type OpenAIOAuthRecoveryService interface {
	RequestImmediateReauth(context.Context, *Account) (*OpenAIOAuthReauthTask, error)
	GetImmediateReauthTask(context.Context, int64) (*OpenAIOAuthReauthTask, error)
	GetImmediateReauthTaskByID(context.Context, int64) (*OpenAIOAuthReauthTask, error)
	VerifyImmediateReauth(context.Context, int64) error
}

type oauthRecoveryBudgetKey struct{}
type oauthRecoveryDiscoveryKey struct{}
type oauthRecoveryProbeKey struct{}
type oauthRecoveryAdmissionCheckKey struct{}
type oauthRecoverySelectionKey struct{}

type oauthRecoverySelection struct {
	excluded        map[int64]struct{}
	transport       OpenAIUpstreamTransport
	capability      OpenAIEndpointCapability
	imageCapability OpenAIImagesCapability
	requireCompact  bool
}

type oauthRecoveryBudget struct {
	mu       sync.Mutex
	deadline time.Time
	attempts map[int64]bool
	tokens   map[int64]string
	pending  []*oauthRecoveryFlight
}

// WithOpenAIOAuthRecoveryBudget shares one minute across all recovery waits in
// a request. The clock starts at the first recovery, not at request admission.
func WithOpenAIOAuthRecoveryBudget(ctx context.Context) context.Context {
	if _, ok := ctx.Value(oauthRecoveryBudgetKey{}).(*oauthRecoveryBudget); ok {
		return ctx
	}
	return NewOpenAIOAuthRecoveryBudget(ctx)
}

// NewOpenAIOAuthRecoveryBudget begins a new client turn on an existing socket.
// Replays of that turn must keep the returned context instead of resetting it.
func NewOpenAIOAuthRecoveryBudget(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauthRecoveryBudgetKey{}, &oauthRecoveryBudget{attempts: make(map[int64]bool), tokens: make(map[int64]string)})
}

func WithOAuthRecoveryAdmissionCheck(ctx context.Context, check func(context.Context) bool) context.Context {
	return context.WithValue(ctx, oauthRecoveryAdmissionCheckKey{}, check)
}

// CheckOAuthRecoveryAdmission never waits while holding a key ticket or slot.
// The owner cleans up admission, waits for recovery, then obtains fresh slots.
func CheckOAuthRecoveryAdmission(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	if check, ok := ctx.Value(oauthRecoveryAdmissionCheckKey{}).(func(context.Context) bool); ok {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if check(ctx) {
			return ErrOAuthRecoveryAdmissionInterrupted
		}
	}
	return nil
}

func recoveryBudget(ctx context.Context) *oauthRecoveryBudget {
	b, _ := ctx.Value(oauthRecoveryBudgetKey{}).(*oauthRecoveryBudget)
	return b
}

func (b *oauthRecoveryBudget) waitContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	b.mu.Lock()
	if b.deadline.IsZero() {
		b.deadline = time.Now().Add(timeout)
	}
	deadline := b.deadline
	b.mu.Unlock()
	return context.WithDeadline(ctx, deadline)
}

func (b *oauthRecoveryBudget) attempted(id int64, mark bool) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	previous := b.attempts[id]
	if mark {
		b.attempts[id] = true
	}
	return previous
}

func recordOAuthRecoveryToken(ctx context.Context, account *Account, token string) {
	if b := recoveryBudget(ctx); b != nil && oauth401CanRecover(account, nil) && token != "" {
		b.mu.Lock()
		b.tokens[account.ID] = token
		b.mu.Unlock()
	}
}

func oauthRecoveryRejectedToken(ctx context.Context, account *Account) string {
	if b := recoveryBudget(ctx); b != nil {
		b.mu.Lock()
		token := b.tokens[account.ID]
		b.mu.Unlock()
		if token != "" {
			return token
		}
	}
	return account.GetCredential("access_token")
}

type oauthRecoveryFlight struct {
	accountID int64
	token     string
	done      chan struct{}
	err       error // published by closing done
	finished  time.Time
}

type openAIOAuthRecoveryCoordinator struct {
	mu       sync.Mutex
	flights  map[int64]*oauthRecoveryFlight
	guard    OpenAIOAuthRecoveryService
	gateway  *OpenAIGatewayService
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	closed   bool
	waitTime time.Duration
	pollTime time.Duration
}

// SetOpenAIOAuthRecovery is called during wiring, before requests are served.
func (s *OpenAIGatewayService) SetOpenAIOAuthRecovery(guard OpenAIOAuthRecoveryService) {
	if s == nil || guard == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &openAIOAuthRecoveryCoordinator{
		flights: make(map[int64]*oauthRecoveryFlight), guard: guard, gateway: s,
		ctx: ctx, cancel: cancel, waitTime: OpenAIOAuthRecoveryWait, pollTime: 250 * time.Millisecond,
	}
	s.oauthRecovery = r
	if s.rateLimitService != nil {
		s.rateLimitService.oauthRecovery = r
	}
}

func (r *openAIOAuthRecoveryCoordinator) stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.closed = true
	r.cancel()
	r.mu.Unlock()
	r.wg.Wait()
}

func oauth401CanRecover(account *Account, body []byte) bool {
	if account == nil || !account.IsOpenAIOAuth() || account.IsOpenAIPersonalAccessToken() || account.IsOpenAIAgentIdentity() || account.IsExcelOAuth() || account.IsSIWCEnabled() || account.AuthConnectionID != "" {
		return false
	}
	if isOpenAIIPUnauthorizedResponse(http.StatusUnauthorized, body) || isOpenAIUpstreamSessionBlocked(body) {
		return false
	}
	code := credentialOperationEnvelope(openAIOAuthErrorEnvelope(body))
	return code == "unknown" || code == "requires_relogin"
}

func (r *openAIOAuthRecoveryCoordinator) begin(ctx context.Context, account *Account, body []byte) (*oauthRecoveryFlight, bool) {
	if r == nil || !oauth401CanRecover(account, body) || recoveryBudget(ctx).attempted(account.ID, false) {
		return nil, false
	}
	rejectedToken := oauthRecoveryRejectedToken(ctx, account)
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, false
	}
	// Keep completed flights briefly so concurrent, late responses for the old
	// token see the same outcome instead of starting another login.
	for id, flight := range r.flights {
		if !flight.finished.IsZero() && time.Since(flight.finished) > OpenAIOAuthRecoveryWait {
			delete(r.flights, id)
		}
	}
	if flight := r.flights[account.ID]; flight != nil {
		if flight.finished.IsZero() || flight.token == rejectedToken {
			r.mu.Unlock()
			return flight, true
		}
	}
	flight := &oauthRecoveryFlight{accountID: account.ID, token: rejectedToken, done: make(chan struct{})}
	r.flights[account.ID] = flight
	r.wg.Add(1)
	r.mu.Unlock()
	// Register first: concurrent requests can already wait while the task is
	// being inserted. Task creation is independent of a client disconnect.
	createCtx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	// TokenProvider may have returned a newer cached/refreshed token than the
	// scheduler snapshot. Bind the rejection to the token actually sent.
	snapshot := account
	current, readErr := r.gateway.accountRepo.GetByID(createCtx, account.ID)
	if readErr == nil && current != nil && current.GetCredential("access_token") == rejectedToken {
		snapshot = current
	} else if account.GetCredential("access_token") != rejectedToken {
		copy := *account
		copy.Credentials = make(map[string]any, len(account.Credentials))
		for key, value := range account.Credentials {
			copy.Credentials[key] = value
		}
		copy.Credentials["access_token"] = rejectedToken
		snapshot = &copy
	}
	task, err := r.guard.RequestImmediateReauth(createCtx, snapshot)
	cancel()
	if err != nil {
		r.finish(flight, fmt.Errorf("%w: %w", ErrOAuthRecoveryFailed, err))
		r.wg.Done()
		if errors.Is(err, ErrOAuthRecoveryUnavailable) {
			r.mu.Lock()
			if r.flights[account.ID] == flight {
				delete(r.flights, account.ID)
			}
			r.mu.Unlock()
			return nil, false
		}
		return flight, true
	}
	go func() {
		defer r.wg.Done()
		r.observe(flight, task)
	}()
	return flight, true
}

func (r *openAIOAuthRecoveryCoordinator) finish(flight *oauthRecoveryFlight, err error) {
	r.mu.Lock()
	flight.err = err
	flight.finished = time.Now()
	close(flight.done)
	r.mu.Unlock()
}

func (r *openAIOAuthRecoveryCoordinator) observe(flight *oauthRecoveryFlight, task *OpenAIOAuthReauthTask) {
	ctx, cancel := context.WithTimeout(r.ctx, openAIOAuthReauthStaleAfter)
	defer cancel()
	if task != nil {
		ticker := time.NewTicker(r.pollTime)
		defer ticker.Stop()
		for {
			current, err := r.guard.GetImmediateReauthTaskByID(ctx, task.ID)
			if err == nil && current != nil {
				if current.Status == OpenAIOAuthReauthStatusSucceeded {
					break
				}
				if current.Status == OpenAIOAuthReauthStatusFailed {
					r.finish(flight, ErrOAuthRecoveryFailed)
					return
				}
			}
			select {
			case <-ctx.Done():
				r.finish(flight, ErrOAuthRecoveryFailed)
				return
			case <-ticker.C:
			}
		}
	}
	verifyCtx, verifyCancel := context.WithTimeout(ctx, 30*time.Second)
	err := r.guard.VerifyImmediateReauth(verifyCtx, flight.accountID)
	verifyCancel()
	if err == nil {
		// Publish a fresh snapshot before waking waiters. The scheduler's usual
		// admission checks still decide whether this account can serve the turn.
		account, readErr := r.gateway.accountRepo.GetByID(ctx, flight.accountID)
		if readErr != nil || account == nil || !account.IsSchedulable() {
			err = ErrOAuthRecoveryFailed
		} else {
			if p := r.gateway.openAITokenProvider; p != nil && p.tokenCache != nil {
				err = p.tokenCache.DeleteAccessToken(ctx, OpenAITokenCacheKey(account))
			}
			if err == nil && r.gateway.schedulerSnapshot != nil {
				err = r.gateway.schedulerSnapshot.UpdateAccountInCache(ctx, account)
			}
		}
	}
	if err != nil {
		err = fmt.Errorf("%w: %w", ErrOAuthRecoveryFailed, err)
	}
	r.finish(flight, err)
}

func (r *openAIOAuthRecoveryCoordinator) active(accountID int64) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	flight := r.flights[accountID]
	return flight != nil && flight.finished.IsZero()
}

func (r *openAIOAuthRecoveryCoordinator) wait(ctx context.Context, flights []*oauthRecoveryFlight) error {
	ctx = WithOpenAIOAuthRecoveryBudget(ctx)
	budget := recoveryBudget(ctx)
	waitCtx, cancel := budget.waitContext(ctx, r.waitTime)
	defer cancel()
	cases := []reflect.SelectCase{{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(waitCtx.Done())}}
	for _, flight := range flights {
		cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(flight.done)})
	}
	for len(cases) > 1 {
		if waitCtx.Err() != nil {
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			return ErrOAuthRecoveryTimeout
		}
		chosen, _, _ := reflect.Select(cases)
		if chosen == 0 || waitCtx.Err() != nil {
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			return ErrOAuthRecoveryTimeout
		}
		flight := flights[chosen-1]
		if flight.err == nil {
			budget.attempted(flight.accountID, true)
			return nil
		}
		cases = append(cases[:chosen], cases[chosen+1:]...)
		flights = append(flights[:chosen-1], flights[chosen:]...)
	}
	return ErrOAuthRecoveryFailed
}

func (r *openAIOAuthRecoveryCoordinator) matching(ctx context.Context, account *Account) *oauthRecoveryFlight {
	if r == nil || account == nil {
		return nil
	}
	token := oauthRecoveryRejectedToken(ctx, account)
	r.mu.Lock()
	defer r.mu.Unlock()
	flight := r.flights[account.ID]
	if flight != nil && (flight.finished.IsZero() || flight.token == token) {
		return flight
	}
	return nil
}

// RecoverOpenAIOAuth401 is called only before semantic output is sent. It does
// not hold an account concurrency slot or replay the request itself.
func (s *OpenAIGatewayService) RecoverOpenAIOAuth401(ctx context.Context, account *Account, body []byte) (bool, error) {
	if s == nil || s.oauthRecovery == nil || !oauth401CanRecover(account, body) {
		return false, nil
	}
	owner, err := resolveCredentialAccount(ctx, s.accountRepo, account)
	if err != nil {
		return false, nil
	}
	ctx = WithOpenAIOAuthRecoveryBudget(ctx)
	if recoveryBudget(ctx).attempted(owner.ID, false) {
		return true, ErrOAuthRecoveryFailed
	}
	// Only the native credential error path may start a login. BPS/SIWC and
	// custom policy failures can also carry 401, but own independent grants.
	flight := s.oauthRecovery.matching(ctx, owner)
	if flight == nil {
		return false, nil
	}
	return true, s.oauthRecovery.wait(ctx, []*oauthRecoveryFlight{flight})
}

// WaitForOpenAIOAuthRecovery parks new requests when their account pool is
// recovering. Healthy alternatives keep serving normally. On a scheduler miss
// it also discovers tasks created by another process or before a restart.
func (s *OpenAIGatewayService) WaitForOpenAIOAuthRecovery(ctx context.Context, groupID *int64, model string) (bool, error) {
	var flights []*oauthRecoveryFlight
	if b := recoveryBudget(ctx); b != nil {
		b.mu.Lock()
		flights, b.pending = b.pending, nil
		b.mu.Unlock()
	}
	if len(flights) == 0 {
		flights = s.pendingOpenAIOAuthRecovery(ctx, groupID, model)
	}
	if len(flights) == 0 {
		return false, nil
	}
	return true, s.oauthRecovery.wait(ctx, flights)
}

func (s *OpenAIGatewayService) HasPendingOpenAIOAuthRecovery(ctx context.Context, groupID *int64, model string) bool {
	budget := recoveryBudget(ctx)
	if budget != nil {
		budget.mu.Lock()
		observed := len(budget.pending) > 0
		budget.mu.Unlock()
		if observed {
			return true
		}
	}
	flights := s.pendingOpenAIOAuthRecovery(ctx, groupID, model)
	if len(flights) == 0 {
		return false
	}
	if budget != nil {
		// Queue cleanup can overlap task completion. Keep this request's exact
		// observed flights so their failure cannot turn into "no accounts".
		budget.mu.Lock()
		budget.pending = append([]*oauthRecoveryFlight(nil), flights...)
		budget.mu.Unlock()
		_, cancel := budget.waitContext(ctx, s.oauthRecovery.waitTime)
		cancel()
	}
	return true
}

func (s *OpenAIGatewayService) pendingOpenAIOAuthRecovery(ctx context.Context, groupID *int64, model string) []*oauthRecoveryFlight {
	if s == nil || s.oauthRecovery == nil || s.accountRepo == nil {
		return nil
	}
	r := s.oauthRecovery
	discover, _ := ctx.Value(oauthRecoveryDiscoveryKey{}).(bool)
	r.mu.Lock()
	active := false
	for _, f := range r.flights {
		if f.finished.IsZero() {
			active = true
			break
		}
	}
	r.mu.Unlock()
	if !active && !discover {
		return nil
	}
	accounts, err := s.accountRepo.ListAllWithFilters(ctx, PlatformOpenAI, "", "active,error", "", derefGroupID(groupID), "")
	if err != nil {
		return nil
	}
	selection, hasSelection := ctx.Value(oauthRecoverySelectionKey{}).(oauthRecoverySelection)
	if !hasSelection {
		if forward, ok := openAIForwardModelFromContext(ctx); ok {
			selection.requireCompact = forward.useCompactModelMapping
		}
	}
	if s.checkChannelPricingRestriction(ctx, groupID, model) {
		return nil
	}
	ctx = s.withOpenAIQuotaAutoPauseContext(ctx)
	ctx = s.withOpenAIProfitControlGate(ctx, groupID)
	scheduler := &defaultOpenAIAccountScheduler{service: s}
	req := OpenAIAccountScheduleRequest{
		GroupID: groupID, Platform: PlatformOpenAI, RequestedModel: model,
		RequiredTransport: selection.transport, RequiredCapability: selection.capability,
		RequiredImageCapability: selection.imageCapability, RequireCompact: selection.requireCompact,
		RequirePrivacySet: s.openAIGroupRequiresPrivacySet(ctx, groupID),
	}
	var candidates []*Account
	for i := range accounts {
		account := &accounts[i]
		if !account.Schedulable || !s.openAIAccountMatchesSchedulingGroup(account, groupID) || (selection.requireCompact && openAICompactSupportTier(account) == 0) {
			continue
		}
		if hasSelection {
			if _, excluded := selection.excluded[account.ID]; excluded {
				continue
			}
			if !s.isOpenAIAccountTransportCompatible(account, selection.transport, model) {
				continue
			}
		}
		if compatible, _ := scheduler.accountRequestCompatibility(ctx, account, req, true); !compatible {
			continue
		}
		// After a real scheduler miss, a superficially healthy account may have
		// failed compact, affinity, capacity or other admission requirements.
		if !discover && account.IsSchedulable() && !s.isOpenAIAccountRuntimeBlocked(account) {
			return nil
		}
		if oauth401CanRecover(account, nil) {
			candidates = append(candidates, account)
		}
	}
	var flights []*oauthRecoveryFlight
	for _, account := range candidates {
		r.mu.Lock()
		flight := r.flights[account.ID]
		if flight != nil && !flight.finished.IsZero() {
			flight = nil
		}
		r.mu.Unlock()
		if flight == nil {
			task, taskErr := r.guard.GetImmediateReauthTask(ctx, account.ID)
			if taskErr != nil || task == nil {
				continue
			}
			r.mu.Lock()
			flight = r.flights[account.ID]
			if !r.closed && (flight == nil || !flight.finished.IsZero()) {
				flight = &oauthRecoveryFlight{accountID: account.ID, token: account.GetCredential("access_token"), done: make(chan struct{})}
				r.flights[account.ID] = flight
				r.wg.Add(1)
				go func(f *oauthRecoveryFlight, task *OpenAIOAuthReauthTask) {
					defer r.wg.Done()
					r.observe(f, task)
				}(flight, task)
			}
			r.mu.Unlock()
		}
		if flight != nil {
			flights = append(flights, flight)
		}
	}
	return flights
}

// FetchOpenAIRecoveryModelsList must verify the newly saved Codex credential,
// never a cached catalog or the independent Excel/BPS grant.
func (s *OpenAIGatewayService) FetchOpenAIRecoveryModelsList(ctx context.Context, account *Account) (*OpenAIModelsResponse, error) {
	return s.fetchNativeOpenAIModelsList(context.WithValue(ctx, oauthRecoveryProbeKey{}, true), account)
}
