-- Pay as you go es prepago puro. Un sobregiro configurable por el propio
-- cliente contradice el producto y convierte el saldo en una línea de crédito.

ALTER TABLE organization_credit_wallets
    DROP CONSTRAINT IF EXISTS chk_credit_wallet_limits;

ALTER TABLE organization_credit_wallets
    DROP COLUMN IF EXISTS allow_overage,
    DROP COLUMN IF EXISTS overage_limit;

ALTER TABLE organization_credit_wallets
    DROP CONSTRAINT IF EXISTS chk_credit_wallet_threshold;
ALTER TABLE organization_credit_wallets
    ADD CONSTRAINT chk_credit_wallet_threshold CHECK (low_balance_threshold >= 0);
