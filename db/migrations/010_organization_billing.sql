-- Facturación comercial de Bawto por organización.
--
-- Esta contabilidad es deliberadamente independiente de provider_rate_cards:
-- Meta cobra directamente al WABA del cliente, mientras estas tablas contienen
-- únicamente precios y documentos emitidos por Bawto/Sistemuino.
CREATE TABLE IF NOT EXISTS organization_billing_profiles (
    organization_id UUID          PRIMARY KEY REFERENCES organizations(id) ON DELETE CASCADE,
    legal_name      TEXT,
    tax_id          TEXT,
    billing_email   TEXT,
    plan_name       TEXT,
    currency        TEXT          NOT NULL DEFAULT 'PEN',
    tax_rate        NUMERIC(7,4)  NOT NULL DEFAULT 0,
    status          TEXT          NOT NULL DEFAULT 'unconfigured',
    billing_day     SMALLINT,
    created_at      TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    CHECK (currency ~ '^[A-Z]{3}$'),
    CHECK (tax_rate >= 0),
    CHECK (status IN ('unconfigured','trial','active','past_due','suspended')),
    CHECK (billing_day IS NULL OR billing_day BETWEEN 1 AND 28)
);

-- Un tarifario puede representar una mensualidad fija, una cantidad fija
-- negociada, bots activos o consumo de IA. unit_size permite, por ejemplo,
-- definir un precio por cada millón de tokens sin perder el consumo original.
CREATE TABLE IF NOT EXISTS organization_service_rates (
    id              UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID          NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    code            TEXT          NOT NULL,
    name            TEXT          NOT NULL,
    description     TEXT,
    metric          TEXT          NOT NULL DEFAULT 'fixed',
    fixed_quantity  NUMERIC(20,6) NOT NULL DEFAULT 1,
    unit_size       NUMERIC(20,6) NOT NULL DEFAULT 1,
    unit_label      TEXT          NOT NULL DEFAULT 'unidad',
    unit_price      NUMERIC(20,6) NOT NULL,
    currency        TEXT          NOT NULL DEFAULT 'PEN',
    active          BOOLEAN       NOT NULL DEFAULT TRUE,
    effective_from  DATE          NOT NULL DEFAULT CURRENT_DATE,
    effective_to    DATE,
    sort_order      INTEGER       NOT NULL DEFAULT 0,
    metadata        JSONB         NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    UNIQUE (organization_id, code, effective_from),
    CHECK (length(trim(code)) > 0 AND length(trim(name)) > 0),
    CHECK (metric IN (
        'fixed','active_bot','ai_request','ai_input_token',
        'ai_output_token','ai_total_token'
    )),
    CHECK (fixed_quantity >= 0 AND unit_size > 0 AND unit_price >= 0),
    CHECK (currency ~ '^[A-Z]{3}$'),
    CHECK (effective_to IS NULL OR effective_to > effective_from)
);
CREATE INDEX IF NOT EXISTS idx_org_service_rates_current
    ON organization_service_rates(organization_id, active, effective_from, sort_order);

-- Los estados de cuenta son snapshots: un cambio posterior de tarifa nunca
-- reescribe un periodo ya emitido.
CREATE TABLE IF NOT EXISTS billing_statements (
    id                       UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id          UUID          NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    period_start             DATE          NOT NULL,
    period_end               DATE          NOT NULL,
    status                   TEXT          NOT NULL DEFAULT 'draft',
    currency                 TEXT          NOT NULL,
    subtotal                 NUMERIC(20,2) NOT NULL,
    tax_rate                 NUMERIC(7,4)  NOT NULL DEFAULT 0,
    tax_amount               NUMERIC(20,2) NOT NULL DEFAULT 0,
    total                    NUMERIC(20,2) NOT NULL,
    issued_at                TIMESTAMPTZ,
    due_at                   TIMESTAMPTZ,
    paid_at                  TIMESTAMPTZ,
    external_document_type   TEXT,
    external_document_number TEXT,
    notes                    TEXT,
    created_at               TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    updated_at               TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    CHECK (period_end > period_start),
    CHECK (status IN ('draft','issued','paid','void')),
    CHECK (currency ~ '^[A-Z]{3}$'),
    CHECK (subtotal >= 0 AND tax_rate >= 0 AND tax_amount >= 0 AND total >= 0)
);
CREATE INDEX IF NOT EXISTS idx_billing_statements_org_period
    ON billing_statements(organization_id, period_start DESC, created_at DESC);

CREATE TABLE IF NOT EXISTS billing_statement_items (
    id              UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    statement_id    UUID          NOT NULL REFERENCES billing_statements(id) ON DELETE CASCADE,
    service_code    TEXT          NOT NULL,
    description     TEXT          NOT NULL,
    metric          TEXT          NOT NULL,
    raw_units       NUMERIC(20,6) NOT NULL,
    quantity        NUMERIC(20,6) NOT NULL,
    unit_label      TEXT          NOT NULL,
    unit_price      NUMERIC(20,6) NOT NULL,
    subtotal        NUMERIC(20,2) NOT NULL,
    sort_order      INTEGER       NOT NULL DEFAULT 0,
    metadata        JSONB         NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    CHECK (raw_units >= 0 AND quantity >= 0 AND unit_price >= 0 AND subtotal >= 0)
);
CREATE INDEX IF NOT EXISTS idx_billing_statement_items_statement
    ON billing_statement_items(statement_id, sort_order, created_at);
