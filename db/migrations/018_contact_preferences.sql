-- Opt-out y opt-in de marketing del usuario (`user_preferences`).
--
-- El campo se suscribió el 2026-08-06 y llegaba sin receptor: un «no me escribas
-- más» se descartaba sin dejar rastro. Hoy no cambia ningún envío porque las tres
-- plantillas de cobranza son UTILITY y el opt-out promocional no las afecta, y
-- precisamente por eso hay que construirlo ahora: el registro tiene que existir
-- **antes** del primer envío de marketing, porque un opt-out perdido no se
-- recupera.
--
-- Va contra el CONTACTO, que ya es la identidad por (org_id, phone_normalized),
-- y no contra el chat ni el bot: la voluntad es de la persona, no de una
-- conversación. Si la misma persona escribe a dos bots de la organización, su
-- decisión vale para ambos.

-- Bitácora append-only. Es un dato de cumplimiento: interesa poder demostrar
-- cuándo dijo que no, no solo cuál es su estado actual. Una proyección sola
-- perdería el historial en cuanto llegara un `resume`.
CREATE TABLE IF NOT EXISTS contact_preference_events (
    id          BIGSERIAL   PRIMARY KEY,
    event_key   TEXT        NOT NULL UNIQUE,   -- idempotencia: Meta reintenta
    contact_id  UUID        NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
    category    TEXT        NOT NULL,          -- valor crudo de Meta
    value       TEXT        NOT NULL,          -- valor crudo de Meta
    detail      TEXT,
    occurred_at TIMESTAMPTZ NOT NULL,
    payload     JSONB       NOT NULL,
    applied_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (jsonb_typeof(payload) = 'object')
);
CREATE INDEX IF NOT EXISTS idx_contact_preference_events_contacto
    ON contact_preference_events (contact_id, occurred_at DESC);

-- Estado vigente, que es lo que consulta el scheduler antes de encolar.
-- Los valores de Meta se guardan crudos, igual que en channel_health: sus enums
-- no están confirmados contra payloads reales.
CREATE TABLE IF NOT EXISTS contact_preferences (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    contact_id  UUID        NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
    category    TEXT        NOT NULL,
    value       TEXT        NOT NULL,
    detail      TEXT,
    -- Guarda de orden, aquí más importante que en ningún otro sitio: un `resume`
    -- atrasado que pisara un `stop` reciente volvería a habilitar envíos a quien
    -- ya dijo que no.
    occurred_at TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (contact_id, category)
);

DROP TRIGGER IF EXISTS trg_contact_preferences_updated_at ON contact_preferences;
CREATE TRIGGER trg_contact_preferences_updated_at
    BEFORE UPDATE ON contact_preferences
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
