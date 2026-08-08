-- Flujos restringidos por audiencia.
-- Espejo de db/schema.sql; PLAN-FLUJOS-POR-AUDIENCIA-Y-PERMISOS.md §1 y §7.
--
-- Un flujo con audiencia solo atiende a los contactos que cumplen una condición
-- sobre una tabla de datos de la organización. La condición es la MISMA
-- configuración que ya usa el bloque «Leer una tabla» (`data_query`): `object`,
-- `where.<n>.field/op/value`, `linkCurrentContact`. No se inventa un lenguaje de
-- filtros nuevo, y por eso `engine.ValidateAudience` puede apoyarse en el
-- validador que ya existe.
--
-- Va en `flows`, la fila mutable, y NO en `flow_versions`: la audiencia es
-- metadato operativo, de la misma familia que `priority` e `is_fallback`, que ya
-- viven aquí y ya surten efecto sin republicar. Es *quién entra al grafo*, no
-- *qué hace el grafo*. Meterla en la versión inmutable obligaría a republicar
-- —revalidando y creando versión— para ampliar un piloto en un contacto, que es
-- justo la operación que este cambio quiere hacer barata.
--
-- NULL es «sin restricción», y no `'{}'`: un objeto vacío sería un predicado sin
-- condiciones, que es una cosa distinta y ambigua. El CHECK admite NULL y objeto
-- no vacío, nunca el objeto vacío.
ALTER TABLE flows ADD COLUMN IF NOT EXISTS audience JSONB;

ALTER TABLE flows DROP CONSTRAINT IF EXISTS flows_audience_object_check;
ALTER TABLE flows ADD CONSTRAINT flows_audience_object_check
    CHECK (audience IS NULL OR (jsonb_typeof(audience) = 'object' AND audience <> '{}'::jsonb));

-- El fallback es el que atiende cuando ningún trigger reconoce el mensaje.
-- Restringirlo por audiencia dejaría al resto de contactos sin nada que los
-- atienda: el bot enmudecería para todos los de fuera sin un solo error. Se
-- rechaza en la base y no solo en el controlador porque es un invariante del
-- despacho, no una regla de la interfaz.
ALTER TABLE flows DROP CONSTRAINT IF EXISTS flows_audience_not_fallback_check;
ALTER TABLE flows ADD CONSTRAINT flows_audience_not_fallback_check
    CHECK (audience IS NULL OR NOT is_fallback);

-- Un flujo `schedule` resuelve su destinatario por `data_view`, no por el
-- contacto de un mensaje entrante: la audiencia no encaja en ese camino tal cual
-- y admitirla en silencio la dejaría sin efecto. Se rechaza hasta que exista una
-- decisión propia (PLAN §3 y §9).
ALTER TABLE flows DROP CONSTRAINT IF EXISTS flows_audience_only_message_check;
ALTER TABLE flows ADD CONSTRAINT flows_audience_only_message_check
    CHECK (audience IS NULL OR trigger_type = 'message');

-- `audiences` y `audience_contacts` se retiran.
--
-- Nunca tuvieron consumidor: fuera de su propio modelo y controlador, las únicas
-- referencias del backend eran sus cuatro rutas HTTP, un comentario y la lista de
-- cmd/migrate_org_data. El scheduler resuelve destinatarios por `data_view`, no
-- por audiencia, y no llegó a existir UI que las llenara. Comprobado vacías
-- (0 filas en ambas) antes de soltarlas.
--
-- Una audiencia es una consulta sobre los datos de la organización, y esa
-- consulta ya está construida y validada. Una tabla dedicada a representar «un
-- conjunto de contactos», cuando el producto ya sabe representar conjuntos de
-- cualquier cosa con su panel y su aislamiento por organización, era el mismo
-- error que `is_test` un nivel más arriba.
DROP TABLE IF EXISTS audience_contacts;
DROP TABLE IF EXISTS audiences;
