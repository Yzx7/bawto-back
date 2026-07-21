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
    verify_token  TEXT,
    flow          JSONB       NOT NULL DEFAULT '{}',    -- grafo del flujo (trigger + nodos + aristas)
    ai_config     JSONB,                                -- system prompt, modelo, tools habilitadas
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (channel, channel_id)
);
CREATE INDEX IF NOT EXISTS idx_bots_org ON bots (org_id);

-- CHATS (estado de conversación) ------------------------------
CREATE TABLE IF NOT EXISTS chats (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    bot_id         UUID        NOT NULL REFERENCES bots(id) ON DELETE CASCADE,
    contact        TEXT        NOT NULL,                -- número/usuario final
    contact_name   TEXT,
    current_layer  JSONB       NOT NULL DEFAULT '[]',   -- stack de capas
    mode           TEXT        NOT NULL DEFAULT 'bot',  -- bot | manual (handoff)
    handoff_until  TIMESTAMPTZ,                          -- si NULL en manual: indefinido
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

-- EJECUCIONES DE FLUJO -----------------------------------------
-- Un run representa una ejecución concreta para un registro y su destinatario.
-- run_key impide que reinicios o dos workers manden el mismo recordatorio.
CREATE TABLE IF NOT EXISTS flow_runs (
    id             UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    bot_id         UUID        NOT NULL REFERENCES bots(id) ON DELETE CASCADE,
    flow_id        TEXT        NOT NULL,
    data_record_id UUID        NOT NULL REFERENCES data_records(id) ON DELETE CASCADE,
    contact_id     UUID        NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
    run_key        TEXT        NOT NULL,
    status         TEXT        NOT NULL DEFAULT 'queued',
    context        JSONB       NOT NULL DEFAULT '{}',
    error          TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at     TIMESTAMPTZ,
    finished_at    TIMESTAMPTZ,
    UNIQUE (bot_id, run_key),
    CHECK (status IN ('queued', 'running', 'sent', 'failed', 'cancelled'))
);
CREATE INDEX IF NOT EXISTS idx_flow_runs_pending ON flow_runs (status, created_at) WHERE status IN ('queued', 'failed');

-- MESSAGES ----------------------------------------------------
CREATE TABLE IF NOT EXISTS messages (
    id           BIGSERIAL   PRIMARY KEY,
    chat_id      UUID        NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    wa_id        TEXT,                                  -- id del mensaje en el canal (idempotencia)
    from_me      BOOLEAN     NOT NULL,
    type         TEXT        NOT NULL,
    body         TEXT,
    metadata     JSONB,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_messages_chat ON messages (chat_id, created_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_wa ON messages (wa_id) WHERE wa_id IS NOT NULL;

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
    FOREACH t IN ARRAY ARRAY['organizations','memberships','bots','chats','contacts','billing_records','contact_fields','audiences','data_objects','data_fields','data_records','data_views']
    LOOP
        EXECUTE format('DROP TRIGGER IF EXISTS trg_%1$s_updated_at ON %1$s;', t);
        EXECUTE format(
            'CREATE TRIGGER trg_%1$s_updated_at BEFORE UPDATE ON %1$s
             FOR EACH ROW EXECUTE FUNCTION set_updated_at();', t);
    END LOOP;
END
$$;
