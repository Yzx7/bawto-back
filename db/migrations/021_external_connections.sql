-- Conexiones a sistemas externos, una fila por organización y clave técnica.
--
-- Es una tabla y no las cuatro que preveía el diseño de sincronización: al
-- consultar la API en directo no hay copia local que reconciliar, así que no
-- existen destinos, procedencias de registro ni historial de ejecuciones. Lo
-- único que hay que guardar es a quién se llama, con qué credencial y si la
-- última llamada funcionó.
--
-- `credential` va cifrada con TOKEN_ENC_KEY (AES-256-GCM, helpers.Cipher),
-- igual que el token del canal. La clave publicable de una tienda es pública
-- por diseño, pero se guarda cifrada de todos modos: así rotarla, auditarla y
-- restringir quién la ve no depende del tipo de clave que hoy toque usar.
CREATE TABLE IF NOT EXISTS external_connections (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id      UUID        NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    -- Clave técnica que nombra el flujo. El grafo referencia esto, nunca el id:
    -- así el mismo flujo exportado vale en otra organización que tenga su propia
    -- conexión "meudim" apuntando a su propia tienda.
    key         TEXT        NOT NULL,
    -- Lista permitida en código (connectors.ValidateTarget). La columna no lleva
    -- CHECK con los nombres: añadir un driver no debe exigir una migración, y el
    -- rechazo tiene que ocurrir antes de abrir la conexión, no en el INSERT.
    driver      TEXT        NOT NULL,
    label       TEXT        NOT NULL,
    base_url    TEXT        NOT NULL,
    credential  BYTEA       NOT NULL,
    status      TEXT        NOT NULL DEFAULT 'active',
    -- Observabilidad, no caché: dicen si la última llamada funcionó. Aquí no se
    -- guarda ni un producto ni un precio.
    last_ok_at  TIMESTAMPTZ,
    last_error  TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (org_id, key),
    CHECK (key ~ '^[a-z][a-z0-9_]{0,63}$'),
    CHECK (length(trim(label)) > 0),
    CHECK (length(credential) > 0),
    CHECK (status IN ('active', 'disabled'))
);

CREATE INDEX IF NOT EXISTS idx_external_connections_org
    ON external_connections (org_id);

DROP TRIGGER IF EXISTS trg_external_connections_updated_at ON external_connections;
CREATE TRIGGER trg_external_connections_updated_at
    BEFORE UPDATE ON external_connections
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
