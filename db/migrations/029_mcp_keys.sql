-- Keys del servicio MCP de autoría (POST /mcp/flows), una fila por credencial.
--
-- Sustituye a los tokens HMAC autofirmados con una clave global. Aquella firma
-- no se podía revocar: el token valía hasta caducar, y quitarle acceso a alguien
-- obligaba a rotar la clave del servidor, o sea a invalidar los de todo el
-- mundo. Con una fila por key, revocar es un UPDATE y se nota en la petición
-- siguiente, porque el servicio la resuelve contra esta tabla cada vez.
--
-- La key **nunca se guarda en claro**. Se guarda su SHA-256 y un prefijo
-- visible para poder distinguirlas en el panel. SHA-256 y no bcrypt/argon2 a
-- propósito: esto no es una contraseña humana, es un secreto de 256 bits que
-- genera el servidor, así que no hay diccionario ni fuerza bruta que atacar y un
-- KDF lento solo añadiría su coste a **cada** llamada del MCP. Lo que protege
-- aquí es la entropía del secreto, no la lentitud del hash.
CREATE TABLE IF NOT EXISTS mcp_keys (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      UUID        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    -- Nombre que le pone la persona («Cursor de Angelo»). Solo sirve para
    -- reconocerla en la lista y decidir cuál revocar.
    name        TEXT        NOT NULL,
    -- Primeros caracteres de la key, en claro. Es lo único que el panel puede
    -- volver a mostrar: sin esto, una lista de keys son filas indistinguibles y
    -- revocar «la correcta» se vuelve adivinar.
    key_prefix  TEXT        NOT NULL,
    key_hash    TEXT        NOT NULL,
    -- Alcance: los flujos que esta key puede leer y escribir. Vacío = todos los
    -- de la organización. Es un array y no una tabla puente porque la lista se
    -- fija al crear la key y no se edita nunca: ampliar el alcance de una
    -- credencial ya repartida es exactamente lo que no debe poder hacerse, así
    -- que se revoca y se emite otra. Un id de un flujo borrado simplemente deja
    -- de casar; no hace falta limpiarlo.
    flow_ids    UUID[]      NOT NULL DEFAULT '{}',
    created_by  TEXT        REFERENCES "user"(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Caducidad opcional. Una key sin fecha vale hasta que alguien la revoque.
    expires_at  TIMESTAMPTZ,
    -- Observabilidad para decidir cuáles sobran. No se actualiza en cada
    -- petición: el servicio solo la refresca si lleva rato sin tocarse, para no
    -- convertir cada lectura del MCP en una escritura.
    last_used_at TIMESTAMPTZ,
    revoked_at  TIMESTAMPTZ,
    revoked_by  TEXT        REFERENCES "user"(id) ON DELETE SET NULL,
    CHECK (length(trim(name)) > 0),
    CHECK (key_hash ~ '^[0-9a-f]{64}$'),
    CHECK (length(key_prefix) BETWEEN 6 AND 32)
);

-- La resolución de cada petición entra por aquí: un único índice único sobre el
-- hash. Es también lo que impide que dos keys distintas colisionen.
CREATE UNIQUE INDEX IF NOT EXISTS uq_mcp_keys_hash ON mcp_keys (key_hash);

-- El listado del panel es siempre por organización y muestra primero las vivas.
CREATE INDEX IF NOT EXISTS idx_mcp_keys_org ON mcp_keys (org_id, created_at DESC);
