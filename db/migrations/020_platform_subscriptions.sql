-- El código enlaza una compra por WhatsApp con una organización ya existente.
-- No concede una sesión: únicamente permite que la acción privilegiada ubique
-- de forma inequívoca dónde aplicar la suscripción registrada en Data.
ALTER TABLE organizations ADD COLUMN IF NOT EXISTS activation_code TEXT;

UPDATE organizations
SET activation_code = upper(substr(encode(gen_random_bytes(8), 'hex'), 1, 10))
WHERE activation_code IS NULL OR trim(activation_code) = '';

ALTER TABLE organizations
    ALTER COLUMN activation_code SET DEFAULT upper(substr(encode(gen_random_bytes(8), 'hex'), 1, 10)),
    ALTER COLUMN activation_code SET NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS uq_organizations_activation_code
    ON organizations (upper(activation_code));

ALTER TABLE organizations DROP CONSTRAINT IF EXISTS organizations_activation_code_check;
ALTER TABLE organizations ADD CONSTRAINT organizations_activation_code_check
    CHECK (activation_code ~ '^[A-F0-9]{10}$');

-- Sólo puede existir un catálogo y un ledger de suscripciones Bawto. La fila
-- sigue perteneciendo a una organización normal; el índice evita que otro
-- tenant suplante la fuente de verdad reservada.
CREATE UNIQUE INDEX IF NOT EXISTS uq_data_objects_planes_bawto
    ON data_objects (key) WHERE key = 'planes_bawto';
CREATE UNIQUE INDEX IF NOT EXISTS uq_data_objects_suscripciones_bawto
    ON data_objects (key) WHERE key = 'suscripciones_bawto';
