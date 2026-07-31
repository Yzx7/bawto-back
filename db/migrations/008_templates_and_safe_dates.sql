-- Fase 4: catálogo operativo de plantillas por WABA y casteo seguro de fechas
-- almacenadas en data_records.data (JSONB).

-- PostgreSQL no ofrece TRY_CAST. Esta función devuelve NULL por fila para
-- valores vacíos, formatos no ISO y fechas imposibles (p. ej. 2026-02-31), de
-- modo que un dato legado incorrecto no aborte una campaña completa.
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

-- Las plantillas pertenecen al WABA, no al bot ni a un tipo de negocio. Dos
-- bots conectados al mismo WABA comparten el mismo catálogo sin duplicarlo.
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

-- Conserva los cambios asíncronos de Meta aunque lleguen repetidos o antes de
-- la primera sincronización completa del catálogo.
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
