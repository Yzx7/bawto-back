-- Ledger general de idempotencia para data_mutate. La mutación real sigue en
-- data_records; aquí solo queda la clave que impide repetir un efecto externo.
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
