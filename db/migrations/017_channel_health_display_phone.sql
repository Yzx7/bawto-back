-- Los payloads reales de salud de cuenta **no traen `phone_number_id`**.
-- Comprobado el 2026-08-07 con los eventos de prueba del panel de Meta:
--   account_update              → {"event":"VERIFIED_ACCOUNT","phone_number":"16505551111"}
--   phone_number_quality_update → {"event":"ONBOARDING","display_phone_number":"16505551111",...}
--
-- La migración 016 asumió `phone_number_id` y sin él pasaban dos cosas: todo
-- aviso de un número caía en la fila de la cuenta, y **dos números distintos
-- colisionaban en esa misma fila**, pisándose el estado mutuamente.
--
-- Se conserva el número tal como lo escribe Meta y se resuelve a nuestro
-- phone_number_id contra `bots` cuando se puede, que es el identificador con el
-- que ya trabaja el resto del sistema (bots.channel_id).

ALTER TABLE channel_account_events
    ADD COLUMN IF NOT EXISTS display_phone TEXT;

ALTER TABLE channel_health
    ADD COLUMN IF NOT EXISTS display_phone TEXT;

-- La clave de la proyección pasa a admitir el número de Meta cuando no hay
-- phone_number_id. Se materializa para poder indexarla: PostgreSQL no permite
-- una restricción UNIQUE sobre una expresión, solo un índice único.
ALTER TABLE channel_health
    DROP CONSTRAINT IF EXISTS channel_health_waba_id_phone_number_id_key;

ALTER TABLE channel_health
    ADD COLUMN IF NOT EXISTS scope TEXT
    GENERATED ALWAYS AS (COALESCE(NULLIF(phone_number_id, ''), NULLIF(display_phone, ''), '')) STORED;

CREATE UNIQUE INDEX IF NOT EXISTS idx_channel_health_scope
    ON channel_health (waba_id, scope);
