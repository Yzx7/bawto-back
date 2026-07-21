-- Convierte identidades ya observadas en chats en contactos de la organización.
INSERT INTO contacts(org_id,phone_normalized,name)
SELECT b.org_id, regexp_replace(ch.contact,'[^0-9]','','g'), NULLIF(ch.contact_name,'')
FROM chats ch
JOIN bots b ON b.id=ch.bot_id
WHERE regexp_replace(ch.contact,'[^0-9]','','g') ~ '^[0-9]{6,20}$'
ON CONFLICT(org_id,phone_normalized) DO UPDATE
SET name=COALESCE(NULLIF(EXCLUDED.name,''),contacts.name),updated_at=NOW();
