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

const (
	OpenAIOAuthRecoveryWait       = time.Minute
	openAIOAuthRecoveryMaxWaiters = 100
)

var (
	ErrOAuthRecoveryTimeout              = errors.New("upstream authentication recovery timed out")
	ErrOAuthRecoveryFailed               = errors.New("upstream authentication recovery failed")
	ErrOAuthRecoveryQueueFull            = errors.New("upstream authentication recovery queue is full")
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
	routing  oauthRecoveryRouting
	observed []*oauthRecoveryFlight
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
	// The remaining fields are protected by the coordinator's mutex.
	taskID   int64
	deadline time.Time
	changed  chan struct{}
	waiters  int
	finished time.Time
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
	// Late responses retain the old outcome unless another owner has already
	// started a newer durable task. Looking for that task must not create one.
	if flight := r.flights[account.ID]; flight != nil {
		if flight.finished.IsZero() {
			r.mu.Unlock()
			return flight, true
		}
		if flight.token == rejectedToken {
			r.mu.Unlock()
			if current := r.discover(ctx, account, true); current != nil {
				return current, true
			}
			return flight, true
		}
	}
	flight := r.newFlight(account.ID, rejectedToken, nil)
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
	r.mu.Lock()
	if task != nil {
		flight.taskID = task.ID
		if deadline := r.taskDeadline(task); deadline.Before(flight.deadline) {
			flight.deadline = deadline
			close(flight.changed)
			flight.changed = make(chan struct{})
		}
	}
	r.mu.Unlock()
	go func() {
		defer r.wg.Done()
		r.observe(flight, task)
	}()
	return flight, true
}

func (r *openAIOAuthRecoveryCoordinator) taskDeadline(task *OpenAIOAuthReauthTask) time.Time {
	start := time.Now()
	if task != nil {
		created := task.CreatedAt
		if !created.IsZero() && created.Before(start) {
			start = created
		}
	}
	return start.Add(r.waitTime)
}

// newFlight is called with r.mu held. Discovery uses durable creation time;
// local creation starts its window before any task insertion or token lookup.
func (r *openAIOAuthRecoveryCoordinator) newFlight(accountID int64, token string, task *OpenAIOAuthReauthTask) *oauthRecoveryFlight {
	flight := &oauthRecoveryFlight{
		accountID: accountID, token: token, done: make(chan struct{}),
		deadline: r.taskDeadline(task), changed: make(chan struct{}),
	}
	if task != nil {
		flight.taskID = task.ID
	}
	return flight
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
	// Reserve once per distinct account flight. A pool wait must neither consume
	// duplicate slots nor mutate the caller's pending/observed flight slices.
	joined := make([]*oauthRecoveryFlight, 0, len(flights))
	held := make(map[*oauthRecoveryFlight]bool)
	seen := make(map[*oauthRecoveryFlight]bool)
	ready := make(map[*oauthRecoveryFlight]bool)
	queueFull, timedOut := false, false
	observed := make([]*oauthRecoveryFlight, 0, len(flights))
	r.mu.Lock()
	for _, flight := range flights {
		if flight == nil || seen[flight] {
			continue
		}
		seen[flight] = true
		observed = append(observed, flight)
		if !flight.finished.IsZero() {
			ready[flight] = true
		} else {
			if flight.waiters >= openAIOAuthRecoveryMaxWaiters {
				queueFull = true
				continue
			}
			flight.waiters++
			held[flight] = true
		}
		joined = append(joined, flight)
	}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		for flight := range held {
			flight.waiters--
		}
		r.mu.Unlock()
	}()
	budget.mu.Lock()
	// Include full queues too: the handler must know which account rejected
	// this wait. Replace earlier targets so a later failure cannot exclude a
	// different account whose recovery already succeeded in this request.
	budget.observed = observed
	budget.mu.Unlock()
	for {
		if waitCtx.Err() != nil {
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			return ErrOAuthRecoveryTimeout
		}
		cases := []reflect.SelectCase{{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(waitCtx.Done())}}
		var deadline time.Time
		var recovered *oauthRecoveryFlight
		now := time.Now()
		r.mu.Lock()
		remaining := joined[:0]
		for _, flight := range joined {
			finished := !flight.finished.IsZero()
			expired := !flight.deadline.After(now)
			if finished && (ready[flight] || !flight.finished.After(flight.deadline)) {
				if flight.err == nil {
					recovered = flight
				}
			} else if expired {
				timedOut = true
			} else {
				remaining = append(remaining, flight)
				if deadline.IsZero() || flight.deadline.Before(deadline) {
					deadline = flight.deadline
				}
				cases = append(cases,
					reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(flight.done)},
					reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(flight.changed)})
				continue
			}
			if held[flight] {
				flight.waiters--
				delete(held, flight)
			}
		}
		joined = remaining
		r.mu.Unlock()
		if recovered != nil {
			budget.attempted(recovered.accountID, true)
			return nil
		}
		if len(joined) == 0 {
			if timedOut {
				return ErrOAuthRecoveryTimeout
			}
			if queueFull {
				return ErrOAuthRecoveryQueueFull
			}
			return ErrOAuthRecoveryFailed
		}
		timer := time.NewTimer(time.Until(deadline))
		cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(timer.C)})
		reflect.Select(cases)
		timer.Stop()
	}
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

// discover only observes durable work; it never requests a new login. A newer
// durable task supersedes a completed outcome even with unchanged credentials.
func (r *openAIOAuthRecoveryCoordinator) discover(ctx context.Context, account *Account, reuseFinished bool) *oauthRecoveryFlight {
	rejectedToken := oauthRecoveryRejectedToken(ctx, account)
	r.mu.Lock()
	flight := r.flights[account.ID]
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	if flight != nil && flight.finished.IsZero() {
		r.mu.Unlock()
		return flight
	}
	r.mu.Unlock()
	task, err := r.guard.GetImmediateReauthTask(ctx, account.ID)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	flight = r.flights[account.ID]
	if flight != nil && flight.finished.IsZero() {
		return flight
	}
	if err != nil || task == nil || task.AccountID != account.ID {
		if reuseFinished && flight != nil && flight.token == rejectedToken {
			return flight
		}
		return nil
	}
	// Another discovery may have installed and completed a newer flight while
	// this lookup was in progress. Task IDs are monotonic; never move backwards.
	if flight != nil && flight.taskID >= task.ID {
		return flight
	}
	flight = r.newFlight(account.ID, rejectedToken, task)
	r.flights[account.ID] = flight
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.observe(flight, task)
	}()
	return flight
}

func (b *oauthRecoveryBudget) queue(flights []*oauthRecoveryFlight) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, flight := range flights {
		pending := false
		for _, previous := range b.pending {
			pending = pending || previous == flight
		}
		if !pending {
			b.pending = append(b.pending, flight)
		}
	}
	b.observed = append([]*oauthRecoveryFlight(nil), b.pending...)
}

// QueueOpenAIOAuthRecoveryForAccount is called after the scheduler has checked
// the selected account's eligibility. True tells admission to release its slots
// and call WaitForOpenAIOAuthRecovery, including for a completed failed round.
// It neither starts a login nor decides whether fallback is allowed.
func (s *OpenAIGatewayService) QueueOpenAIOAuthRecoveryForAccount(ctx context.Context, accountID int64) bool {
	if s == nil || s.oauthRecovery == nil || s.accountRepo == nil || oauthRecoveryAccountExcluded(ctx, accountID) {
		return false
	}
	budget := recoveryBudget(ctx)
	if budget == nil {
		return false
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil || account == nil {
		return false
	}
	flight := s.oauthRecovery.discover(ctx, account, true)
	if flight == nil {
		return false
	}
	budget.queue([]*oauthRecoveryFlight{flight})
	_, cancel := budget.waitContext(ctx, s.oauthRecovery.waitTime)
	cancel()
	return true
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
		budget.queue(flights)
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
		if oauthRecoveryAccountExcluded(ctx, account.ID) {
			continue
		}
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
		flight := r.discover(ctx, account, false)
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
