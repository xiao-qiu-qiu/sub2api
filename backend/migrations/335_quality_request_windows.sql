-- Keep probe boundaries independently of pruned test history and usage arrival.
CREATE TABLE IF NOT EXISTS account_quality_probe_checkpoints (
    plan_id BIGINT PRIMARY KEY REFERENCES scheduled_test_plans(id) ON DELETE CASCADE,
    account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    plan_version TIMESTAMPTZ NOT NULL,
    healthy_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS account_quality_request_windows (
    id BIGSERIAL PRIMARY KEY,
    account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    plan_id BIGINT NOT NULL,
    window_start TIMESTAMPTZ NOT NULL,
    window_end TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (plan_id, window_end),
    CHECK (window_start < window_end)
);
CREATE INDEX IF NOT EXISTS account_quality_request_windows_account_end_idx
    ON account_quality_request_windows(account_id, window_end DESC);
