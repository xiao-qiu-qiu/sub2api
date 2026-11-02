package service

import (
	"context"
	"errors"
	"net/http"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// ErrOAuthRecoveryUnavailable identifies accounts for which the gateway must
// retain its ordinary upstream-error handling rather than wait for re-login.
var ErrOAuthRecoveryUnavailable = errors.New("automatic OAuth recovery is unavailable")

const ImmediateOAuthRecoveryErrorMessage = "OAuth credentials rejected (401); automatic re-login in progress"

// This optional extension keeps event recovery independent of inspection
// leases. Implementations must serialize with the durable automatic budget.
type AccountTokenGuardV2RecoveryRepository interface {
	RequestImmediateReauth(context.Context, int64, map[string]any, string, AccountTokenGuardV2Rules, time.Time) (*OpenAIOAuthReauthTaskRecord, error)
	GetImmediateReauthTask(context.Context, int64) (*OpenAIOAuthReauthTaskRecord, error)
	CompleteImmediateReauthVerification(context.Context, int64, map[string]any, AccountTokenGuardV2ProbeCompletion, AccountTokenGuardV2Rules) error
}

type accountTokenGuardV2RecoveryProber interface {
	FetchOpenAIRecoveryModelsList(context.Context, *Account) (*OpenAIModelsResponse, error)
}

func immediateOAuthRecoveryAccount(account *Account) bool {
	return account != nil && account.ID > 0 && account.Platform == PlatformOpenAI && account.Type == AccountTypeOAuth &&
		!account.IsCredentialShadow() && !account.IsOpenAIPersonalAccessToken() && !account.IsOpenAIAgentIdentity()
}

// RequestImmediateReauth consumes the credential snapshot used by the rejected
// business request. A nil task with no error means those credentials have
// already changed; callers must verify the current credentials before retrying.
// Enabled controls periodic inspections only; AutoReloginEnabled controls this
// event entry point. The repository alone decides task reuse and budget gates.
func (s *AccountTokenGuardV2Service) RequestImmediateReauth(ctx context.Context, account *Account) (*OpenAIOAuthReauthTask, error) {
	if s == nil || s.repo == nil || s.accounts == nil || s.reauth == nil || !immediateOAuthRecoveryAccount(account) {
		return nil, ErrOAuthRecoveryUnavailable
	}
	repo, ok := s.repo.(AccountTokenGuardV2RecoveryRepository)
	if !ok {
		return nil, ErrOAuthRecoveryUnavailable
	}
	record, err := s.repo.GetAccount(ctx, account.ID)
	if err != nil {
		return nil, err
	}
	if record == nil || !record.AutoReloginEnabled {
		return nil, ErrOAuthRecoveryUnavailable
	}
	current, err := s.accounts.GetAccount(ctx, account.ID)
	if err != nil {
		return nil, err
	}
	if !immediateOAuthRecoveryAccount(current) || !current.Schedulable || (current.Status != StatusActive && current.Status != StatusError) {
		return nil, ErrOAuthRecoveryUnavailable
	}
	expectedHash, err := hashReauthCredentials(account.Credentials)
	if err != nil {
		return nil, err
	}
	currentHash, err := hashReauthCredentials(current.Credentials)
	if err != nil {
		return nil, err
	}
	if expectedHash != currentHash {
		return nil, nil
	}
	rules, err := s.GetRules(ctx)
	if err != nil {
		return nil, err
	}
	var eventErr error
	task, err := s.reauth.createTaskForProfile(ctx, account.ID, "codex", func(createCtx context.Context, id int64, hash string) (*OpenAIOAuthReauthTaskRecord, error) {
		if hash != expectedHash {
			return nil, nil
		}
		var task *OpenAIOAuthReauthTaskRecord
		task, eventErr = repo.RequestImmediateReauth(createCtx, id, account.Credentials, expectedHash, rules, s.clockNow())
		return task, eventErr
	})
	if eventErr != nil {
		return nil, eventErr // Preserve sentinel identity through the manual-task validator.
	}
	if err != nil {
		switch infraerrors.Reason(err) {
		case "OPENAI_REAUTH_UNAVAILABLE", "OPENAI_REAUTH_ENCRYPTION_KEY_REQUIRED", "OPENAI_REAUTH_CONFIG_REQUIRED",
			"OPENAI_REAUTH_ACCOUNT_INVALID", "OPENAI_REAUTH_ACCOUNT_NOT_FOUND", "OPENAI_REAUTH_MODE_UNAVAILABLE", "OPENAI_REAUTH_WORKER_UNAVAILABLE":
			return nil, ErrOAuthRecoveryUnavailable
		}
		return nil, err
	}
	if task != nil {
		s.reauth.EnsureWorker()
	}
	return task, nil
}

// GetImmediateReauthTask discovers a recoverable in-flight task after restart
// or on a different gateway instance. It neither creates nor schedules work.
func (s *AccountTokenGuardV2Service) GetImmediateReauthTask(ctx context.Context, accountID int64) (*OpenAIOAuthReauthTask, error) {
	if s == nil || s.repo == nil || s.accounts == nil || accountID <= 0 {
		return nil, nil
	}
	repo, ok := s.repo.(AccountTokenGuardV2RecoveryRepository)
	if !ok {
		return nil, nil
	}
	account, err := s.accounts.GetAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if !immediateOAuthRecoveryAccount(account) || !account.Schedulable || (account.Status != StatusActive && account.Status != StatusError) {
		return nil, nil
	}
	task, err := repo.GetImmediateReauthTask(ctx, accountID)
	return safeReauthTask(task), err
}

// GetImmediateReauthTaskByID preserves the original task identity while the
// gateway waits, even if unrelated authorization tasks are queued later.
func (s *AccountTokenGuardV2Service) GetImmediateReauthTaskByID(ctx context.Context, taskID int64) (*OpenAIOAuthReauthTask, error) {
	if s == nil || s.reauth == nil || s.reauth.repo == nil {
		return nil, ErrOAuthRecoveryUnavailable
	}
	task, err := s.reauth.repo.GetTask(ctx, taskID)
	return safeReauthTask(task), err
}

type immediateOAuthFreshProbe struct {
	accountTokenGuardV2RecoveryProber
}

func (p immediateOAuthFreshProbe) FetchOpenAIModelsList(ctx context.Context, account *Account) (*OpenAIModelsResponse, error) {
	return p.FetchOpenAIRecoveryModelsList(ctx, account)
}

// VerifyImmediateReauth always makes a fresh native OAuth request. It never
// calls ProbeNow/runProbe, which may create another task and use a model cache.
// Accounting and probe persistence use a credential CAS, so an overlapping
// refresh cannot turn an old probe into proof that newer credentials work.
func (s *AccountTokenGuardV2Service) VerifyImmediateReauth(ctx context.Context, accountID int64) error {
	if s == nil || s.repo == nil || s.accounts == nil || s.prober == nil {
		return ErrOAuthRecoveryUnavailable
	}
	repo, ok := s.repo.(AccountTokenGuardV2RecoveryRepository)
	if !ok {
		return ErrOAuthRecoveryUnavailable
	}
	prober := s.prober
	if fresh, ok := prober.(accountTokenGuardV2RecoveryProber); ok {
		prober = immediateOAuthFreshProbe{fresh}
	}
	account, err := s.accounts.GetAccount(ctx, accountID)
	if err != nil {
		return err
	}
	if !immediateOAuthRecoveryAccount(account) || !account.Schedulable || (account.Status != StatusActive && account.Status != StatusError) {
		return ErrOAuthRecoveryUnavailable
	}
	rules, err := s.GetRules(ctx)
	if err != nil {
		return err
	}
	started := s.clockNow()
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	state, detail := (&AccountTokenGuardV2Service{prober: prober}).probeAccount(probeCtx, account)
	cancel()
	next := started.Add(time.Duration(rules.ProbeIntervalSeconds) * time.Second)
	if state != AccountTokenGuardV2ProbeOK {
		next = started.Add(time.Duration(rules.RetryIntervalSeconds) * time.Second)
	}
	completion := AccountTokenGuardV2ProbeCompletion{ProbeState: state, ProbeDetail: detail, LastProbeAt: started, NextProbeAt: next}
	if err := repo.CompleteImmediateReauthVerification(ctx, accountID, account.Credentials, completion, rules); err != nil {
		return err
	}
	if state != AccountTokenGuardV2ProbeOK {
		return infraerrors.New(http.StatusBadGateway, "TOKEN_GUARD_V2_RECOVERY_VERIFY_FAILED", detail)
	}
	return nil
}
