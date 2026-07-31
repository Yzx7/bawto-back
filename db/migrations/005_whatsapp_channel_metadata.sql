-- Metadatos de la cuenta de WhatsApp obtenidos durante Embedded Signup.
--
-- Los templates pertenecen a la WABA, no al phone_number_id. Persistir esta
-- relación evita deducirla de los permisos de un token que puede alcanzar más
-- de una WABA y permite que el scheduler aplique pacing por cuenta.

ALTER TABLE bots
    ADD COLUMN IF NOT EXISTS waba_id TEXT,
    ADD COLUMN IF NOT EXISTS business_id TEXT,
    ADD COLUMN IF NOT EXISTS channel_connected_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS templates_synced_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_bots_waba
    ON bots (waba_id)
    WHERE waba_id IS NOT NULL;
