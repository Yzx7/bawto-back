-- Salud de cuenta de WhatsApp: bitácora de eventos y proyección del estado.
--
-- Seis campos de webhook (account_update, phone_number_quality_update,
-- account_alerts, account_review_update, security, phone_number_name_update)
-- llevaban suscritos en Meta desde el alta de la app sin ningún receptor: sus
-- eventos llegaban y el backend los descartaba sin dejar traza. Con Coexistence
-- eso significaba que una desconexión del teléfono solo se notaba porque dejaban
-- de entrar mensajes.
--
-- No hay una tabla por campo. Los seis responden a la misma pregunta —¿en qué
-- estado está el canal de este bot?— y se resuelven con el patrón que ya funciona
-- en plantillas: bitácora inmutable idempotente + proyección del estado actual,
-- es decir el par channel_template_events / channel_templates aplicado a la cuenta.

-- Bitácora. Conserva el evento tal como llegó: si mañana descubrimos que
-- interpretamos mal un valor, el dato original sigue aquí.
CREATE TABLE IF NOT EXISTS channel_account_events (
    id              BIGSERIAL   PRIMARY KEY,
    -- Idempotencia: Meta reintenta. Igual que en plantillas, el hash cubre campo,
    -- identificadores, instante y valor crudo.
    event_key       TEXT        NOT NULL UNIQUE,
    waba_id         TEXT        NOT NULL,
    -- Nulo en los eventos de cuenta; presente en los de número.
    phone_number_id TEXT,
    field           TEXT        NOT NULL,
    severity        TEXT        NOT NULL DEFAULT 'warning',
    occurred_at     TIMESTAMPTZ NOT NULL,
    payload         JSONB       NOT NULL,
    applied_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (jsonb_typeof(payload) = 'object'),
    CHECK (severity IN ('info', 'warning', 'critical'))
);

CREATE INDEX IF NOT EXISTS idx_channel_account_events_waba
    ON channel_account_events (waba_id, occurred_at DESC);

-- El panel pregunta casi siempre por lo que va mal, no por el histórico completo.
CREATE INDEX IF NOT EXISTS idx_channel_account_events_problemas
    ON channel_account_events (waba_id, occurred_at DESC)
    WHERE severity <> 'info';

-- Proyección del estado actual. La bitácora responde «qué pasó»; el panel
-- necesita «cómo está ahora».
--
-- Los nombres de las columnas describen su origen (quality_event, name_decision)
-- y no un vocabulario propio: los valores concretos de Meta se guardan tal cual.
-- Traducirlos exigiría fijar enums que todavía no se han confirmado contra un
-- payload real, y equivocarse ahí significaría pausar los envíos de un cliente
-- por una restricción que no existe, o no pausarlos por una que sí.
CREATE TABLE IF NOT EXISTS channel_health (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    waba_id             TEXT        NOT NULL,
    phone_number_id     TEXT,
    quality_event       TEXT,       -- phone_number_quality_update.event
    messaging_limit     TEXT,       -- phone_number_quality_update.current_limit
    account_event       TEXT,       -- account_update.event
    review_decision     TEXT,       -- account_review_update.decision
    name_decision       TEXT,       -- phone_number_name_update.decision
    severity            TEXT        NOT NULL DEFAULT 'info',
    last_event_field    TEXT,
    -- Guarda de orden. Los webhooks de Meta no llegan ordenados: sin esto un
    -- evento atrasado pisaría un estado más nuevo y la alarma quedaría encendida
    -- para siempre. Misma cláusula que StoreAndApplyTemplateEvent.
    last_event_at       TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- NULLS NOT DISTINCT (PostgreSQL 15+) para que el evento de WABA sin número
    -- ocupe una sola fila en vez de duplicarse en cada llegada.
    UNIQUE NULLS NOT DISTINCT (waba_id, phone_number_id),
    CHECK (severity IN ('info', 'warning', 'critical'))
);

CREATE INDEX IF NOT EXISTS idx_channel_health_waba
    ON channel_health (waba_id);

DROP TRIGGER IF EXISTS trg_channel_health_updated_at ON channel_health;
CREATE TRIGGER trg_channel_health_updated_at
    BEFORE UPDATE ON channel_health
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
