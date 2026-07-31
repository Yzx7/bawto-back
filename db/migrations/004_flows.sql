-- Fase 1 del plan multiflujos (§5.1 y §5.2): saca el grafo de `bots.flow`, que
-- solo admite un flujo por bot, a `flows` + `flow_versions`.
--
-- No toca `bots.flow`: durante la fase de compatibilidad la escritura sigue yendo
-- allí y la lectura cae ahí cuando no hay versión publicada (§12).

CREATE TABLE IF NOT EXISTS flows (
    id                    UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    bot_id                UUID        NOT NULL REFERENCES bots(id) ON DELETE CASCADE,
    key                   TEXT        NOT NULL,
    name                  TEXT        NOT NULL,
    trigger_type          TEXT        NOT NULL,
    status                TEXT        NOT NULL DEFAULT 'draft',
    priority              INTEGER     NOT NULL DEFAULT 100,
    is_fallback           BOOLEAN     NOT NULL DEFAULT FALSE,
    draft                 JSONB       NOT NULL DEFAULT '{}',
    published_version_id  UUID,
    created_by            TEXT,
    updated_by            TEXT,
    archived_at           TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (trigger_type IN ('message', 'schedule', 'event')),
    CHECK (status IN ('draft', 'published', 'paused', 'archived')),
    CHECK (key ~ '^[a-z][a-z0-9_-]{0,62}$'),
    CHECK (length(trim(name)) > 0),
    CHECK (jsonb_typeof(draft) = 'object')
);

-- La key se libera al archivar: si no, duplicar un flujo archivado obliga a
-- inventar nombres como "recordatorio-d3-v2". Por eso es un índice único parcial
-- y no un UNIQUE(bot_id, key) de tabla.
CREATE UNIQUE INDEX IF NOT EXISTS uq_flows_bot_key
    ON flows (bot_id, key) WHERE archived_at IS NULL;

-- Solo un flujo `message` vivo puede ser el fallback del bot (§5.1).
CREATE UNIQUE INDEX IF NOT EXISTS uq_flows_bot_fallback
    ON flows (bot_id) WHERE is_fallback AND archived_at IS NULL AND trigger_type = 'message';

-- El dispatcher (fase 7) y el scheduler (fase 2) listan por bot + tipo + estado.
CREATE INDEX IF NOT EXISTS idx_flows_bot_trigger
    ON flows (bot_id, trigger_type, status) WHERE archived_at IS NULL;

CREATE TABLE IF NOT EXISTS flow_versions (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    flow_id       UUID        NOT NULL REFERENCES flows(id) ON DELETE CASCADE,
    version       INTEGER     NOT NULL,
    definition    JSONB       NOT NULL,
    checksum      TEXT        NOT NULL,
    published_by  TEXT,
    published_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (flow_id, version)
);
-- Deliberadamente SIN UNIQUE (flow_id, checksum): impediría restaurar una
-- versión anterior y volver a publicarla, que es justo lo que hace
-- POST /versions/:versionId/restore. El checksum detecta el no-op, no bloquea.
CREATE INDEX IF NOT EXISTS idx_flow_versions_flow ON flow_versions (flow_id, version DESC);

-- La FK va aquí y no en la definición de `flows` porque flow_versions aún no
-- existía; ON DELETE SET NULL evita que borrar una versión rompa el flujo.
ALTER TABLE flows DROP CONSTRAINT IF EXISTS flows_published_version_fk;
ALTER TABLE flows ADD CONSTRAINT flows_published_version_fk
    FOREIGN KEY (published_version_id) REFERENCES flow_versions(id) ON DELETE SET NULL;

DROP TRIGGER IF EXISTS trg_flows_updated_at ON flows;
CREATE TRIGGER trg_flows_updated_at BEFORE UPDATE ON flows
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
