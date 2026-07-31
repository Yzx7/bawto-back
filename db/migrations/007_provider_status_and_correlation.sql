-- Fase 3: estados asíncronos del proveedor y correlación de respuestas.

ALTER TABLE flow_runs DROP CONSTRAINT IF EXISTS flow_runs_status_check;
ALTER TABLE flow_runs ADD CONSTRAINT flow_runs_status_check
    CHECK (status IN ('queued','running','retry_wait','sent','delivered','read','played',
                      'failed','dead','unverified','cancelled'));
ALTER TABLE flow_runs
    ADD COLUMN IF NOT EXISTS provider_status_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS delivered_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS read_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS played_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS conversation_id TEXT,
    ADD COLUMN IF NOT EXISTS conversation_type TEXT,
    ADD COLUMN IF NOT EXISTS pricing_model TEXT,
    ADD COLUMN IF NOT EXISTS pricing_type TEXT,
    ADD COLUMN IF NOT EXISTS pricing_category TEXT,
    ADD COLUMN IF NOT EXISTS billable BOOLEAN;

CREATE UNIQUE INDEX IF NOT EXISTS uq_flow_runs_provider_message
    ON flow_runs(provider_message_id) WHERE provider_message_id IS NOT NULL;

ALTER TABLE messages
    ADD COLUMN IF NOT EXISTS provider_status TEXT,
    ADD COLUMN IF NOT EXISTS provider_status_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS provider_error_code TEXT,
    ADD COLUMN IF NOT EXISTS provider_error TEXT,
    ADD COLUMN IF NOT EXISTS conversation_id TEXT,
    ADD COLUMN IF NOT EXISTS conversation_type TEXT,
    ADD COLUMN IF NOT EXISTS pricing_model TEXT,
    ADD COLUMN IF NOT EXISTS pricing_type TEXT,
    ADD COLUMN IF NOT EXISTS pricing_category TEXT,
    ADD COLUMN IF NOT EXISTS billable BOOLEAN;
ALTER TABLE messages DROP CONSTRAINT IF EXISTS messages_provider_status_check;
ALTER TABLE messages ADD CONSTRAINT messages_provider_status_check
    CHECK (provider_status IS NULL OR provider_status IN ('sent','delivered','read','played','failed'));

-- Inbox durable: un status que se adelante al INSERT del mensaje queda pendiente
-- y el mismo webhook/scheduler lo reconcilia cuando aparezca provider_message_id.
CREATE TABLE IF NOT EXISTS provider_status_events (
    id                BIGSERIAL PRIMARY KEY,
    event_key         TEXT NOT NULL UNIQUE,
    channel           TEXT NOT NULL,
    channel_id        TEXT,
    provider_message_id TEXT NOT NULL,
    status            TEXT NOT NULL,
    occurred_at       TIMESTAMPTZ NOT NULL,
    recipient_id      TEXT,
    error_code        TEXT,
    error_title       TEXT,
    error_message     TEXT,
    error_details     TEXT,
    conversation_id   TEXT,
    conversation_type TEXT,
    pricing_model     TEXT,
    pricing_type      TEXT,
    pricing_category  TEXT,
    billable          BOOLEAN,
    opaque_callback_data TEXT,
    metadata          JSONB NOT NULL DEFAULT '{}',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    applied_at        TIMESTAMPTZ,
    CHECK (status IN ('sent','delivered','read','played','failed'))
);
CREATE INDEX IF NOT EXISTS idx_provider_status_pending
    ON provider_status_events(provider_message_id, occurred_at, id)
    WHERE applied_at IS NULL;

CREATE TABLE IF NOT EXISTS message_correlations (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    inbound_message_id  BIGINT NOT NULL UNIQUE REFERENCES messages(id) ON DELETE CASCADE,
    outbound_message_id BIGINT REFERENCES messages(id) ON DELETE SET NULL,
    flow_run_id         UUID REFERENCES flow_runs(id) ON DELETE SET NULL,
    data_record_id      UUID REFERENCES data_records(id) ON DELETE SET NULL,
    method              TEXT NOT NULL,
    quoted_provider_message_id TEXT,
    candidate_count     INTEGER NOT NULL DEFAULT 0,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (method IN ('exact','inferred','ambiguous','none')),
    CHECK (candidate_count >= 0)
);
CREATE INDEX IF NOT EXISTS idx_message_correlations_record
    ON message_correlations(data_record_id, created_at DESC)
    WHERE data_record_id IS NOT NULL;
