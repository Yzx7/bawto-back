-- migrate:baseline
-- Histórica: aplicada a mano y ya presente en schema.sql. Se registra sin
-- ejecutarse para no reabrir DDL sobre bases que ya la tienen.
CREATE TABLE IF NOT EXISTS message_media (
    message_id   BIGINT      PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
    provider_id  TEXT        NOT NULL,
    mime_type    TEXT        NOT NULL,
    data         BYTEA       NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (octet_length(data) <= 10485760)
);
