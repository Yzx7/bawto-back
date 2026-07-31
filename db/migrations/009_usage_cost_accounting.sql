-- Costos y consumo por organización.
--
-- Meta no envía el importe monetario en el webhook: envía si el mensaje fue
-- facturable y su categoría. Las tarifas se versionan aquí para poder estimar
-- el costo histórico sin fijarlas en el código. `effective_to` es exclusiva.
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

-- Tarifario público de Meta vigente para destinatarios de Perú desde
-- 2026-07-01, moneda PEN. Utility y Authentication tienen descuentos
-- progresivos por volumen mensual; Marketing y Service no.
INSERT INTO provider_rate_cards
    (provider, market, currency, category, tier_from, tier_to, unit_price,
     effective_from, source_url, notes)
VALUES
    ('meta_whatsapp','PE','PEN','utility',       1,       100000, 0.0665,'2026-07-01','https://whatsappbusiness.com/es-la/products/platform-pricing/#rates','Tarifa pública; el nivel real se calcula por portafolio comercial y mes'),
    ('meta_whatsapp','PE','PEN','utility',  100001,      1000000, 0.0632,'2026-07-01','https://whatsappbusiness.com/es-la/products/platform-pricing/#rates','Descuento por volumen 5%'),
    ('meta_whatsapp','PE','PEN','utility', 1000001,      4500000, 0.0599,'2026-07-01','https://whatsappbusiness.com/es-la/products/platform-pricing/#rates','Descuento por volumen 10%'),
    ('meta_whatsapp','PE','PEN','utility', 4500001,     15000000, 0.0565,'2026-07-01','https://whatsappbusiness.com/es-la/products/platform-pricing/#rates','Descuento por volumen 15%'),
    ('meta_whatsapp','PE','PEN','utility',15000001,     30000000, 0.0532,'2026-07-01','https://whatsappbusiness.com/es-la/products/platform-pricing/#rates','Descuento por volumen 20%'),
    ('meta_whatsapp','PE','PEN','utility',30000001,          NULL, 0.0499,'2026-07-01','https://whatsappbusiness.com/es-la/products/platform-pricing/#rates','Descuento por volumen 25%'),
    ('meta_whatsapp','PE','PEN','authentication',       1,       100000, 0.0665,'2026-07-01','https://whatsappbusiness.com/es-la/products/platform-pricing/#rates','Tarifa pública; el nivel real se calcula por portafolio comercial y mes'),
    ('meta_whatsapp','PE','PEN','authentication',  100001,      1000000, 0.0632,'2026-07-01','https://whatsappbusiness.com/es-la/products/platform-pricing/#rates','Descuento por volumen 5%'),
    ('meta_whatsapp','PE','PEN','authentication', 1000001,      4500000, 0.0599,'2026-07-01','https://whatsappbusiness.com/es-la/products/platform-pricing/#rates','Descuento por volumen 10%'),
    ('meta_whatsapp','PE','PEN','authentication', 4500001,     15000000, 0.0565,'2026-07-01','https://whatsappbusiness.com/es-la/products/platform-pricing/#rates','Descuento por volumen 15%'),
    ('meta_whatsapp','PE','PEN','authentication',15000001,     30000000, 0.0532,'2026-07-01','https://whatsappbusiness.com/es-la/products/platform-pricing/#rates','Descuento por volumen 20%'),
    ('meta_whatsapp','PE','PEN','authentication',30000001,          NULL, 0.0499,'2026-07-01','https://whatsappbusiness.com/es-la/products/platform-pricing/#rates','Descuento por volumen 25%'),
    ('meta_whatsapp','PE','PEN','marketing',       1,          NULL, 0.2339,'2026-07-01','https://whatsappbusiness.com/es-la/products/platform-pricing/#rates','Tarifa pública por plantilla entregada'),
    ('meta_whatsapp','PE','PEN','service',         1,          NULL, 0.0000,'2026-07-01','https://whatsappbusiness.com/es-la/products/platform-pricing/#rates','Mensajes de servicio sin plantilla dentro de la ventana de atención')
ON CONFLICT (provider, market, currency, category, tier_from, effective_from)
DO NOTHING;

-- Una fila por respuesta válida del proveedor de IA. Los contadores provienen
-- del objeto `usage` de la API, no de una estimación por longitud del texto.
-- Las tarifas se copian en cada evento para que el costo histórico no cambie
-- cuando se cambie de modelo o de proveedor.
CREATE TABLE IF NOT EXISTS ai_usage_events (
    id                         BIGSERIAL     PRIMARY KEY,
    organization_id            UUID          NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    bot_id                     UUID          REFERENCES bots(id) ON DELETE SET NULL,
    chat_id                    UUID          REFERENCES chats(id) ON DELETE SET NULL,
    inbound_message_id         BIGINT        REFERENCES messages(id) ON DELETE SET NULL,
    provider                   TEXT          NOT NULL,
    model                      TEXT          NOT NULL,
    provider_request_id        TEXT,
    input_tokens               BIGINT        NOT NULL DEFAULT 0,
    output_tokens              BIGINT        NOT NULL DEFAULT 0,
    cache_read_input_tokens    BIGINT        NOT NULL DEFAULT 0,
    cache_creation_input_tokens BIGINT       NOT NULL DEFAULT 0,
    input_usd_per_million      NUMERIC(20,8) NOT NULL,
    output_usd_per_million     NUMERIC(20,8) NOT NULL,
    cache_read_usd_per_million NUMERIC(20,8) NOT NULL,
    cache_write_usd_per_million NUMERIC(20,8) NOT NULL,
    estimated_cost_usd         NUMERIC(20,10) GENERATED ALWAYS AS (
        ROUND((
            input_tokens * input_usd_per_million +
            output_tokens * output_usd_per_million +
            cache_read_input_tokens * cache_read_usd_per_million +
            cache_creation_input_tokens * cache_write_usd_per_million
        ) / 1000000.0, 10)
    ) STORED,
    outcome                    TEXT          NOT NULL DEFAULT 'ok',
    metadata                   JSONB         NOT NULL DEFAULT '{}',
    occurred_at                TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    CHECK (input_tokens >= 0 AND output_tokens >= 0),
    CHECK (cache_read_input_tokens >= 0 AND cache_creation_input_tokens >= 0),
    CHECK (input_usd_per_million >= 0 AND output_usd_per_million >= 0),
    CHECK (cache_read_usd_per_million >= 0 AND cache_write_usd_per_million >= 0),
    CHECK (outcome IN ('ok','invalid_output'))
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_ai_usage_provider_request
    ON ai_usage_events(provider, provider_request_id)
    WHERE provider_request_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_ai_usage_org_time
    ON ai_usage_events(organization_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_ai_usage_bot_time
    ON ai_usage_events(bot_id, occurred_at DESC)
    WHERE bot_id IS NOT NULL;
