-- Migración 023: Monedero de créditos y libro mayor (Ledger) Pay as you go
-- 1 USD = 400 créditos. Almacenamiento en NUMERIC(14, 6) para evitar pérdida por redondeo.

CREATE TABLE IF NOT EXISTS organization_credit_wallets (
    organization_id UUID PRIMARY KEY REFERENCES organizations(id) ON DELETE CASCADE,
    balance NUMERIC(14, 6) NOT NULL DEFAULT 0.000000,
    lifetime_credited NUMERIC(14, 6) NOT NULL DEFAULT 0.000000,
    lifetime_consumed NUMERIC(14, 6) NOT NULL DEFAULT 0.000000,
    low_balance_threshold NUMERIC(14, 6) NOT NULL DEFAULT 50.000000,
    allow_overage BOOLEAN NOT NULL DEFAULT FALSE,
    overage_limit NUMERIC(14, 6) NOT NULL DEFAULT 0.000000,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_credit_wallet_limits CHECK (low_balance_threshold >= 0 AND overage_limit >= 0)
);

CREATE TABLE IF NOT EXISTS organization_credit_transactions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    amount NUMERIC(14, 6) NOT NULL,
    balance_after NUMERIC(14, 6) NOT NULL,
    type VARCHAR(32) NOT NULL,
    reference_type VARCHAR(64),
    reference_id VARCHAR(128),
    notes TEXT,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_credit_tx_org_created ON organization_credit_transactions(organization_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_credit_tx_ref ON organization_credit_transactions(reference_type, reference_id);

-- Poblar monederos iniciales para todas las organizaciones existentes
INSERT INTO organization_credit_wallets (organization_id)
SELECT id FROM organizations
ON CONFLICT (organization_id) DO NOTHING;
