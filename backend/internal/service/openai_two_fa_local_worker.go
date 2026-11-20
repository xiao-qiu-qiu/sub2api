package service

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"slices"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

const (
	// Reserve the upper half of the Worker protocol's positive, JS-safe integer IDs for in-memory tasks.
	twoFALoginWorkerTaskIDMin int64 = 1 << 52
	twoFALoginWorkerTaskIDMax int64 = 1 << 53
	twoFALoginWorkerQueueMax        = 32
)

type twoFALoginWorkerResult struct {
	credentials map[string]any
	err         error
}

type twoFALoginWorkerTask struct {
	ctx           context.Context
	queuedAt      time.Time
	workerID      string
	claim         OpenAIOAuthReauthClaim
	expectedEmail string
	result        chan twoFALoginWorkerResult
}

type twoFALoginWorkerQueue struct {
	mu    sync.Mutex
	tasks map[int64]*twoFALoginWorkerTask
}

func isTwoFALoginWorkerTaskID(taskID int64) bool {
	return taskID > twoFALoginWorkerTaskIDMin && taskID < twoFALoginWorkerTaskIDMax
}

func twoFALoginWorkerTaskNotActive() error {
	return infraerrors.Conflict("OPENAI_TWO_FA_TASK_NOT_ACTIVE", "2FA login task is no longer active")
}

func (svc *OpenAIOAuthReauthService) CheckTwoFALoginWorker(ctx context.Context, proxyID *int64) error {
	_, err := svc.twoFALoginWorkerProxy(ctx, proxyID)
	return err
}

func (svc *OpenAIOAuthReauthService) twoFALoginWorkerProxy(ctx context.Context, proxyID *int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if svc == nil || svc.oauth == nil {
		return "", infraerrors.ServiceUnavailable("OPENAI_REAUTH_UNAVAILABLE", "OpenAI 2FA login is not available")
	}
	if proxyID != nil && svc.accounts == nil {
		return "", infraerrors.BadRequest("OPENAI_TWO_FA_PROXY_UNAVAILABLE", "Selected proxy is unavailable")
	}
	proxyURL, err := svc.reauthProxyURL(ctx, proxyID)
	if err != nil {
		return "", infraerrors.BadRequest("OPENAI_TWO_FA_PROXY_UNAVAILABLE", "Selected proxy is unavailable")
	}
	svc.EnsureWorker()
	if svc.WorkerStatus().State == "unavailable" {
		return "", infraerrors.ServiceUnavailable("OPENAI_REAUTH_WORKER_UNAVAILABLE", "Re-login service is unavailable; see Credential Operations status")
	}
	return proxyURL, nil
}

func (svc *OpenAIOAuthReauthService) LoginTwoFA(ctx context.Context, entry AccountTokenGuardReloginAccount, proxyID *int64) (map[string]any, error) {
	if err := ValidateOpenAITwoFALogin(entry); err != nil {
		return nil, err
	}
	proxyURL, err := svc.twoFALoginWorkerProxy(ctx, proxyID)
	if err != nil {
		return nil, err
	}
	taskID, task, err := svc.twoFALoginQueue.enqueue(ctx, entry, proxyURL)
	if err != nil {
		return nil, err
	}
	defer svc.twoFALoginQueue.remove(taskID, task)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-task.result:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return result.credentials, result.err
	}
}

func (queue *twoFALoginWorkerQueue) enqueue(ctx context.Context, entry AccountTokenGuardReloginAccount, proxyURL string) (int64, *twoFALoginWorkerTask, error) {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	if len(queue.tasks) >= twoFALoginWorkerQueueMax {
		return 0, nil, infraerrors.TooManyRequests("OPENAI_TWO_FA_QUEUE_FULL", "Too many 2FA login tasks; try again later")
	}
	var taskID int64
	for {
		offset, err := rand.Int(rand.Reader, big.NewInt(twoFALoginWorkerTaskIDMax-twoFALoginWorkerTaskIDMin-1))
		if err != nil {
			return 0, nil, infraerrors.InternalServer("OPENAI_TWO_FA_TASK_ID_FAILED", "Could not initialize the 2FA login task")
		}
		taskID = twoFALoginWorkerTaskIDMin + 1 + offset.Int64()
		if queue.tasks[taskID] == nil {
			break
		}
	}
	email := strings.ToLower(strings.TrimSpace(entry.Email))
	// The existing Worker requires a positive account_id for logging and its temporary export label.
	// First login has no account, so use the task ID on the wire without looking it up or storing it as an account ID.
	wireAccountID := taskID
	task := &twoFALoginWorkerTask{
		ctx: ctx, queuedAt: time.Now(), expectedEmail: email, result: make(chan twoFALoginWorkerResult, 1),
		claim: OpenAIOAuthReauthClaim{
			OAuthProfile: "codex", CredentialMode: OpenAIOAuthReauthModePasswordTOTP, Engine: OpenAIOAuthReauthEngineLocal,
			TaskID: taskID, AccountID: wireAccountID, LoginEmail: email, Password: entry.Password,
			TOTPSecret: strings.TrimSpace(entry.MFASecret), ProxyURL: proxyURL,
		},
	}
	if queue.tasks == nil {
		queue.tasks = make(map[int64]*twoFALoginWorkerTask)
	}
	queue.tasks[taskID] = task
	return taskID, task, nil
}

func (queue *twoFALoginWorkerQueue) claim(workerID string) *OpenAIOAuthReauthClaim {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	var next *twoFALoginWorkerTask
	for taskID, task := range queue.tasks {
		if task.ctx.Err() != nil {
			queue.removeLocked(taskID, task)
			continue
		}
		if task.workerID == "" && (next == nil || task.queuedAt.Before(next.queuedAt)) {
			next = task
		}
	}
	if next == nil {
		return nil
	}
	next.workerID = workerID
	claim := next.claim
	next.claim = OpenAIOAuthReauthClaim{}
	return &claim
}

func (queue *twoFALoginWorkerQueue) activeEmail(taskID int64, workerID string) (string, error) {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	task := queue.activeTaskLocked(taskID, workerID)
	if task == nil {
		return "", twoFALoginWorkerTaskNotActive()
	}
	return task.expectedEmail, nil
}

func (queue *twoFALoginWorkerQueue) activeTaskLocked(taskID int64, workerID string) *twoFALoginWorkerTask {
	task := queue.tasks[taskID]
	if task == nil || task.workerID == "" || task.workerID != strings.TrimSpace(workerID) || task.ctx.Err() != nil {
		return nil
	}
	return task
}

func (queue *twoFALoginWorkerQueue) finish(taskID int64, workerID string, result twoFALoginWorkerResult) error {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	task := queue.activeTaskLocked(taskID, workerID)
	if task == nil {
		return twoFALoginWorkerTaskNotActive()
	}
	queue.removeLocked(taskID, task)
	task.result <- result
	return nil
}

func (queue *twoFALoginWorkerQueue) remove(taskID int64, task *twoFALoginWorkerTask) {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.tasks[taskID] == task {
		queue.removeLocked(taskID, task)
	}
}

func (queue *twoFALoginWorkerQueue) removeLocked(taskID int64, task *twoFALoginWorkerTask) {
	task.claim = OpenAIOAuthReauthClaim{}
	task.expectedEmail = ""
	delete(queue.tasks, taskID)
}

func (svc *OpenAIOAuthReauthService) submitTwoFALoginCredentials(taskID int64, workerID string, credentials, workerExtra map[string]any) (*OpenAIOAuthReauthTask, error) {
	email, err := svc.twoFALoginQueue.activeEmail(taskID, workerID)
	if err != nil {
		return nil, err
	}
	result, validationErr := svc.twoFALoginCredentials(email, credentials, workerExtra)
	if err := svc.twoFALoginQueue.finish(taskID, workerID, twoFALoginWorkerResult{credentials: result, err: validationErr}); err != nil {
		return nil, err
	}
	if validationErr != nil {
		return nil, validationErr
	}
	return &OpenAIOAuthReauthTask{ID: taskID, Status: OpenAIOAuthReauthStatusSucceeded, Stage: OpenAIOAuthReauthStageSucceeded, OAuthProfile: "codex"}, nil
}

func (svc *OpenAIOAuthReauthService) twoFALoginCredentials(email string, credentials, workerExtra map[string]any) (map[string]any, error) {
	invalid := infraerrors.BadRequest("OPENAI_TWO_FA_INVALID_CREDENTIALS", "Worker returned invalid Codex OAuth credentials")
	for _, key := range []string{"access_token", "refresh_token", "id_token"} {
		value, ok := credentials[key].(string)
		if !ok || len(value) > 65536 || strings.TrimSpace(value) == "" {
			return nil, invalid
		}
	}
	for _, fields := range []map[string]any{credentials, workerExtra} {
		if raw, present := fields["client_id"]; present {
			clientID, ok := raw.(string)
			if !ok || (strings.TrimSpace(clientID) != "" && strings.TrimSpace(clientID) != openai.ClientID) {
				return nil, invalid
			}
		}
	}
	info, _, err := directReauthTokenInfo(credentials, nil)
	if err != nil || info.ExpiresAt <= time.Now().Unix() || info.ClientID != openai.ClientID {
		return nil, invalid
	}
	claims, err := openai.DecodeIDToken(info.IDToken)
	if err != nil || (len(claims.Aud) > 0 && !slices.Contains(claims.Aud, openai.ClientID)) {
		return nil, invalid
	}
	if err := resolveReauthTokenIdentity(info); err != nil {
		return nil, err
	}
	if !strings.EqualFold(info.Email, email) {
		return nil, infraerrors.Conflict("OPENAI_REAUTH_EMAIL_MISMATCH", "New OAuth login email does not match the supplied account")
	}
	return twoFALoginCredential(svc.oauth.BuildAccountCredentials(info), email), nil
}

func (svc *OpenAIOAuthReauthService) failTwoFALoginTask(taskID int64, workerID, reason string) error {
	reason = sanitizeReauthError(reason)
	if reason == "" {
		reason = "worker reported a re-login failure"
	}
	return svc.twoFALoginQueue.finish(taskID, workerID, twoFALoginWorkerResult{err: errors.New(reason)})
}
