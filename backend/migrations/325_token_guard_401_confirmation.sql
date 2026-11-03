-- Persist the per-account 401 confirmation across restarts and worker leases.
-- Bind it to the rejected credential snapshot so a refresh starts a new pair.
ALTER TABLE account_token_guard_v2_accounts
    ADD COLUMN IF NOT EXISTS unauthorized_retry_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS unauthorized_credentials_hash TEXT NOT NULL DEFAULT '';
