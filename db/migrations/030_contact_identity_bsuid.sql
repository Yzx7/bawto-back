-- La identidad de un contacto deja de ser su teléfono.
--
-- Desde que WhatsApp tiene nombres de usuario, Meta puede omitir `from` y
-- `contacts[].wa_id` en el webhook y entregar solo el BSUID (business-scoped
-- user ID), que es el único identificador que garantiza en todos los mensajes.
-- El teléfono pasa a ser un atributo que se *aprende* cuando Meta lo manda —y
-- que deja de venir tras 30 días sin interacción—, así que ya no puede ser ni
-- obligatorio ni la clave por la que se reconoce a una persona.

-- 1. contacts: dos identidades, cualquiera de las dos basta.
ALTER TABLE contacts ADD COLUMN IF NOT EXISTS channel_user_id TEXT;
ALTER TABLE contacts ADD COLUMN IF NOT EXISTS username TEXT;
ALTER TABLE contacts ALTER COLUMN phone_normalized DROP NOT NULL;

-- El CHECK del formato databa de cuando el teléfono era obligatorio.
ALTER TABLE contacts DROP CONSTRAINT IF EXISTS contacts_phone_normalized_check;
ALTER TABLE contacts ADD CONSTRAINT contacts_phone_normalized_check
    CHECK (phone_normalized IS NULL OR phone_normalized ~ '^[0-9]{6,20}$');

-- Un contacto sin ninguna de las dos identidades no se puede ni reconocer ni
-- contestar: es una fila muerta, como las que creaba el webhook antes de esto.
ALTER TABLE contacts ADD CONSTRAINT contacts_identity_check
    CHECK (phone_normalized IS NOT NULL OR channel_user_id IS NOT NULL);

-- UNIQUE (org_id, phone_normalized) sigue existiendo y ahora admite varios
-- NULL, que es justo lo que hace falta: muchos contactos sin teléfono visible.
CREATE UNIQUE INDEX IF NOT EXISTS idx_contacts_channel_user
    ON contacts (org_id, channel_user_id) WHERE channel_user_id IS NOT NULL;

-- 2. chats: apuntan al contacto, no a una cadena de teléfono.
ALTER TABLE chats ADD COLUMN IF NOT EXISTS contact_id UUID REFERENCES contacts(id) ON DELETE CASCADE;

-- Los chats que ya existen se enlazan por el teléfono que guardaban. El que no
-- tenga contacto se lo crea aquí: `chats.contact` era hasta ahora la única
-- prueba de que esa persona escribió alguna vez.
INSERT INTO contacts (org_id, phone_normalized, name)
SELECT DISTINCT ON (b.org_id, c.contact) b.org_id, c.contact, NULLIF(c.contact_name, '')
FROM chats c
JOIN bots b ON b.id = c.bot_id
WHERE c.contact ~ '^[0-9]{6,20}$'
  AND NOT EXISTS (
      SELECT 1 FROM contacts x WHERE x.org_id = b.org_id AND x.phone_normalized = c.contact
  )
ON CONFLICT (org_id, phone_normalized) DO NOTHING;

UPDATE chats c
SET contact_id = ct.id
FROM bots b, contacts ct
WHERE b.id = c.bot_id
  AND ct.org_id = b.org_id
  AND ct.phone_normalized = c.contact
  AND c.contact_id IS NULL;

-- Un chat que no se pudo enlazar no tiene identidad recuperable: su `contact`
-- no era un teléfono. Es exactamente la basura que producía el webhook al
-- recibir un mensaje sin `from`, y no se conserva.
DELETE FROM chats WHERE contact_id IS NULL;

ALTER TABLE chats ALTER COLUMN contact_id SET NOT NULL;

-- 3. Fuera las columnas viejas: el teléfono y el nombre viven en contacts, y
-- dos copias del mismo dato acaban discrepando.
ALTER TABLE chats DROP CONSTRAINT IF EXISTS chats_bot_id_contact_key;
ALTER TABLE chats DROP COLUMN IF EXISTS contact;
ALTER TABLE chats DROP COLUMN IF EXISTS contact_name;
ALTER TABLE chats ADD CONSTRAINT chats_bot_id_contact_id_key UNIQUE (bot_id, contact_id);
