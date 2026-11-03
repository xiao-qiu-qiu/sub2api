ALTER TABLE account_trash_archives
    ADD COLUMN IF NOT EXISTS delete_note TEXT NOT NULL DEFAULT ''
    CHECK (char_length(delete_note) <= 1000);
