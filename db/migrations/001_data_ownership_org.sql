-- Mueve la propiedad de datos de bots hacia organizaciones.
-- Aplicar una sola vez, después de respaldar la base.
BEGIN;

ALTER TABLE contacts ADD COLUMN IF NOT EXISTS org_id UUID REFERENCES organizations(id) ON DELETE CASCADE;
UPDATE contacts c SET org_id = b.org_id FROM bots b WHERE c.bot_id = b.id AND c.org_id IS NULL;
ALTER TABLE contacts ALTER COLUMN org_id SET NOT NULL;
ALTER TABLE contacts DROP CONSTRAINT IF EXISTS contacts_bot_id_phone_normalized_key;
ALTER TABLE contacts ADD CONSTRAINT contacts_org_phone_normalized_key UNIQUE (org_id, phone_normalized);
ALTER TABLE contacts DROP COLUMN IF EXISTS bot_id;
CREATE INDEX IF NOT EXISTS idx_contacts_org ON contacts (org_id, created_at DESC);

ALTER TABLE contact_fields ADD COLUMN IF NOT EXISTS org_id UUID REFERENCES organizations(id) ON DELETE CASCADE;
UPDATE contact_fields f SET org_id = b.org_id FROM bots b WHERE f.bot_id = b.id AND f.org_id IS NULL;
ALTER TABLE contact_fields ALTER COLUMN org_id SET NOT NULL;
ALTER TABLE contact_fields DROP CONSTRAINT IF EXISTS contact_fields_bot_id_key_key;
ALTER TABLE contact_fields ADD CONSTRAINT contact_fields_org_key_key UNIQUE (org_id, key);
ALTER TABLE contact_fields DROP COLUMN IF EXISTS bot_id;

ALTER TABLE audiences ADD COLUMN IF NOT EXISTS org_id UUID REFERENCES organizations(id) ON DELETE CASCADE;
UPDATE audiences a SET org_id = b.org_id FROM bots b WHERE a.bot_id = b.id AND a.org_id IS NULL;
ALTER TABLE audiences ALTER COLUMN org_id SET NOT NULL;
ALTER TABLE audiences DROP CONSTRAINT IF EXISTS audiences_bot_id_name_key;
ALTER TABLE audiences DROP COLUMN IF EXISTS bot_id;
CREATE INDEX IF NOT EXISTS idx_audiences_org ON audiences (org_id, created_at DESC);

ALTER TABLE data_objects ADD COLUMN IF NOT EXISTS org_id UUID REFERENCES organizations(id) ON DELETE CASCADE;
UPDATE data_objects o SET org_id = b.org_id FROM bots b WHERE o.bot_id = b.id AND o.org_id IS NULL;
ALTER TABLE data_objects ALTER COLUMN org_id SET NOT NULL;
ALTER TABLE data_objects DROP CONSTRAINT IF EXISTS data_objects_bot_id_key_key;
ALTER TABLE data_objects ADD CONSTRAINT data_objects_org_key_key UNIQUE (org_id, key);
ALTER TABLE data_objects DROP COLUMN IF EXISTS bot_id;

COMMIT;
