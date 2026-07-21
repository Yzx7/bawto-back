CREATE TABLE IF NOT EXISTS message_media (
    message_id   BIGINT      PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
    provider_id  TEXT        NOT NULL,
    mime_type    TEXT        NOT NULL,
    data         BYTEA       NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (octet_length(data) <= 10485760)
);
