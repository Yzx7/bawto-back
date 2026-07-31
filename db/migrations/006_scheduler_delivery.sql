-- Fase 2: scheduler durable, auditable e idempotente.
--
-- Los flow_runs existentes pertenecen al prototipo anterior y no contienen
-- suficiente información para reanudarlos con seguridad. Esta instalación
-- solo contiene datos de prueba, por lo que se descartan explícitamente.
TRUNCATE TABLE flow_runs;

ALTER TABLE flows ADD COLUMN IF NOT EXISTS last_tick_at TIMESTAMPTZ;

ALTER TABLE flow_runs DROP CONSTRAINT IF EXISTS flow_runs_bot_id_run_key_key;
ALTER TABLE flow_runs DROP CONSTRAINT IF EXISTS flow_runs_status_check;
ALTER TABLE flow_runs DROP CONSTRAINT IF EXISTS flow_runs_data_record_id_fkey;
ALTER TABLE flow_runs DROP CONSTRAINT IF EXISTS flow_runs_contact_id_fkey;
ALTER TABLE flow_runs ALTER COLUMN flow_id TYPE UUID USING flow_id::uuid;
ALTER TABLE flow_runs ALTER COLUMN data_record_id DROP NOT NULL;
ALTER TABLE flow_runs ALTER COLUMN contact_id DROP NOT NULL;
ALTER TABLE flow_runs RENAME COLUMN error TO last_error;

ALTER TABLE flow_runs
    ADD COLUMN IF NOT EXISTS flow_version_id UUID REFERENCES flow_versions(id) ON DELETE RESTRICT,
    ADD COLUMN IF NOT EXISTS scheduled_for TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'schedule',
    ADD COLUMN IF NOT EXISTS attempt INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS max_attempts INTEGER NOT NULL DEFAULT 5,
    ADD COLUMN IF NOT EXISTS postponement_count INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ADD COLUMN IF NOT EXISTS locked_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS locked_by TEXT,
    ADD COLUMN IF NOT EXISTS heartbeat_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS provider_message_id TEXT,
    ADD COLUMN IF NOT EXISTS last_error_code TEXT,
    ADD COLUMN IF NOT EXISTS last_error_class TEXT,
    ADD COLUMN IF NOT EXISTS cancel_reason TEXT;

ALTER TABLE flow_runs
    ADD CONSTRAINT flow_runs_flow_id_fkey
        FOREIGN KEY (flow_id) REFERENCES flows(id) ON DELETE CASCADE,
    ADD CONSTRAINT flow_runs_data_record_id_fkey
        FOREIGN KEY (data_record_id) REFERENCES data_records(id) ON DELETE SET NULL,
    ADD CONSTRAINT flow_runs_contact_id_fkey
        FOREIGN KEY (contact_id) REFERENCES contacts(id) ON DELETE SET NULL,
    ADD CONSTRAINT flow_runs_status_check
        CHECK (status IN ('queued','running','retry_wait','sent','failed','dead','unverified','cancelled')),
    ADD CONSTRAINT flow_runs_source_check
        CHECK (source IN ('schedule','manual','event')),
    ADD CONSTRAINT flow_runs_attempt_check
        CHECK (attempt >= 0 AND max_attempts > 0 AND postponement_count >= 0);

CREATE UNIQUE INDEX IF NOT EXISTS uq_flow_runs_run_key ON flow_runs(run_key);
DROP INDEX IF EXISTS idx_flow_runs_pending;
CREATE INDEX IF NOT EXISTS idx_flow_runs_claim
    ON flow_runs(next_attempt_at, created_at)
    WHERE status IN ('queued','retry_wait');
CREATE INDEX IF NOT EXISTS idx_flow_runs_history
    ON flow_runs(flow_id, scheduled_for DESC);
CREATE INDEX IF NOT EXISTS idx_flow_runs_record
    ON flow_runs(data_record_id, scheduled_for DESC)
    WHERE data_record_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS flow_schedule_occurrences (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    flow_id          UUID NOT NULL REFERENCES flows(id) ON DELETE CASCADE,
    flow_version_id  UUID NOT NULL REFERENCES flow_versions(id) ON DELETE RESTRICT,
    scheduled_for    TIMESTAMPTZ NOT NULL,
    status           TEXT NOT NULL,
    reason           TEXT,
    queued_count     INTEGER NOT NULL DEFAULT 0,
    skipped_count    INTEGER NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(flow_id, scheduled_for),
    CHECK (status IN ('processed','skipped','failed'))
);

CREATE TABLE IF NOT EXISTS waba_delivery_state (
    waba_id          TEXT PRIMARY KEY,
    next_allowed_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    paused_until     TIMESTAMPTZ,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Publicar/reanudar no debe recuperar horarios anteriores a la publicación.
UPDATE flows
   SET last_tick_at = NOW()
 WHERE trigger_type = 'schedule' AND status = 'published' AND last_tick_at IS NULL;
