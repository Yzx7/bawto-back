-- Qué recurso creó Bawto en el sistema externo, cuando lo creó Bawto.
--
-- Hasta ahora una conexión solo sabía a quién llamar. Desde que el asistente
-- puede pedirle a MEUD que abra la tienda (GUIA-CONEXION-MEUD.md), hacen falta
-- dos hechos que la fila no tenía:
--
--   1. El id de la tienda. La API pública se resuelve sola desde la `sk_`, pero
--      el aprovisionamiento trabaja por encima de las tiendas y sus rutas lo
--      llevan en la URL: `POST /internal/provision/stores/<id>/admins`.
--   2. Que la creamos nosotros. La credencial de aprovisionamiento vale para
--      **todas** las tiendas de todos los clientes, así que sin este dato una
--      organización podría pegar la `sk_` de una tienda ajena y pedirnos que
--      añadiera a alguien como administrador: convertiríamos «tiene una clave»
--      en «tiene una cuenta en el panel de esa tienda».
--
-- NULL significa que la credencial la pegó una persona, que es el caso de todas
-- las conexiones existentes. Por eso la columna es nullable y no tiene default:
-- inventar un id para las filas de antes sería exactamente la afirmación falsa
-- que esta columna existe para evitar.
ALTER TABLE external_connections ADD COLUMN IF NOT EXISTS provisioned_id TEXT;

COMMENT ON COLUMN external_connections.provisioned_id IS
    'Id del recurso que Bawto creó en el sistema externo; NULL si la credencial la pegó una persona';
