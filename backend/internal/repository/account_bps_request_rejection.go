package repository

import (
	"context"
	"encoding/json"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	dbpredicate "github.com/Wei-Shaw/sub2api/ent/predicate"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.AccountBPSRequestRejectionRepository = (*accountRepository)(nil)
var _ service.AccountBPSRequestRejectionResetRepository = (*accountRepository)(nil)

func bpsRequestNotRejectedPredicate() dbpredicate.Account {
	return func(s *entsql.Selector) {
		s.Where(entsql.ExprP("COALESCE(" + s.C(dbaccount.FieldExtra) + " -> 'openai_bps_request_rejected' -> 'blocked', 'false'::jsonb) <> 'true'::jsonb"))
	}
}

func (r *accountRepository) MarkBPSRequestRejected(ctx context.Context, observed *service.Account, reason string) (bool, error) {
	if observed == nil || observed.ID <= 0 || !service.BPSRequestRejectionWritesAllowed(ctx) {
		return false, nil
	}
	if scoped := r.accountTransactionScope(ctx); scoped != r {
		changed, err := scoped.MarkBPSRequestRejected(ctx, observed, reason)
		if changed && err == nil {
			service.AfterAccountCommit(ctx, func(committed context.Context) {
				_ = r.publishBPSRequestRejectionSchedulingState(committed, observed.ID)
			})
		}
		return changed, err
	}
	switch reason {
	case "usage_policy", "invalid_request":
	default:
		reason = "upstream_rejected"
	}
	credentials, err := json.Marshal(normalizeJSONMap(observed.Credentials))
	if err != nil {
		return false, err
	}
	generation, err := json.Marshal(observed.Extra[service.BPSRequestRejectedGenerationExtraKey])
	if err != nil {
		return false, err
	}
	// The atomic statement publishes an outbox event only if the originating
	// credentials and reset epoch still match. No unrelated status is changed.
	result, err := r.sql.ExecContext(ctx, `
		WITH updated AS (
			UPDATE accounts AS a
			SET extra = jsonb_set(COALESCE(a.extra, '{}'::jsonb), '{openai_bps_request_rejected}',
				jsonb_build_object('blocked', true, 'blocked_at', $6::text, 'reason', $7::text)),
				updated_at = clock_timestamp()
			WHERE a.id = $1 AND a.deleted_at IS NULL
				AND a.created_at = $10
				AND a.platform = $2
				AND (($8 = '' AND a.type = $3 AND a.credentials = $4::jsonb
					AND COALESCE(a.extra -> 'openai_connections_managed', 'false'::jsonb) <> 'true'::jsonb)
					OR ($8 <> '' AND EXISTS (
						SELECT 1 FROM openai_auth_connection_policies p WHERE p.account_id=a.id AND p.enabled
					) AND EXISTS (
						SELECT 1 FROM openai_auth_connections c
						WHERE c.id::text = $8 AND c.account_id = a.id AND c.revision = $9
							AND c.kind = 'bps' AND c.status = 'active')))
				AND COALESCE(a.extra -> 'openai_bps_request_rejected_generation', 'null'::jsonb) = $5::jsonb
			RETURNING a.id
		)
		INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload)
		SELECT $11, updated.id, NULL, NULL FROM updated
	`, observed.ID, observed.Platform, observed.Type, string(credentials), string(generation),
		time.Now().UTC().Format(time.RFC3339Nano), reason,
		observed.AuthConnectionID, observed.AuthConnectionRevision, observed.CreatedAt, service.SchedulerOutboxEventAccountChanged)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected == 0 {
		return false, err
	}
	if dbent.TxFromContext(ctx) != nil {
		return true, nil
	}
	// SetAccount advances metadata revisions, invalidating warm candidate memos
	// as well as the sticky/full-account payload before reporting success.
	propagationCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	return true, r.publishBPSRequestRejectionSchedulingState(propagationCtx, observed.ID)
}

func (r *accountRepository) ResetBPSRequestRejected(ctx context.Context, id int64) error {
	if scoped := r.accountTransactionScope(ctx); scoped != r {
		if err := scoped.ResetBPSRequestRejected(ctx, id); err != nil {
			return err
		}
		service.AfterAccountCommit(ctx, func(committed context.Context) {
			_ = r.publishBPSRequestRejectionSchedulingState(committed, id)
		})
		return nil
	}
	result, err := r.sql.ExecContext(ctx, `
		WITH updated AS (
			UPDATE accounts
			SET extra = jsonb_set(COALESCE(extra, '{}'::jsonb) - 'openai_bps_request_rejected',
				'{openai_bps_request_rejected_generation}', to_jsonb(gen_random_uuid()::text)),
				updated_at = clock_timestamp()
			WHERE id = $1 AND deleted_at IS NULL
			RETURNING id
		)
		INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload)
		SELECT $2, updated.id, NULL, NULL FROM updated
	`, id, service.SchedulerOutboxEventAccountChanged)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return service.ErrAccountNotFound
	}
	if dbent.TxFromContext(ctx) != nil {
		return nil
	}
	propagationCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	return r.publishBPSRequestRejectionSchedulingState(propagationCtx, id)
}

func (r *accountRepository) publishBPSRequestRejectionSchedulingState(ctx context.Context, id int64) error {
	return r.WithLatestSchedulerAccounts(ctx, []int64{id}, func(accounts []service.Account) error {
		if len(accounts) == 0 {
			if r.schedulerCache != nil {
				return r.schedulerCache.DeleteAccount(ctx, id)
			}
			return nil
		}
		service.PublishBPSRequestRejectionSchedulingState(&accounts[0])
		if r.schedulerCache != nil {
			return r.schedulerCache.SetAccount(ctx, &accounts[0])
		}
		return nil
	})
}
