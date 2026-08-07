-- ============================================================
-- Schema PostgreSQL — sacs-chatbots (DOMINIO)
-- ============================================================
-- Las tablas de IDENTIDAD (user, session, account, verification, jwks) las crea
-- y administra **Better Auth** (frontend) vía su CLI de migración. Este schema
-- solo define el dominio y referencia la tabla "user"(id) TEXT de Better Auth.
-- Orden de aplicación:
--   1) better-auth migrate   (crea identidad)
--   2) este schema.sql       (crea dominio)
-- ============================================================

CREATE EXTENSION IF NOT EXISTS "pgcrypto";
CREATE EXTENSION IF NOT EXISTS vector;   -- pgvector (RAG por bot)

-- ORGANIZATIONS (un negocio = una org) ------------------------
CREATE TABLE IF NOT EXISTS organizations (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT        NOT NULL,
    ruc         TEXT,
    cel         TEXT,
    created_by  TEXT        REFERENCES "user"(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (length(trim(name)) > 0)
);

-- MEMBERSHIPS (roles por org: multi-tenant) -------------------
CREATE TABLE IF NOT EXISTS memberships (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     TEXT        NOT NULL REFERENCES "user"(id) ON DELETE CASCADE,
    org_id      UUID        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    role        TEXT        NOT NULL DEFAULT 'viewer',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (org_id, user_id),
    CHECK (role IN ('owner', 'admin', 'member', 'viewer'))
);
CREATE INDEX IF NOT EXISTS idx_memberships_org  ON memberships (org_id);
CREATE INDEX IF NOT EXISTS idx_memberships_user ON memberships (user_id);

-- BOTS --------------------------------------------------------
CREATE TABLE IF NOT EXISTS bots (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id        UUID        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name          TEXT        NOT NULL DEFAULT '',
    channel       TEXT        NOT NULL DEFAULT 'wsp',   -- wsp | tlgrm | ig | msgr
    channel_id    TEXT,                                 -- phone_number_id (WA); NULL hasta conectar canal
    phone         TEXT,
    token_enc     BYTEA,                                -- token del canal CIFRADO (NULL hasta conectar)
    waba_id       TEXT,                                 -- cuenta WA propietaria del número/templates
    business_id   TEXT,                                 -- portfolio comercial, si Meta lo devuelve
    channel_connected_at TIMESTAMPTZ,
    templates_synced_at  TIMESTAMPTZ,
    verify_token  TEXT,
    -- El grafo no vive aquí: está en `flows` / `flow_versions` (ver más abajo).
    ai_config     JSONB,                                -- system prompt, modelo, tools habilitadas
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (channel, channel_id)
);
CREATE INDEX IF NOT EXISTS idx_bots_org ON bots (org_id);
CREATE INDEX IF NOT EXISTS idx_bots_waba ON bots (waba_id) WHERE waba_id IS NOT NULL;

-- CHATS (estado de conversación) ------------------------------
CREATE TABLE IF NOT EXISTS chats (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    bot_id         UUID        NOT NULL REFERENCES bots(id) ON DELETE CASCADE,
    contact        TEXT        NOT NULL,                -- número/usuario final
    contact_name   TEXT,
    current_layer  JSONB       NOT NULL DEFAULT '[]',   -- stack de capas
    mode           TEXT        NOT NULL DEFAULT 'bot',  -- bot | manual (handoff)
    handoff_until  TIMESTAMPTZ,                          -- si NULL en manual: indefinido
    last_read_at   TIMESTAMPTZ,                          -- corte de "no leídos" en la bandeja
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (bot_id, contact),
    CHECK (mode IN ('bot', 'manual'))
);
CREATE INDEX IF NOT EXISTS idx_chats_bot ON chats (bot_id, updated_at DESC);

-- CONTACTS / CRM ------------------------------------------------
-- El teléfono se normaliza a dígitos para que el mensaje entrante se resuelva
-- inequívocamente dentro del bot que recibió el webhook.
CREATE TABLE IF NOT EXISTS contacts (
    id               UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id           UUID        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    phone_normalized TEXT        NOT NULL,
    name             TEXT,
    data             JSONB       NOT NULL DEFAULT '{}', -- plan, zona, DNI, etc.
    status           TEXT        NOT NULL DEFAULT 'active',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (org_id, phone_normalized),
    CHECK (phone_normalized ~ '^[0-9]{6,20}$'),
    CHECK (status IN ('active', 'inactive', 'blocked'))
);
CREATE INDEX IF NOT EXISTS idx_contacts_org ON contacts (org_id, created_at DESC);

-- Un cliente puede tener varios cobros. El scheduler seleccionará los pending
-- según due_date; cada periodo conserva su propio importe y evidencia.
CREATE TABLE IF NOT EXISTS billing_records (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    contact_id    UUID        NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
    period        TEXT        NOT NULL,                 -- ej. 2026-08
    amount        NUMERIC(12,2) NOT NULL CHECK (amount >= 0),
    currency      TEXT        NOT NULL DEFAULT 'PEN',
    due_date      DATE        NOT NULL,
    status        TEXT        NOT NULL DEFAULT 'pending',
    paid_at       TIMESTAMPTZ,
    evidence      JSONB       NOT NULL DEFAULT '{}',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (contact_id, period),
    CHECK (status IN ('pending', 'paid', 'overdue', 'cancelled'))
);
CREATE INDEX IF NOT EXISTS idx_billing_due_pending
    ON billing_records (due_date, contact_id) WHERE status IN ('pending', 'overdue');

-- ESQUEMAS Y AUDIENCIAS ----------------------------------------
-- Los campos definen el contrato visible para cada ISP; los valores siguen en
-- contacts.data para evitar DDL dinámico por cliente.
CREATE TABLE IF NOT EXISTS contact_fields (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      UUID        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    key         TEXT        NOT NULL,
    label       TEXT        NOT NULL,
    type        TEXT        NOT NULL DEFAULT 'text',
    required    BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (org_id, key),
    CHECK (key ~ '^[a-z][a-z0-9_]{0,62}$'),
    CHECK (type IN ('text', 'number', 'date', 'boolean'))
);

CREATE TABLE IF NOT EXISTS audiences (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      UUID        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name        TEXT        NOT NULL,
    kind        TEXT        NOT NULL DEFAULT 'dynamic', -- dynamic | manual
    filter      JSONB       NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (kind IN ('dynamic', 'manual')),
    CHECK (length(trim(name)) > 0)
);
CREATE INDEX IF NOT EXISTS idx_audiences_org ON audiences (org_id, created_at DESC);

CREATE TABLE IF NOT EXISTS audience_contacts (
    audience_id UUID NOT NULL REFERENCES audiences(id) ON DELETE CASCADE,
    contact_id  UUID NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (audience_id, contact_id)
);

-- DATOS GENÉRICOS ----------------------------------------------
-- El producto no modela "facturas", "citas" o "pedidos" en su núcleo. Cada
-- organización define objetos, campos y registros; Contacts queda especial solo
-- porque representa al destinatario de los canales de mensajería.
CREATE TABLE IF NOT EXISTS data_objects (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      UUID        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    key         TEXT        NOT NULL,
    name        TEXT        NOT NULL,
    plural_name TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (org_id, key),
    CHECK (key ~ '^[a-z][a-z0-9_]{0,62}$')
);

CREATE TABLE IF NOT EXISTS data_fields (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    object_id   UUID        NOT NULL REFERENCES data_objects(id) ON DELETE CASCADE,
    key         TEXT        NOT NULL,
    label       TEXT        NOT NULL,
    type        TEXT        NOT NULL DEFAULT 'text',
    required    BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (object_id, key),
    CHECK (key ~ '^[a-z][a-z0-9_]{0,62}$'),
    CHECK (type IN ('text', 'number', 'date', 'boolean', 'json'))
);

CREATE TABLE IF NOT EXISTS data_records (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    object_id   UUID        NOT NULL REFERENCES data_objects(id) ON DELETE CASCADE,
    data        JSONB       NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (jsonb_typeof(data) = 'object')
);
CREATE INDEX IF NOT EXISTS idx_data_records_object ON data_records (object_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_data_records_data ON data_records USING GIN (data);

-- IDEMPOTENCIA DE MUTACIONES GENERALES ----------------------
CREATE TABLE IF NOT EXISTS data_record_mutations (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id        UUID        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    object_id     UUID        NOT NULL REFERENCES data_objects(id) ON DELETE CASCADE,
    record_id     UUID        NOT NULL REFERENCES data_records(id) ON DELETE CASCADE,
    mutation_key  TEXT        NOT NULL,
    operation     TEXT        NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (org_id, object_id, mutation_key),
    CHECK (length(mutation_key) BETWEEN 1 AND 200),
    CHECK (operation IN ('create','update','upsert'))
);
CREATE INDEX IF NOT EXISTS idx_data_record_mutations_record
    ON data_record_mutations (record_id, created_at DESC);

-- Una relación puede unir cualquier registro a un contacto (destinatario), sin
-- asumir que el objeto sea una factura, pedido o cita.
CREATE TABLE IF NOT EXISTS data_record_contacts (
    record_id   UUID NOT NULL REFERENCES data_records(id) ON DELETE CASCADE,
    contact_id  UUID NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
    role        TEXT NOT NULL DEFAULT 'primary',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (record_id, contact_id, role)
);
CREATE INDEX IF NOT EXISTS idx_data_record_contacts_contact ON data_record_contacts (contact_id);

CREATE TABLE IF NOT EXISTS data_views (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    object_id   UUID        NOT NULL REFERENCES data_objects(id) ON DELETE CASCADE,
    name        TEXT        NOT NULL,
    filter      JSONB       NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (jsonb_typeof(filter) = 'object')
);

-- PLANTILLAS DE CANAL (catálogo WABA) --------------------------
-- Un valor JSONB mal escrito nunca debe abortar la resolución de una vista.
CREATE OR REPLACE FUNCTION safe_iso_date(value TEXT)
RETURNS DATE
LANGUAGE plpgsql
IMMUTABLE
PARALLEL SAFE
AS $$
BEGIN
    IF value IS NULL OR value !~ '^\d{4}-\d{2}-\d{2}$' THEN
        RETURN NULL;
    END IF;
    RETURN value::date;
EXCEPTION
    WHEN invalid_datetime_format OR datetime_field_overflow THEN
        RETURN NULL;
END;
$$;

CREATE TABLE IF NOT EXISTS channel_templates (
    id                         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    waba_id                    TEXT        NOT NULL,
    meta_template_id           TEXT,
    name                       TEXT        NOT NULL,
    language                   TEXT        NOT NULL,
    status                     TEXT        NOT NULL DEFAULT 'UNKNOWN',
    category                   TEXT,
    quality_score              TEXT,
    parameter_format           TEXT,
    components                 JSONB       NOT NULL DEFAULT '[]',
    parameter_schema           JSONB       NOT NULL DEFAULT '[]',
    body_parameter_count       INTEGER     NOT NULL DEFAULT 0,
    has_unsupported_parameters BOOLEAN     NOT NULL DEFAULT FALSE,
    rejected_reason            TEXT,
    pending_category           TEXT,
    category_change_at         TIMESTAMPTZ,
    meta_updated_at            TIMESTAMPTZ,
    last_event_at              TIMESTAMPTZ,
    last_synced_at             TIMESTAMPTZ,
    sync_marker                TEXT,
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (waba_id, name, language),
    CHECK (jsonb_typeof(components) = 'array'),
    CHECK (jsonb_typeof(parameter_schema) = 'array'),
    CHECK (body_parameter_count >= 0)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_channel_templates_meta_id
    ON channel_templates (waba_id, meta_template_id)
    WHERE meta_template_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_channel_templates_waba_status
    ON channel_templates (waba_id, status, category, name, language);

CREATE TABLE IF NOT EXISTS channel_template_events (
    id                BIGSERIAL   PRIMARY KEY,
    event_key         TEXT        NOT NULL UNIQUE,
    waba_id           TEXT        NOT NULL,
    field             TEXT        NOT NULL,
    meta_template_id  TEXT,
    template_name     TEXT,
    template_language TEXT,
    occurred_at       TIMESTAMPTZ NOT NULL,
    payload           JSONB       NOT NULL,
    applied_at        TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (jsonb_typeof(payload) = 'object')
);
CREATE INDEX IF NOT EXISTS idx_channel_template_events_waba
    ON channel_template_events (waba_id, occurred_at DESC);

-- SALUD DE CUENTA ----------------------------------------------
-- Mismo patrón que las plantillas —bitácora inmutable + proyección del estado—
-- aplicado a los seis campos de webhook que describen el canal: account_update,
-- phone_number_quality_update, account_alerts, account_review_update, security y
-- phone_number_name_update. No hay una tabla por campo: todos responden a la
-- misma pregunta, ¿en qué estado está el canal de este bot?
-- Espejo de db/migrations/016_channel_account_events.sql.
CREATE TABLE IF NOT EXISTS channel_account_events (
    id              BIGSERIAL   PRIMARY KEY,
    event_key       TEXT        NOT NULL UNIQUE,
    waba_id         TEXT        NOT NULL,
    phone_number_id TEXT,                                 -- resuelto contra bots
    display_phone   TEXT,                                 -- el número tal como lo escribe Meta
    field           TEXT        NOT NULL,
    severity        TEXT        NOT NULL DEFAULT 'warning',
    occurred_at     TIMESTAMPTZ NOT NULL,
    payload         JSONB       NOT NULL,                  -- el evento tal como llegó
    applied_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (jsonb_typeof(payload) = 'object'),
    CHECK (severity IN ('info', 'warning', 'critical'))
);
CREATE INDEX IF NOT EXISTS idx_channel_account_events_waba
    ON channel_account_events (waba_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_channel_account_events_problemas
    ON channel_account_events (waba_id, occurred_at DESC)
    WHERE severity <> 'info';

-- Los nombres de columna describen su origen y no un vocabulario propio: los
-- valores de Meta se guardan tal cual porque sus enums no están confirmados
-- contra payloads reales.
CREATE TABLE IF NOT EXISTS channel_health (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    waba_id             TEXT        NOT NULL,
    phone_number_id     TEXT,
    display_phone       TEXT,       -- Meta identifica el número por aquí, no por id
    quality_event       TEXT,       -- phone_number_quality_update.event
    messaging_limit     TEXT,       -- phone_number_quality_update.current_limit
    account_event       TEXT,       -- account_update.event
    review_decision     TEXT,       -- account_review_update.decision
    name_decision       TEXT,       -- phone_number_name_update.decision
    severity            TEXT        NOT NULL DEFAULT 'info',
    last_event_field    TEXT,
    last_event_at       TIMESTAMPTZ,                       -- guarda de orden
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Los payloads reales no traen phone_number_id: sin `scope`, dos números
    -- distintos colisionaban en la fila de la cuenta y se pisaban el estado.
    scope               TEXT GENERATED ALWAYS AS
        (COALESCE(NULLIF(phone_number_id, ''), NULLIF(display_phone, ''), '')) STORED,
    CHECK (severity IN ('info', 'warning', 'critical'))
);
CREATE INDEX IF NOT EXISTS idx_channel_health_waba ON channel_health (waba_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_channel_health_scope
    ON channel_health (waba_id, scope);

-- AGENTES IA REUTILIZABLES -------------------------------------
-- Un nodo `agent` puede referenciar uno con `agentRef` en vez de repetir la
-- instrucción: el agente aporta la base y el nodo le añade su dominio. Se
-- expande al ejecutar, no al publicar, para que corregir una regla aquí valga
-- para todos los flujos que lo usan sin republicarlos.
-- Espejo de db/migrations/012_ai_agents.sql.
CREATE TABLE IF NOT EXISTS ai_agents (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id       UUID        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    key          TEXT        NOT NULL,
    name         TEXT        NOT NULL,
    instruction  TEXT        NOT NULL,
    outputs      TEXT[]      NOT NULL,   -- ramas que sabe elegir
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

-- FLUJOS (multiflujo por bot) ----------------------------------
-- Única fuente de verdad del grafo: un bot tiene a la vez su flujo de atención
-- `message` y varios `schedule`, cada uno con su borrador y sus versiones
-- inmutables. El webhook ejecuta la versión publicada.
-- Espejo de db/migrations/004_flows.sql y 011_drop_bots_flow.sql; PLAN §5.1 y §5.2.
CREATE TABLE IF NOT EXISTS flows (
    id                    UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    bot_id                UUID        NOT NULL REFERENCES bots(id) ON DELETE CASCADE,
    key                   TEXT        NOT NULL,
    name                  TEXT        NOT NULL,
    trigger_type          TEXT        NOT NULL,                -- message | schedule | event
    status                TEXT        NOT NULL DEFAULT 'draft',-- draft | published | paused | archived
    priority              INTEGER     NOT NULL DEFAULT 100,
    is_fallback           BOOLEAN     NOT NULL DEFAULT FALSE,
    draft                 JSONB       NOT NULL DEFAULT '{}',
    published_version_id  UUID,
    created_by            TEXT,
    updated_by            TEXT,
    archived_at           TIMESTAMPTZ,                          -- archivar libera la key
    last_tick_at          TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (trigger_type IN ('message', 'schedule', 'event')),
    CHECK (status IN ('draft', 'published', 'paused', 'archived')),
    CHECK (key ~ '^[a-z][a-z0-9_-]{0,62}$'),
    CHECK (length(trim(name)) > 0),
    CHECK (jsonb_typeof(draft) = 'object')
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_flows_bot_key
    ON flows (bot_id, key) WHERE archived_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_flows_bot_fallback
    ON flows (bot_id) WHERE is_fallback AND archived_at IS NULL AND trigger_type = 'message';
CREATE INDEX IF NOT EXISTS idx_flows_bot_trigger
    ON flows (bot_id, trigger_type, status) WHERE archived_at IS NULL;

-- Una versión publicada es inmutable: fija la definición que ejecuta cada run.
-- Sin UNIQUE (flow_id, checksum) a propósito: bloquearía restaurar y republicar.
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
CREATE INDEX IF NOT EXISTS idx_flow_versions_flow ON flow_versions (flow_id, version DESC);

ALTER TABLE flows DROP CONSTRAINT IF EXISTS flows_published_version_fk;
ALTER TABLE flows ADD CONSTRAINT flows_published_version_fk
    FOREIGN KEY (published_version_id) REFERENCES flow_versions(id) ON DELETE SET NULL;

-- EJECUCIONES DE FLUJO -----------------------------------------
-- Un run representa una ejecución concreta para un registro y su destinatario.
-- run_key impide que reinicios o dos workers manden el mismo recordatorio.
CREATE TABLE IF NOT EXISTS flow_runs (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    bot_id         UUID        NOT NULL REFERENCES bots(id) ON DELETE CASCADE,
    flow_id        UUID        NOT NULL REFERENCES flows(id) ON DELETE CASCADE,
    flow_version_id UUID       NOT NULL REFERENCES flow_versions(id) ON DELETE RESTRICT,
    data_record_id UUID        REFERENCES data_records(id) ON DELETE SET NULL,
    contact_id     UUID        REFERENCES contacts(id) ON DELETE SET NULL,
    run_key        TEXT        NOT NULL,
    status         TEXT        NOT NULL DEFAULT 'queued',
    scheduled_for  TIMESTAMPTZ NOT NULL,
    source         TEXT        NOT NULL DEFAULT 'schedule',
    attempt        INTEGER     NOT NULL DEFAULT 0,
    max_attempts   INTEGER     NOT NULL DEFAULT 5,
    postponement_count INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    locked_at      TIMESTAMPTZ,
    locked_by      TEXT,
    heartbeat_at   TIMESTAMPTZ,
    provider_message_id TEXT,
    context        JSONB       NOT NULL DEFAULT '{}',
    last_error_code TEXT,
    last_error_class TEXT,
    last_error     TEXT,
    cancel_reason  TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at     TIMESTAMPTZ,
    finished_at    TIMESTAMPTZ,
    provider_status_at TIMESTAMPTZ,
    delivered_at TIMESTAMPTZ,
    read_at TIMESTAMPTZ,
    played_at TIMESTAMPTZ,
    conversation_id TEXT,
    conversation_type TEXT,
    pricing_model TEXT,
    pricing_type TEXT,
    pricing_category TEXT,
    billable BOOLEAN,
    UNIQUE (run_key),
    CHECK (status IN ('queued','running','retry_wait','sent','delivered','read','played','failed','dead','unverified','cancelled')),
    CHECK (source IN ('schedule','manual','event'))
);
CREATE INDEX IF NOT EXISTS idx_flow_runs_claim ON flow_runs (next_attempt_at, created_at) WHERE status IN ('queued','retry_wait');
CREATE INDEX IF NOT EXISTS idx_flow_runs_history ON flow_runs (flow_id, scheduled_for DESC);
CREATE INDEX IF NOT EXISTS idx_flow_runs_record ON flow_runs (data_record_id, scheduled_for DESC) WHERE data_record_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_flow_runs_provider_message ON flow_runs(provider_message_id) WHERE provider_message_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS flow_schedule_occurrences (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    flow_id UUID NOT NULL REFERENCES flows(id) ON DELETE CASCADE,
    flow_version_id UUID NOT NULL REFERENCES flow_versions(id) ON DELETE RESTRICT,
    scheduled_for TIMESTAMPTZ NOT NULL,
    status TEXT NOT NULL,
    reason TEXT,
    queued_count INTEGER NOT NULL DEFAULT 0,
    skipped_count INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(flow_id, scheduled_for),
    CHECK (status IN ('processed','skipped','failed'))
);

CREATE TABLE IF NOT EXISTS waba_delivery_state (
    waba_id TEXT PRIMARY KEY,
    next_allowed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    paused_until TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- MESSAGES ----------------------------------------------------
CREATE TABLE IF NOT EXISTS messages (
    id           BIGSERIAL   PRIMARY KEY,
    chat_id      UUID        NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    wa_id        TEXT,                                  -- id del mensaje en el canal (idempotencia)
    from_me      BOOLEAN     NOT NULL,
    type         TEXT        NOT NULL,
    body         TEXT,
    metadata     JSONB,
    provider_status TEXT,
    provider_status_at TIMESTAMPTZ,
    provider_error_code TEXT,
    provider_error TEXT,
    conversation_id TEXT,
    conversation_type TEXT,
    pricing_model TEXT,
    pricing_type TEXT,
    pricing_category TEXT,
    billable BOOLEAN,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (provider_status IS NULL OR provider_status IN ('sent','delivered','read','played','failed'))
);
CREATE INDEX IF NOT EXISTS idx_messages_chat ON messages (chat_id, created_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_wa ON messages (wa_id) WHERE wa_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS provider_status_events (
    id BIGSERIAL PRIMARY KEY,
    event_key TEXT NOT NULL UNIQUE,
    channel TEXT NOT NULL,
    channel_id TEXT,
    provider_message_id TEXT NOT NULL,
    status TEXT NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    recipient_id TEXT,
    error_code TEXT,
    error_title TEXT,
    error_message TEXT,
    error_details TEXT,
    conversation_id TEXT,
    conversation_type TEXT,
    pricing_model TEXT,
    pricing_type TEXT,
    pricing_category TEXT,
    billable BOOLEAN,
    opaque_callback_data TEXT,
    metadata JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    applied_at TIMESTAMPTZ,
    CHECK (status IN ('sent','delivered','read','played','failed'))
);
CREATE INDEX IF NOT EXISTS idx_provider_status_pending ON provider_status_events(provider_message_id, occurred_at, id) WHERE applied_at IS NULL;

-- COSTOS Y CONSUMO --------------------------------------------
-- Meta solo informa billable/categoría en el webhook. Las tarifas públicas
-- viven versionadas para convertir esos eventos en una estimación auditable.
CREATE TABLE IF NOT EXISTS provider_rate_cards (
    id             UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    provider       TEXT          NOT NULL,
    market         TEXT          NOT NULL,
    currency       TEXT          NOT NULL,
    category       TEXT          NOT NULL,
    tier_from      BIGINT        NOT NULL DEFAULT 1,
    tier_to        BIGINT,
    unit_price     NUMERIC(20,8) NOT NULL,
    effective_from DATE          NOT NULL,
    effective_to   DATE,
    source_url     TEXT          NOT NULL,
    notes          TEXT,
    created_at     TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    UNIQUE (provider, market, currency, category, tier_from, effective_from),
    CHECK (tier_from > 0),
    CHECK (tier_to IS NULL OR tier_to >= tier_from),
    CHECK (unit_price >= 0),
    CHECK (effective_to IS NULL OR effective_to > effective_from)
);
CREATE INDEX IF NOT EXISTS idx_provider_rate_cards_lookup
    ON provider_rate_cards(provider, market, category, effective_from, tier_from);

CREATE TABLE IF NOT EXISTS ai_usage_events (
    id                          BIGSERIAL     PRIMARY KEY,
    organization_id             UUID          NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    bot_id                      UUID          REFERENCES bots(id) ON DELETE SET NULL,
    chat_id                     UUID          REFERENCES chats(id) ON DELETE SET NULL,
    inbound_message_id          BIGINT        REFERENCES messages(id) ON DELETE SET NULL,
    provider                    TEXT          NOT NULL,
    model                       TEXT          NOT NULL,
    provider_request_id         TEXT,
    input_tokens                BIGINT        NOT NULL DEFAULT 0,
    output_tokens               BIGINT        NOT NULL DEFAULT 0,
    cache_read_input_tokens     BIGINT        NOT NULL DEFAULT 0,
    cache_creation_input_tokens BIGINT        NOT NULL DEFAULT 0,
    input_usd_per_million       NUMERIC(20,8) NOT NULL,
    output_usd_per_million      NUMERIC(20,8) NOT NULL,
    cache_read_usd_per_million  NUMERIC(20,8) NOT NULL,
    cache_write_usd_per_million NUMERIC(20,8) NOT NULL,
    estimated_cost_usd          NUMERIC(20,10) GENERATED ALWAYS AS (
        ROUND((
            input_tokens * input_usd_per_million +
            output_tokens * output_usd_per_million +
            cache_read_input_tokens * cache_read_usd_per_million +
            cache_creation_input_tokens * cache_write_usd_per_million
        ) / 1000000.0, 10)
    ) STORED,
    outcome                     TEXT          NOT NULL DEFAULT 'ok',
    metadata                    JSONB         NOT NULL DEFAULT '{}',
    occurred_at                 TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    CHECK (input_tokens >= 0 AND output_tokens >= 0),
    CHECK (cache_read_input_tokens >= 0 AND cache_creation_input_tokens >= 0),
    CHECK (input_usd_per_million >= 0 AND output_usd_per_million >= 0),
    CHECK (cache_read_usd_per_million >= 0 AND cache_write_usd_per_million >= 0),
    CHECK (outcome IN ('ok','invalid_output'))
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_ai_usage_provider_request
    ON ai_usage_events(provider, provider_request_id)
    WHERE provider_request_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_ai_usage_org_time ON ai_usage_events(organization_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_ai_usage_bot_time ON ai_usage_events(bot_id, occurred_at DESC) WHERE bot_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS message_correlations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    inbound_message_id BIGINT NOT NULL UNIQUE REFERENCES messages(id) ON DELETE CASCADE,
    outbound_message_id BIGINT REFERENCES messages(id) ON DELETE SET NULL,
    flow_run_id UUID REFERENCES flow_runs(id) ON DELETE SET NULL,
    data_record_id UUID REFERENCES data_records(id) ON DELETE SET NULL,
    method TEXT NOT NULL,
    quoted_provider_message_id TEXT,
    candidate_count INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (method IN ('exact','inferred','ambiguous','none'))
);

-- MEDIA -------------------------------------------------------
-- Conserva una copia durable: los media_id/URL de WhatsApp expiran.
CREATE TABLE IF NOT EXISTS message_media (
    message_id   BIGINT      PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
    provider_id  TEXT        NOT NULL,
    mime_type    TEXT        NOT NULL,
    data         BYTEA       NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (octet_length(data) <= 10485760)
);

-- KNOWLEDGE (RAG por bot) -------------------------------------
CREATE TABLE IF NOT EXISTS bot_knowledge (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    bot_id      UUID        NOT NULL REFERENCES bots(id) ON DELETE CASCADE,
    content     TEXT        NOT NULL,
    embedding   vector(1536),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_knowledge_bot ON bot_knowledge (bot_id);

-- ============================================================
-- FUNCIÓN + TRIGGERS: updated_at automático
-- ============================================================
CREATE OR REPLACE FUNCTION set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['organizations','memberships','bots','chats','contacts','billing_records','contact_fields','audiences','data_objects','data_fields','data_records','data_views','flows','channel_health']
    LOOP
        EXECUTE format('DROP TRIGGER IF EXISTS trg_%1$s_updated_at ON %1$s;', t);
        EXECUTE format(
            'CREATE TRIGGER trg_%1$s_updated_at BEFORE UPDATE ON %1$s
             FOR EACH ROW EXECUTE FUNCTION set_updated_at();', t);
    END LOOP;
END
$$;
