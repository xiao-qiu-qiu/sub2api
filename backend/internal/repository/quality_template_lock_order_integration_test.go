//go:build integration

package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// Dedicated one-connection pools give every worker a stable PostgreSQL PID.
// All barriers are observed through pg_blocking_pids, never timing-only sleeps.
func qualityLockOrderDB(t *testing.T, ctx context.Context, label string) (*sql.DB, int) {
	t.Helper()
	parsed, err := url.Parse(integrationPostgresDSN)
	require.NoError(t, err)
	query := parsed.Query()
	query.Set("application_name", label)
	parsed.RawQuery = query.Encode()
	db, err := sql.Open("postgres", parsed.String())
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `SET statement_timeout='10s'; SET lock_timeout='8s'; SET deadlock_timeout='100ms'`)
	require.NoError(t, err)
	var pid int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pid))
	return db, pid
}

func qualityLockOrderGate(t *testing.T, ctx context.Context, namespace int, keys ...int64) (*sql.Tx, int) {
	t.Helper()
	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	var pid int
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pid))
	for _, key := range keys {
		_, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1::integer,$2::integer)", namespace, key)
		require.NoError(t, err)
	}
	return tx, pid
}

func qualityLockOrderWait(t *testing.T, ctx context.Context, worker int, blockers ...int) int {
	t.Helper()
	var observed int
	require.Eventually(t, func() bool {
		err := integrationDB.QueryRowContext(ctx, `SELECT COALESCE((SELECT blocker FROM unnest(pg_blocking_pids($1)) blocker WHERE blocker=ANY($2::integer[]) LIMIT 1),0)`, worker, pq.Array(blockers)).Scan(&observed)
		return err == nil && observed != 0
	}, 5*time.Second, 10*time.Millisecond, "worker %d did not reach the expected database lock barrier %v", worker, blockers)
	return observed
}

func qualityLockOrderTrigger(t *testing.T, ctx context.Context, event, body string) {
	t.Helper()
	// zz sorts after both PostgreSQL FK checks and the 337 admission AFTER trigger.
	name := fmt.Sprintf("zz_quality_lock_order_%d", time.Now().UnixNano())
	quoted := pq.QuoteIdentifier(name)
	_, err := integrationDB.ExecContext(ctx, "CREATE FUNCTION "+quoted+"() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN "+body+" RETURN NEW; END $$")
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, err := integrationDB.ExecContext(cleanCtx, "DROP TRIGGER IF EXISTS "+quoted+" ON scheduled_test_plans")
		if err != nil {
			t.Errorf("remove lock barrier trigger: %v", err)
			return
		}
		_, err = integrationDB.ExecContext(cleanCtx, "DROP FUNCTION IF EXISTS "+quoted+"()")
		if err != nil {
			t.Errorf("remove lock barrier function: %v", err)
		}
	})
	_, err = integrationDB.ExecContext(ctx, "CREATE TRIGGER "+quoted+" "+event+" ON scheduled_test_plans FOR EACH ROW EXECUTE FUNCTION "+quoted+"()")
	require.NoError(t, err)
}

func qualityLockOrderExpect(t *testing.T, legacy bool, errs ...error) {
	t.Helper()
	deadlocks := 0
	for _, err := range errs {
		if err == nil {
			continue
		}
		var pgErr *pq.Error
		if errors.As(err, &pgErr) && pgErr.Code == "40P01" {
			deadlocks++
			continue
		}
		require.NoError(t, err, "unexpected error; timeouts do not count as a deadlock reproduction")
	}
	if legacy {
		require.Equal(t, 1, deadlocks, "RED control must reproduce an actual PostgreSQL deadlock")
		t.Log("RED control: observed PostgreSQL SQLSTATE 40P01")
	} else {
		require.Zero(t, deadlocks, "GREEN production implementation must complete both workers")
		t.Log("GREEN: both production transactions completed without deadlock")
	}
}

// The negative control reproduces only the pre-fix database lock protocol. Model
// discovery is deliberately outside this transaction, just as in ReincludeAccount.
func qualityLockOrderLegacyReinclude(ctx context.Context, db *sql.DB, f *qualityReincludeFixture) (service.QualityTemplateReincludeResult, error) {
	out := service.QualityTemplateReincludeResult{AccountID: f.account}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	var id int64
	if err = tx.QueryRowContext(ctx, "SELECT id FROM quality_rule_templates WHERE id=$1 FOR SHARE", f.tpl.ID).Scan(&id); err != nil {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT id FROM scheduled_test_plans WHERE account_id=$1 ORDER BY id FOR UPDATE", f.account)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		if err = rows.Scan(&id); err != nil {
			_ = rows.Close()
			return out, err
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return out, err
	}
	if err = tx.QueryRowContext(ctx, "SELECT id FROM accounts WHERE id=$1 FOR NO KEY UPDATE", f.account).Scan(&id); err != nil {
		return out, err
	}
	var prior sql.NullInt64
	if err = tx.QueryRowContext(ctx, "SELECT plan_id FROM quality_rule_template_accounts WHERE template_id=$1 AND account_id=$2 FOR UPDATE", f.tpl.ID, f.account).Scan(&prior); err != nil {
		return out, err
	}
	plan, _ := f.prepare(ctx, service.QualityTemplateAccount{ID: f.account})
	plan.SourceTemplateID = f.tpl.ID
	created, err := insertScheduledTestPlan(ctx, tx, plan)
	if err != nil {
		return out, err
	}
	_, err = tx.ExecContext(ctx, "UPDATE quality_rule_template_accounts SET plan_id=$3 WHERE template_id=$1 AND account_id=$2", f.tpl.ID, f.account, created.ID)
	if err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	out.Status = "created"
	return out, nil
}

func TestQualityTemplateLockOrderReincludeAndOrdinaryCreate(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		name := "GREEN_current"
		if legacy {
			name = "RED_legacy_lock_protocol"
		}
		t.Run(name, func(t *testing.T) {
			f := newQualityReincludeFixture(t)
			ctx, cancel := context.WithTimeout(f.ctx, 20*time.Second)
			defer cancel()
			reincludeDB, reincludePID := qualityLockOrderDB(t, ctx, "quality-lock-reinclude")
			ordinaryDB, ordinaryPID := qualityLockOrderDB(t, ctx, "quality-lock-ordinary")
			const namespace = 339001
			qualityLockOrderTrigger(t, ctx, "BEFORE INSERT", fmt.Sprintf("IF NEW.account_id=%d AND NEW.source_template_id=%d THEN PERFORM pg_advisory_xact_lock(%d,%d); END IF;", f.account, f.tpl.ID, namespace, f.tpl.ID))
			gate, gatePID := qualityLockOrderGate(t, ctx, namespace, f.tpl.ID)
			defer func() { _ = gate.Rollback() }()
			type reincludeOutcome struct {
				result service.QualityTemplateReincludeResult
				err    error
			}
			reincludeDone := make(chan reincludeOutcome, 1)
			go func() {
				var result service.QualityTemplateReincludeResult
				var err error
				if legacy {
					result, err = qualityLockOrderLegacyReinclude(ctx, reincludeDB, f)
				} else {
					repo := &qualityRuleTemplateRepository{db: reincludeDB}
					result, err = repo.ReincludeAccount(ctx, f.tpl, f.account, f.prepare)
				}
				reincludeDone <- reincludeOutcome{result, err}
			}()
			// Reinclude now owns its account lock, but has not inserted the replacement.
			qualityLockOrderWait(t, ctx, reincludePID, gatePID)
			ordinaryDone := make(chan error, 1)
			go func() {
				plan, _ := f.prepare(ctx, service.QualityTemplateAccount{ID: f.account})
				_, err := NewScheduledTestPlanRepository(ordinaryDB).Create(ctx, plan)
				ordinaryDone <- err
			}()
			// Old NO KEY UPDATE lets the FK KEY SHARE in; new FOR UPDATE blocks that FK.
			// Both cases visibly wait on Reinclude, so releasing one gate tests the upgrade.
			qualityLockOrderWait(t, ctx, ordinaryPID, reincludePID)
			require.NoError(t, gate.Commit())
			var reinclude reincludeOutcome
			select {
			case reinclude = <-reincludeDone:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var ordinaryErr error
			select {
			case ordinaryErr = <-ordinaryDone:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			qualityLockOrderExpect(t, legacy, reinclude.err, ordinaryErr)
			if !legacy {
				require.Equal(t, "created", reinclude.result.Status)
				require.True(t, f.binding().Valid)
				require.Equal(t, 2, f.countPlans(), "manual and template identities both persist")
			}
		})
	}
}

// Replace the function only in this test process's disposable database. The
// released 338 SQL is read unchanged; the installed 339 definition is restored
// before the GREEN subtest and before fixture cleanup. No repository file changes.
func qualityLockOrderPauseDefinition(t *testing.T, legacy bool) func() {
	t.Helper()
	var installed string
	require.NoError(t, integrationDB.QueryRow("SELECT pg_get_functiondef('quality_template_pause_plans()'::regprocedure)").Scan(&installed))
	entries, err := migrations.FS.ReadDir(".")
	require.NoError(t, err)
	has339 := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "339_") {
			has339 = true
		}
	}
	require.True(t, has339, "run with all migrations, including the pause lock-order migration 339")
	var admissionTrigger bool
	require.NoError(t, integrationDB.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_trigger WHERE tgrelid='scheduled_test_plans'::regclass AND tgname='quality_probe_admission_plan_changed_trigger' AND NOT tgisinternal)`).Scan(&admissionTrigger))
	require.True(t, admissionTrigger, "337 admission trigger must participate in both RED and GREEN")
	if legacy {
		raw, err := migrations.FS.ReadFile("338_quality_template_identity.sql")
		require.NoError(t, err)
		script := string(raw)
		start := strings.Index(script, "CREATE OR REPLACE FUNCTION quality_template_pause_plans()")
		require.NotEqual(t, -1, start)
		end := strings.Index(script[start:], "END $$;")
		require.NotEqual(t, -1, end)
		_, err = integrationDB.Exec(script[start : start+end+len("END $$;")])
		require.NoError(t, err)
	}
	return func() {
		if !legacy {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, err := integrationDB.ExecContext(ctx, installed)
		if err != nil {
			t.Errorf("restore installed 339 pause function: %v", err)
		}
	}
}

func TestQualityTemplateLockOrderTwoMultiAccountPauses(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		name := "GREEN_current_339"
		if legacy {
			name = "RED_released_338"
		}
		t.Run(name, func(t *testing.T) {
			restore := qualityLockOrderPauseDefinition(t, legacy)
			defer restore()
			f := newQualityReincludeFixture(t)
			ctx, cancel := context.WithTimeout(f.ctx, 20*time.Second)
			defer cancel()
			first, second := f.account, f.addAccount()
			makeTemplate := func() *service.QualityRuleTemplate {
				copy := *f.tpl
				copy.ID = 0
				tpl, err := f.repo.Create(ctx, &copy)
				require.NoError(t, err)
				return tpl
			}
			x, y := makeTemplate(), makeTemplate()
			add := func(tpl *service.QualityRuleTemplate, account int64) int64 {
				plan, _ := f.prepare(ctx, service.QualityTemplateAccount{ID: account})
				created, err := f.repo.CreateLinkedPlan(ctx, tpl, plan)
				require.NoError(t, err)
				return created.ID
			}
			// Opposite insertion/plan-ID order makes the old bulk updates visit A/B and B/A.
			firstX := add(x, first)
			add(x, second)
			firstY := add(y, second)
			add(y, first)
			xDB, xPID := qualityLockOrderDB(t, ctx, "quality-lock-pause-x")
			yDB, yPID := qualityLockOrderDB(t, ctx, "quality-lock-pause-y")
			for _, db := range []*sql.DB{xDB, yDB} {
				// Force a single-batch sequential/hash plan, so the fixture's opposite heap
				// order is stable. 339's explicit ORDER BY locks remain authoritative.
				_, err := db.ExecContext(ctx, `SET enable_indexscan=off; SET enable_indexonlyscan=off; SET enable_bitmapscan=off; SET enable_nestloop=off; SET enable_mergejoin=off; SET work_mem='16MB'`)
				require.NoError(t, err)
			}
			const namespace = 339002
			qualityLockOrderTrigger(t, ctx, "AFTER UPDATE OF enabled", fmt.Sprintf("IF OLD.enabled AND NOT NEW.enabled THEN IF NEW.id=%d THEN PERFORM pg_advisory_xact_lock(%d,%d); ELSIF NEW.id=%d THEN PERFORM pg_advisory_xact_lock(%d,%d); END IF; END IF;", firstX, namespace, x.ID, firstY, namespace, y.ID))
			gate, gatePID := qualityLockOrderGate(t, ctx, namespace, x.ID, y.ID)
			defer func() { _ = gate.Rollback() }()
			xDone, yDone := make(chan error, 1), make(chan error, 1)
			go func() {
				_, err := xDB.ExecContext(ctx, "UPDATE quality_rule_templates SET enabled=false,updated_at=clock_timestamp() WHERE id=$1", x.ID)
				xDone <- err
			}()
			qualityLockOrderWait(t, ctx, xPID, gatePID)
			go func() {
				_, err := yDB.ExecContext(ctx, "UPDATE quality_rule_templates SET enabled=false,updated_at=clock_timestamp() WHERE id=$1", y.ID)
				yDone <- err
			}()
			if legacy {
				// Both old transactions own a different account before their first barrier.
				qualityLockOrderWait(t, ctx, yPID, gatePID)
			} else {
				// 339 waits on X's account pre-lock before its first child update: never
				// require Y to reach its advisory gate while X still owns both accounts.
				qualityLockOrderWait(t, ctx, yPID, xPID)
			}
			require.NoError(t, gate.Commit())
			var xErr, yErr error
			select {
			case xErr = <-xDone:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			select {
			case yErr = <-yDone:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			qualityLockOrderExpect(t, legacy, xErr, yErr)
			if !legacy {
				var enabled int
				require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT count(*) FROM scheduled_test_plans WHERE source_template_id=ANY($1) AND enabled", pq.Array([]int64{x.ID, y.ID})).Scan(&enabled))
				require.Zero(t, enabled)
				var admitted int
				require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT count(*) FROM account_quality_probe_admissions WHERE account_id=ANY($1) AND enabled", pq.Array([]int64{first, second})).Scan(&admitted))
				require.Zero(t, admitted, "337 admission must observe both templates fully paused")
			}
		})
	}
}
