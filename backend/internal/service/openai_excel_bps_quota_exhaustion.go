package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service/basispoints"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type excelBPSRateLimitGenerationRepository interface {
	SetRateLimitedIfUnchanged(context.Context, int64, time.Time, *time.Time, *time.Time, time.Time) (bool, error)
}

func isExcelBPSQuotaExhaustion(raw []byte) bool {
	for _, path := range []string{"response.error.code", "response.error.type", "error.code", "error.type", "code", "type"} {
		switch gjson.GetBytes(raw, path).String() {
		case "usage_limit_reached", "insufficient_quota":
			return true
		}
	}
	return false
}

func sameExcelBPSQuotaIdentity(observed, current *Account) bool {
	if observed.ID != current.ID || observed.Platform != current.Platform || observed.Type != current.Type ||
		observed.AuthConnectionID != current.AuthConnectionID || observed.AuthConnectionRevision != current.AuthConnectionRevision ||
		observed.HasManagedOpenAIConnections() != current.HasManagedOpenAIConnections() {
		return false
	}
	for _, key := range []string{"client_id", "chatgpt_account_id", "chatgpt_user_id", "access_token", "refresh_token", "task_id"} {
		if observed.GetCredential(key) != current.GetCredential(key) {
			return false
		}
	}
	return true
}

// Only a separately trusted OAuth snapshot can turn a quota rejection into an
// account block. BPS headers and body reset timestamps are not quota evidence.
// State updates outlive client cancellation but remain bounded.
func (s *OpenAIGatewayService) handleExcelBPSRateLimit(ctx context.Context, account *Account, retryAfter string, raw []byte) {
	if s == nil || account == nil || isQualityObservation(ctx) {
		return
	}
	if isExcelBPSQuotaExhaustion(raw) && account.IsOpenAIOAuth() && account.AuthConnectionID == "" && !account.HasManagedOpenAIConnections() && s.accountRepo != nil {
		stateCtx, cancel := openAIAccountStateContext(ctx)
		defer cancel()
		current, err := s.accountRepo.GetByID(stateCtx, account.ID)
		if err == nil && current != nil && sameExcelBPSQuotaIdentity(account, current) {
			if window, resetAt, exhausted := openAIExhaustedQuotaWindow(current, time.Now()); exhausted {
				if current.RateLimitResetAt != nil && current.RateLimitResetAt.After(resetAt) {
					resetAt = *current.RateLimitResetAt
				}
				if repo, ok := s.accountRepo.(excelBPSRateLimitGenerationRepository); ok {
					written, writeErr := repo.SetRateLimitedIfUnchanged(stateCtx, account.ID, current.UpdatedAt, current.RateLimitedAt, current.RateLimitResetAt, resetAt)
					if writeErr != nil {
						logger.LegacyPrintf("service.openai_excel_bps", "quota cooldown persistence failed: account_id=%d error_type=%T", account.ID, writeErr)
					}
					if !written || writeErr != nil {
						// An admin edit, new credentials or a newer cooldown
						// generation wins over this inference result.
						return
					}
				}
				// Without the atomic generation guard, keep only the local
				// block; never issue an unguarded long write to the row.
				s.BlockAccountScheduling(current, resetAt, "excel_bps_quota_"+window)
				return
			}
		}
	}
	s.coolDownExcelBPS(ctx, account, retryAfter)
}

// Observe raw failure terminals before the bridge normalizes access-state
// codes and quota identifiers supplied only as error.type.
// HTTP SSE and the WS adapter share this reader; ordinary deltas are not decoded.
type excelBPSQuotaResponseBody struct {
	io.ReadCloser
	mu                 sync.Mutex
	line               []byte
	discardLine        bool
	eventType          string
	eventData          []byte
	discardEvent       bool
	limited            bool
	accessBlocked      bool
	requestRejected    bool
	generationObserved bool
	usageOnce          sync.Once
	observedAt         time.Time
	ctx                context.Context
	account            *Account
	headers            http.Header
	svc                *OpenAIGatewayService
}

func (s *OpenAIGatewayService) observeExcelBPSQuotaResponse(ctx context.Context, account *Account, resp *http.Response) *excelBPSQuotaResponseBody {
	body := &excelBPSQuotaResponseBody{ReadCloser: resp.Body, ctx: ctx, account: account, headers: resp.Header, svc: s, observedAt: time.Now()}
	resp.Body = body
	return body
}

func (r *excelBPSQuotaResponseBody) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.mu.Lock()
	defer r.mu.Unlock()
	for remaining := p[:n]; len(remaining) > 0; {
		end := bytes.IndexByte(remaining, '\n')
		part := remaining
		if end >= 0 {
			part = remaining[:end]
		}
		if !r.discardLine {
			if len(r.line)+len(part) > 16<<20 {
				r.line, r.discardLine = r.line[:0], true
			} else {
				r.line = append(r.line, part...)
			}
		}
		if end < 0 {
			break
		}
		r.observeLine()
		r.line, r.discardLine = r.line[:0], false
		remaining = remaining[end+1:]
	}
	if err == io.EOF {
		r.observeLine()
		r.observeEvent()
		r.recordUsage()
	}
	return n, err
}

func (r *excelBPSQuotaResponseBody) observeLine() {
	if r.discardLine {
		r.generationObserved = true
		r.discardEvent = true
		return
	}
	line := bytes.TrimSuffix(r.line, []byte{'\r'})
	if len(line) == 0 {
		r.observeEvent()
	} else if bytes.HasPrefix(line, []byte("event:")) {
		r.eventType = strings.TrimSpace(string(bytes.TrimPrefix(line, []byte("event:"))))
	} else if bytes.HasPrefix(line, []byte("data:")) && !r.discardEvent {
		data := bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte{' '})
		if len(r.eventData)+len(data)+1 > 16<<20 {
			r.discardEvent, r.generationObserved = true, true
			r.eventData = nil
		} else {
			r.eventData = append(r.eventData, data...)
			r.eventData = append(r.eventData, '\n')
		}
	}
}

// Match the bridge's frame boundaries: event fields may follow data fields,
// and all data lines together form the JSON that the upstream actually sent.
func (r *excelBPSQuotaResponseBody) observeEvent() {
	raw := bytes.TrimSuffix(r.eventData, []byte{'\n'})
	kind, discarded := r.eventType, r.discardEvent
	r.eventData, r.eventType, r.discardEvent = nil, "", false
	if discarded || len(raw) == 0 {
		return
	}
	if !gjson.ValidBytes(raw) {
		if !(r.limited && bytes.Equal(raw, []byte("[DONE]"))) {
			r.generationObserved = true
		}
		return
	}
	if r.limited && bytes.Equal(raw, []byte("[DONE]")) {
		return
	}
	if gjson.GetBytes(raw, "type").String() == "" && kind != "" {
		var err error
		raw, err = sjson.SetBytes(raw, "type", kind)
		if err != nil {
			r.generationObserved = true
			return
		}
	}
	if !excelBPSBeforeGeneration(raw) {
		r.generationObserved = true
	}
	switch gjson.GetBytes(raw, "type").String() {
	case "error", "response.failed", "response.cancelled":
		if !r.requestRejected {
			r.requestRejected = r.svc.handleExcelBPSRequestRejected(r.ctx, r.account, raw)
		}
		if !r.accessBlocked && r.svc.handleExcelBPSAccessState(r.ctx, r.account, http.StatusBadGateway, r.headers, raw) != nil {
			// Generation was accepted: isolate future requests, never replay it.
			r.accessBlocked = true
		}
		if failure := basispoints.ParseUpstreamFailure(raw); failure != nil && failure.Status == http.StatusTooManyRequests {
			if !r.limited {
				r.svc.handleExcelBPSRateLimit(r.ctx, r.account, r.headers.Get("Retry-After"), raw)
			}
			r.limited = true
			return
		}
	case "response.completed", "response.incomplete":
	default:
		return
	}
	r.recordUsage()
}

func (r *excelBPSQuotaResponseBody) recordUsage() {
	if r.limited || isQualityObservation(r.ctx) || r.account.AuthConnectionID != "" || r.account.HasManagedOpenAIConnections() || r.svc.accountRepo == nil {
		return
	}
	r.usageOnce.Do(func() {
		snapshot := ParseCodexRateLimitHeaders(r.headers)
		if snapshot == nil {
			return
		}
		snapshot.UpdatedAt = r.observedAt.UTC().Format(time.RFC3339Nano)
		// Keep the HTTP observation time across a long stream. Stamping at
		// completion would let old headers overwrite a newer WHAM snapshot.
		updates := buildCodexUsageExtraUpdates(snapshot, r.observedAt)
		if len(updates) == 0 {
			return
		}
		r.svc.getCodexSnapshotThrottle().submitCodex(r.svc.accountRepo, r.account.ID, updates, func(workerCtx context.Context, updates map[string]any) error {
			stateCtx, cancel := openAIAccountStateContext(workerCtx)
			defer cancel()
			written, err := persistCodexUsageUpdates(stateCtx, r.svc.accountRepo, r.account.ID, updates)
			if written && err == nil {
				notifyOpenAIAutoReset(r.account.ID)
			}
			return err
		})
	})
}
