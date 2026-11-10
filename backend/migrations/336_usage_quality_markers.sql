ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS request_started_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS quality_interrupted BOOLEAN NOT NULL DEFAULT FALSE;

COMMENT ON COLUMN usage_logs.request_started_at IS
    'Observed request start; NULL means unknown and must not be inferred from log creation time or duration';
COMMENT ON COLUMN usage_logs.quality_interrupted IS
    'True when the request was interrupted by an account quality action';

CREATE INDEX IF NOT EXISTS idx_usage_logs_account_request_started_at
    ON usage_logs (account_id, request_started_at)
    WHERE request_started_at IS NOT NULL;
