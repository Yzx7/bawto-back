-- Persistencia del Copilot de autoría de flujos.
--
-- La conversación no es el borrador: el modelo trabaja sobre una copia y solo
-- una propuesta confirmada por un editor puede llegar a `flows.draft`. Guardar
-- los tres snapshots evita reconstruir después lo que el modelo quiso cambiar.

CREATE TABLE IF NOT EXISTS flow_copilot_sessions (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    bot_id          UUID        NOT NULL REFERENCES bots(id) ON DELETE CASCADE,
    flow_id         UUID        NOT NULL REFERENCES flows(id) ON DELETE CASCADE,
    created_by      TEXT        NOT NULL,
    title           TEXT        NOT NULL DEFAULT '',
    status          TEXT        NOT NULL DEFAULT 'active',
    summary         TEXT        NOT NULL DEFAULT '',
    next_sequence   BIGINT      NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    closed_at       TIMESTAMPTZ,
    CHECK (status IN ('active', 'closed')),
    CHECK (next_sequence > 0)
);
CREATE INDEX IF NOT EXISTS idx_flow_copilot_sessions_owner
    ON flow_copilot_sessions(flow_id, created_by, updated_at DESC);

DROP TRIGGER IF EXISTS trg_flow_copilot_sessions_updated_at ON flow_copilot_sessions;
CREATE TRIGGER trg_flow_copilot_sessions_updated_at
    BEFORE UPDATE ON flow_copilot_sessions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS flow_copilot_turns (
    id                         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id                 UUID        NOT NULL REFERENCES flow_copilot_sessions(id) ON DELETE CASCADE,
    organization_id            UUID        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    created_by                 TEXT        NOT NULL,
    sequence                   BIGINT      NOT NULL,
    user_message               TEXT        NOT NULL,
    assistant_message          TEXT,
    status                     TEXT        NOT NULL DEFAULT 'running',
    mode                       TEXT,
    editor_revision            TEXT        NOT NULL,
    persisted_draft_checksum   TEXT        NOT NULL,
    working_draft_checksum     TEXT        NOT NULL,
    tool_trace                 JSONB       NOT NULL DEFAULT '[]',
    playbook_versions          JSONB       NOT NULL DEFAULT '[]',
    capability_hash            TEXT,
    resource_hash              TEXT,
    knowledge_bundle_hash      TEXT,
    error_code                 TEXT,
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at               TIMESTAMPTZ,
    UNIQUE (session_id, sequence),
    CHECK (sequence > 0),
    CHECK (length(user_message) BETWEEN 1 AND 8000),
    CHECK (status IN ('running', 'completed', 'failed', 'cancelled')),
    CHECK (mode IS NULL OR mode IN ('question', 'explanation', 'proposal')),
    CHECK (jsonb_typeof(tool_trace) = 'array'),
    CHECK (jsonb_typeof(playbook_versions) = 'array')
);

-- Un usuario no multiplica gasto abriendo pestañas o sesiones distintas. El
-- servicio cierra turnos huérfanos por timeout antes de intentar crear otro.
CREATE UNIQUE INDEX IF NOT EXISTS uq_flow_copilot_turns_running_user
    ON flow_copilot_turns(organization_id, created_by)
    WHERE status = 'running';
CREATE INDEX IF NOT EXISTS idx_flow_copilot_turns_session
    ON flow_copilot_turns(session_id, sequence);

CREATE TABLE IF NOT EXISTS flow_copilot_proposals (
    id                         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    turn_id                    UUID        NOT NULL UNIQUE REFERENCES flow_copilot_turns(id) ON DELETE CASCADE,
    session_id                 UUID        NOT NULL REFERENCES flow_copilot_sessions(id) ON DELETE CASCADE,
    persisted_base             JSONB       NOT NULL,
    persisted_base_checksum    TEXT        NOT NULL,
    working_base               JSONB       NOT NULL,
    working_base_checksum      TEXT        NOT NULL,
    editor_revision            TEXT        NOT NULL,
    candidate                  JSONB       NOT NULL,
    candidate_checksum         TEXT        NOT NULL,
    operations                 JSONB       NOT NULL DEFAULT '[]',
    diff                       JSONB       NOT NULL DEFAULT '{}',
    assumptions                JSONB       NOT NULL DEFAULT '[]',
    requirements               JSONB       NOT NULL DEFAULT '[]',
    diagnostics                JSONB       NOT NULL DEFAULT '[]',
    playbook_versions          JSONB       NOT NULL DEFAULT '[]',
    knowledge_bundle_hash      TEXT,
    status                     TEXT        NOT NULL DEFAULT 'pending',
    applied_by                 TEXT,
    applied_at                 TIMESTAMPTZ,
    dismissed_by               TEXT,
    dismissed_at               TIMESTAMPTZ,
    undone_by                  TEXT,
    undone_at                  TIMESTAMPTZ,
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (status IN ('pending', 'applied', 'dismissed', 'stale', 'undone')),
    CHECK (jsonb_typeof(persisted_base) = 'object'),
    CHECK (jsonb_typeof(working_base) = 'object'),
    CHECK (jsonb_typeof(candidate) = 'object'),
    CHECK (jsonb_typeof(operations) = 'array'),
    CHECK (jsonb_typeof(diff) = 'object'),
    CHECK (jsonb_typeof(assumptions) = 'array'),
    CHECK (jsonb_typeof(requirements) = 'array'),
    CHECK (jsonb_typeof(diagnostics) = 'array'),
    CHECK (jsonb_typeof(playbook_versions) = 'array')
);

-- Una revisión sustituye la propuesta anterior en la misma transacción; dos
-- tarjetas pendientes para una sesión tendrían una base ambigua.
CREATE UNIQUE INDEX IF NOT EXISTS uq_flow_copilot_proposals_pending_session
    ON flow_copilot_proposals(session_id)
    WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_flow_copilot_proposals_created
    ON flow_copilot_proposals(created_at DESC);

-- Autoría y runtime tienen presupuestos y métricas distintos. El default migra
-- todas las filas históricas al significado que ya tenían.
ALTER TABLE ai_usage_events
    ADD COLUMN IF NOT EXISTS purpose TEXT NOT NULL DEFAULT 'flow_runtime';
ALTER TABLE ai_usage_events
    DROP CONSTRAINT IF EXISTS ai_usage_events_purpose_check;
ALTER TABLE ai_usage_events
    ADD CONSTRAINT ai_usage_events_purpose_check
        CHECK (purpose IN ('flow_runtime', 'flow_authoring'));
CREATE INDEX IF NOT EXISTS idx_ai_usage_org_purpose_time
    ON ai_usage_events(organization_id, purpose, occurred_at DESC);
