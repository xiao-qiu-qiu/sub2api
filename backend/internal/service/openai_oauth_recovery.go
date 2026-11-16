package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
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
	ErrOAuthRecoveryCredentialsRejected  = errors.New("recovered OAuth credentials were rejected")
	errOAuthRecoveryCredentialsChanged   = errors.New("verified OAuth credentials changed before publication")
)

// OpenAIOAuthRecoveryService keeps request waiting separate from durable login
// ownership. Disconnecting a waiter never cancels the account's login task.
type OpenAIOAuthRecoveryService interface {
	RequestImmediateReauth(context.Context, *Account) (*OpenAIOAuthReauthTask, error)
	GetImmediateReauthTask(context.Context, int64) (*OpenAIOAuthReauthTask, error)
	GetImmediateReauthTaskByID(context.Context, int64) (*OpenAIOAuthReauthTask, error)
	VerifyImmediateReauth(context.Context, int64) error
}

type openAIOAuthRecoveryAvailability interface {
	CheckImmediateReauthAvailable(context.Context, int64) error
}

type openAIOAuthRecoverySnapshotVerifier interface {
	VerifyImmediateReauthSnapshot(context.Context, int64) (*Account, error)
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
	taskID       int64
	waiters      int
	finished     time.Time
	observeUntil time.Time
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
	}
	r.mu.Unlock()
	go func() {
		defer r.wg.Done()
		r.observe(flight, task)
	}()
	return flight, true
}

// newFlight is called with r.mu held. Login ownership is shared, but its age
// must not shorten a new request's own recovery budget.
func (r *openAIOAuthRecoveryCoordinator) newFlight(accountID int64, token string, task *OpenAIOAuthReauthTask) *oauthRecoveryFlight {
	flight := &oauthRecoveryFlight{
		accountID: accountID, token: token, done: make(chan struct{}),
		observeUntil: time.Now().Add(r.waitTime),
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

func (r *openAIOAuthRecoveryCoordinator) finishIfIdle(flight *oauthRecoveryFlight) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if flight.waiters != 0 || time.Now().Before(flight.observeUntil) {
		return false
	}
	flight.err = ErrOAuthRecoveryTimeout
	flight.finished = time.Now()
	close(flight.done)
	return true
}

func (r *openAIOAuthRecoveryCoordinator) observe(flight *oauthRecoveryFlight, task *OpenAIOAuthReauthTask) {
	ctx, cancel := context.WithTimeout(r.ctx, openAIOAuthReauthStaleAfter)
	defer cancel()
	ticker := time.NewTicker(r.pollTime)
	defer ticker.Stop()
	var nextVerify time.Time
	verifiedVersion, rejectedToken := "", ""
	retryDelay := 2 * time.Second
	lastDiagnostic := ""
	retry := func(stage string, err error) {
		reason := infraerrors.Reason(err)
		diagnostic := stage + ":" + reason
		if diagnostic != lastDiagnostic {
			// Provider bodies and credential-bearing error strings stay private.
			slog.Warn("openai_oauth_recovery_retry", "account_id", flight.accountID, "stage", stage, "error_reason", reason)
			lastDiagnostic = diagnostic
		}
		nextVerify = time.Now().Add(retryDelay)
		retryDelay *= 2
		if retryDelay > 5*time.Second {
			retryDelay = 5 * time.Second
		}
	}
	for {
		if r.finishIfIdle(flight) {
			return
		}
		account, err := r.recoveryAccount(ctx, flight.accountID)
		if errors.Is(err, ErrOAuthRecoveryUnavailable) {
			slog.Info("openai_oauth_recovery_stopped", "account_id", flight.accountID, "stage", "availability")
			r.finish(flight, fmt.Errorf("%w: %w", ErrOAuthRecoveryFailed, err))
			return
		}
		if err != nil {
			retry("availability", err)
		} else if !time.Now().Before(nextVerify) {
			version, _ := hashReauthCredentials(account.Credentials)
			if verifiedVersion != "" && verifiedVersion != version {
				verifiedVersion = ""
			}
			ready := task == nil
			if task != nil {
				current, taskErr := r.guard.GetImmediateReauthTaskByID(ctx, task.ID)
				if taskErr != nil {
					retry("task_read", taskErr)
				} else if current != nil {
					ready = current.Status == OpenAIOAuthReauthStatusSucceeded
					if current.Status == OpenAIOAuthReauthStatusFailed || rejectedToken != "" {
						// A failed attempt can be followed by another owner/worker's
						// task. Keep the request's deadline while observing that work.
						next, lookupErr := r.guard.GetImmediateReauthTask(ctx, flight.accountID)
						if lookupErr != nil {
							retry("task_discovery", lookupErr)
						} else if next != nil && next.ID > task.ID {
							task = next
							verifiedVersion, rejectedToken = "", ""
							ready = false
							r.mu.Lock()
							flight.taskID = task.ID
							r.mu.Unlock()
						} else {
							ready = account.GetCredential("access_token") != flight.token
							retry("login_attempt", nil)
						}
					}
				}
			}
			if ready && verifiedVersion == "" && (rejectedToken == "" || rejectedToken != account.GetCredential("access_token")) {
				r.mu.Lock()
				verifyDeadline := flight.observeUntil
				r.mu.Unlock()
				if timeout := time.Now().Add(30 * time.Second); timeout.Before(verifyDeadline) {
					verifyDeadline = timeout
				}
				verifyCtx, verifyCancel := context.WithDeadline(ctx, verifyDeadline)
				verifiedAccount := account
				var verifyErr error
				if verifier, ok := r.guard.(openAIOAuthRecoverySnapshotVerifier); ok {
					verifiedAccount, verifyErr = verifier.VerifyImmediateReauthSnapshot(verifyCtx, flight.accountID)
				} else {
					verifyErr = r.guard.VerifyImmediateReauth(verifyCtx, flight.accountID)
				}
				verifyCancel()
				if errors.Is(verifyErr, ErrOAuthRecoveryUnavailable) {
					r.finish(flight, fmt.Errorf("%w: %w", ErrOAuthRecoveryFailed, verifyErr))
					return
				}
				if verifyErr != nil {
					if errors.Is(verifyErr, ErrOAuthRecoveryCredentialsRejected) {
						rejectedToken = account.GetCredential("access_token")
					}
					retry("credential_verification", verifyErr)
				} else {
					if verifiedAccount == nil {
						retry("account_refresh", ErrOAuthRecoveryFailed)
					} else {
						verifiedVersion, err = hashReauthCredentials(verifiedAccount.Credentials)
						if err != nil {
							retry("account_refresh", err)
						}
					}
				}
			}
			if verifiedVersion != "" {
				if err = r.publishRecovery(ctx, flight.accountID, verifiedVersion); errors.Is(err, ErrOAuthRecoveryUnavailable) {
					r.finish(flight, fmt.Errorf("%w: %w", ErrOAuthRecoveryFailed, err))
					return
				} else if err != nil {
					if errors.Is(err, errOAuthRecoveryCredentialsChanged) {
						verifiedVersion = ""
					}
					retry("cache_refresh", err)
				} else {
					r.finish(flight, nil)
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			r.finish(flight, fmt.Errorf("%w: %w", ErrOAuthRecoveryFailed, context.Cause(ctx)))
			return
		case <-ticker.C:
		}
	}
}

func (r *openAIOAuthRecoveryCoordinator) recoveryAccount(ctx context.Context, accountID int64) (*Account, error) {
	account, err := r.gateway.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if account == nil || !oauth401CanRecover(account, nil) || !account.Schedulable ||
		(account.Status != StatusActive && (account.Status != StatusError || account.ErrorMessage != ImmediateOAuthRecoveryErrorMessage)) {
		return nil, ErrOAuthRecoveryUnavailable
	}
	if availability, ok := r.guard.(openAIOAuthRecoveryAvailability); ok {
		if err := availability.CheckImmediateReauthAvailable(ctx, accountID); err != nil {
			return nil, err
		}
	}
	return account, nil
}

func (r *openAIOAuthRecoveryCoordinator) publishRecovery(ctx context.Context, accountID int64, verifiedVersion string) error {
	account, err := r.recoveryAccount(ctx, accountID)
	if err != nil {
		return err
	}
	if account.Status != StatusActive {
		return ErrOAuthRecoveryFailed
	}
	version, err := hashReauthCredentials(account.Credentials)
	if err != nil {
		return err
	}
	if version != verifiedVersion {
		return errOAuthRecoveryCredentialsChanged
	}
	// Credential recovery does not override quota, performance or quality gates.
	// Publish the fresh account first; normal selection applies those gates.
	if p := r.gateway.openAITokenProvider; p != nil && p.tokenCache != nil {
		if err := p.tokenCache.DeleteAccessToken(ctx, OpenAITokenCacheKey(account)); err != nil {
			return err
		}
	}
	if r.gateway.schedulerSnapshot != nil {
		return r.gateway.schedulerSnapshot.UpdateAccountInCache(ctx, account)
	}
	return nil
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
	deadline, _ := waitCtx.Deadline()
	// Reserve once per distinct account flight. A pool wait must neither consume
	// duplicate slots nor mutate the caller's pending/observed flight slices.
	joined := make([]*oauthRecoveryFlight, 0, len(flights))
	held := make(map[*oauthRecoveryFlight]bool)
	seen := make(map[*oauthRecoveryFlight]bool)
	queueFull := false
	observed := make([]*oauthRecoveryFlight, 0, len(flights))
	r.mu.Lock()
	for _, flight := range flights {
		if flight == nil || seen[flight] {
			continue
		}
		seen[flight] = true
		observed = append(observed, flight)
		if flight.finished.IsZero() {
			if flight.waiters >= openAIOAuthRecoveryMaxWaiters {
				queueFull = true
				continue
			}
			if deadline.After(flight.observeUntil) {
				flight.observeUntil = deadline
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
		var recovered *oauthRecoveryFlight
		r.mu.Lock()
		remaining := joined[:0]
		for _, flight := range joined {
			if !flight.finished.IsZero() {
				if flight.err == nil {
					recovered = flight
				}
			} else {
				remaining = append(remaining, flight)
				cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(flight.done)})
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
			if queueFull {
				return ErrOAuthRecoveryQueueFull
			}
			return ErrOAuthRecoveryFailed
		}
		reflect.Select(cases)
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
	if flight != nil && (flight.taskID > task.ID || (flight.taskID == task.ID && !errors.Is(flight.err, ErrOAuthRecoveryTimeout))) {
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
		if !account.Schedulable || (account.Status != StatusActive && (account.Status != StatusError || account.ErrorMessage != ImmediateOAuthRecoveryErrorMessage)) ||
			!s.openAIAccountMatchesSchedulingGroup(account, groupID) || (selection.requireCompact && openAICompactSupportTier(account) == 0) {
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
		if !discover && account.IsSchedulable() && !r.active(account.ID) && !s.isOpenAIAccountRuntimeBlocked(account) {
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
