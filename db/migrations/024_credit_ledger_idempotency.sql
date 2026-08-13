-- La clave de idempotencia forma parte del ledger, no de metadata opcional.
-- Su unicidad global permite que un mismo comprobante o request del proveedor
-- no acredite ni descuente dos veces aunque cambie la organización objetivo.

ALTER TABLE organization_credit_transactions
    ADD COLUMN IF NOT EXISTS idempotency_key VARCHAR(220);

UPDATE organization_credit_transactions
SET idempotency_key = CASE
    WHEN NULLIF(metadata->>'idempotency_key', '') IS NOT NULL
        THEN 'legacy:' || organization_id::text || ':' || (metadata->>'idempotency_key')
    WHEN reference_type = 'ai_usage_events' AND NULLIF(reference_id, '') IS NOT NULL
        THEN 'legacy-ai:' || organization_id::text || ':' || type || ':' || reference_id
    ELSE NULL
END
WHERE idempotency_key IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS uq_credit_tx_idempotency
    ON organization_credit_transactions(idempotency_key)
    WHERE idempotency_key IS NOT NULL;

ALTER TABLE organization_credit_transactions
    DROP CONSTRAINT IF EXISTS chk_credit_tx_idempotency_not_blank;
ALTER TABLE organization_credit_transactions
    ADD CONSTRAINT chk_credit_tx_idempotency_not_blank
        CHECK (idempotency_key IS NULL OR btrim(idempotency_key) <> '');
