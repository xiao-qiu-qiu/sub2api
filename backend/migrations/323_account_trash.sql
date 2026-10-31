-- Login secrets are copied verbatim from the live configuration: never decrypt
-- or re-encrypt while deleting/restoring an account. Other protocol credentials
-- remain on the soft-deleted accounts row.
CREATE TABLE IF NOT EXISTS account_trash_archives (
    account_id BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    group_bindings JSONB NOT NULL DEFAULT '[]'::jsonb,
    original_proxy_id BIGINT,
    original_proxy_fallback_origin_id BIGINT,
    login_email TEXT,
    credential_mode TEXT,
    engine TEXT,
    proxy_source TEXT,
    login_proxy_id BIGINT,
    password_ciphertext TEXT,
    totp_secret_ciphertext TEXT,
    otp_url_ciphertext TEXT,
    login_created_at TIMESTAMPTZ,
    archived_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (jsonb_typeof(group_bindings) = 'array')
);

CREATE INDEX IF NOT EXISTS idx_accounts_trash_deleted
    ON accounts (deleted_at DESC, id DESC) WHERE deleted_at IS NOT NULL;
