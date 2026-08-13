-- Todo movimiento, incluidos los anteriores al contrato, tiene identidad. Las
-- filas históricas sin referencia reciben la identidad de su propio UUID.

UPDATE organization_credit_transactions
SET idempotency_key = 'legacy-tx:' || id::text
WHERE idempotency_key IS NULL OR btrim(idempotency_key) = '';

DROP INDEX IF EXISTS uq_credit_tx_idempotency;

ALTER TABLE organization_credit_transactions
    ALTER COLUMN idempotency_key SET NOT NULL;

CREATE UNIQUE INDEX uq_credit_tx_idempotency
    ON organization_credit_transactions(idempotency_key);
