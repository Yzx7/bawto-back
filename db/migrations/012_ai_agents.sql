-- Agentes IA reutilizables por organización, referenciados desde un nodo
-- `agent` con `agentRef` (ARQUITECTURA §13, TODO §7).
--
-- El caso que lo motiva: el flujo de Sistemuino tiene ocho consultores que
-- comparten el mismo esqueleto —el contrato de ramas `seguir`/`asesor`/`menu` y
-- las reglas de "no inventes precios ni plazos"— y solo cambian de dominio. Hoy
-- ese texto está copiado ocho veces, así que corregir una regla obliga a editar
-- ocho nodos y confiar en no olvidarse de ninguno.
--
-- Scope de organización, como `data_objects` y `contacts`: un mismo consultor
-- sirve a varios bots del negocio.
CREATE TABLE IF NOT EXISTS ai_agents (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id       UUID        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    key          TEXT        NOT NULL,
    name         TEXT        NOT NULL,
    -- Instrucción base. La del nodo se le añade, no la reemplaza.
    instruction  TEXT        NOT NULL,
    -- Ramas que el agente sabe elegir. El nodo que lo referencia debe declarar
    -- exactamente estas mismas (en el orden que quiera: el orden es la posición
    -- de los puertos en el canvas, una decisión visual del autor del flujo).
    outputs      TEXT[]      NOT NULL,
    context_mode TEXT        NOT NULL DEFAULT 'none',
    silent       BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (org_id, key),
    CHECK (key ~ '^[a-z][a-z0-9_-]{0,62}$'),
    CHECK (length(trim(name)) > 0),
    CHECK (length(trim(instruction)) > 0),
    CHECK (array_length(outputs, 1) BETWEEN 1 AND 32),
    CHECK (context_mode IN ('none', 'recent'))
);

CREATE INDEX IF NOT EXISTS idx_ai_agents_org ON ai_agents (org_id);

DROP TRIGGER IF EXISTS trg_ai_agents_updated_at ON ai_agents;
CREATE TRIGGER trg_ai_agents_updated_at
    BEFORE UPDATE ON ai_agents
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
