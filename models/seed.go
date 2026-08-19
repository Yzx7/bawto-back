package models

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Yzx7/sacs-chatbots/db/defaults"
)

// Semilla de creación (PLAN-SEMILLA-DE-ORGANIZACION-Y-BOT.md). Una organización
// nace con sus tablas; un bot nace con su flujo. Se siembran **filas reales**, no
// una resolución por defecto en ejecución: la única fuente de verdad de qué
// campos tiene una tabla sigue siendo `data_fields`, y lo sembrado es del dueño
// desde el primer segundo — puede renombrarlo o borrarlo, y si lo borra no vuelve.
//
// Las dos siembras corren dentro de la transacción de creación, así que no hay
// estado a medias que reconciliar: o está completo, o no está la organización.

// seedQueryer es lo que la semilla necesita para decidir: consultar. Lo cumplen
// tanto el pool como una transacción, y por eso los hechos de §4 se pueden leer
// antes de abrir la transacción que crea el bot.
type seedQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// seedObjects es la fuente de las tablas por defecto. Es una variable y no una
// llamada directa por un único motivo: la propiedad que importa de la Fase 1 es
// que **un fallo de la semilla deja la organización sin crear**, y sin poder
// hacer fallar la semilla a propósito esa prueba no se puede escribir. El
// producto nunca la sustituye.
var seedObjects = defaults.Objetos

// seedOrganizationTx inserta las tablas por defecto de una organización recién
// creada. Va dentro de la transacción de CreateOrganization: si la semilla falla,
// la organización no llega a existir.
func seedOrganizationTx(ctx context.Context, tx pgx.Tx, orgID string) error {
	objects, err := seedObjects()
	if err != nil {
		return err
	}
	for _, object := range objects {
		var objectID string
		if err := tx.QueryRow(ctx,
			`INSERT INTO data_objects (org_id, key, name, plural_name)
			 VALUES ($1::uuid, $2, $3, $4) RETURNING id::text`,
			orgID, object.Key, object.Name, object.PluralName).Scan(&objectID); err != nil {
			return fmt.Errorf("semilla de la tabla %q: %w", object.Key, err)
		}
		for _, field := range object.Fields {
			// Se valida lo sembrado con la misma función que valida lo que crea el
			// panel: si la semilla y el alta manual pudieran producir cosas
			// distintas, ya serían dos definiciones de qué es un campo.
			if err := ValidateDataFieldDefinition(field.Key, field.Label, field.Type); err != nil {
				return fmt.Errorf("semilla de la tabla %q, campo %q: %w", object.Key, field.Key, err)
			}
			// clock_timestamp() y no el NOW() de la columna: NOW() es la marca de
			// **la transacción**, así que los campos de una misma tabla nacerían
			// todos con el mismo created_at y el ORDER BY created_at del panel los
			// mostraría en un orden arbitrario. El orden del fichero es información
			// del autor de la semilla.
			if _, err := tx.Exec(ctx,
				`INSERT INTO data_fields (object_id, key, label, type, required, created_at, updated_at)
				 VALUES ($1::uuid, $2, $3, $4, $5, clock_timestamp(), clock_timestamp())`,
				objectID, field.Key, field.Label, field.Type, field.Required); err != nil {
				return fmt.Errorf("semilla de la tabla %q, campo %q: %w", object.Key, field.Key, err)
			}
		}
	}
	return nil
}

// seedFlowDraft decide y devuelve el grafo del primer flujo del bot.
//
// La decisión son **dos hechos que se consultan a la base**, no parámetros de la
// petición (§4 del plan): que la fila de `negocio` diga que quiere vender, y que
// exista una conexión `meudim` activa. Que sean hechos y no argumentos es lo que
// hace que funcione igual si el dueño conectó la tienda por su cuenta, sin pasar
// por el cuestionario, y si crea su segundo bot un mes después.
//
// Las dos condiciones o núcleo pelado: querer vender sin tienda conectada no
// permite dibujar la rama comercial, porque `engine.Validate` rechaza una
// `connection` vacía y el flujo ni siquiera se podría publicar.
func seedFlowDraft(ctx context.Context, q seedQueryer, orgID string) (json.RawMessage, error) {
	nucleo, err := defaults.FlujoInicial()
	if err != nil {
		return nil, err
	}
	vende, err := orgQuiereVender(ctx, q, orgID)
	if err != nil {
		return nil, err
	}
	if !vende {
		return nucleo, nil
	}
	conectada, err := tiendaMeudimActiva(ctx, q, orgID)
	if err != nil {
		return nil, err
	}
	if !conectada {
		return nucleo, nil
	}
	return defaults.Injertar(nucleo)
}

// orgQuiereVender lee lo que el cuestionario escribió en `negocio.automatiza`.
// Una organización sin esa fila —el cuestionario se puede abandonar en cualquier
// paso— simplemente no vende: el bot nace conversacional y sano.
func orgQuiereVender(ctx context.Context, q seedQueryer, orgID string) (bool, error) {
	automatiza, err := seedScalar(ctx, q,
		`SELECT COALESCE(r.data->>'automatiza', '')
		 FROM data_records r
		 JOIN data_objects o ON o.id = r.object_id
		 WHERE o.org_id = $1::uuid AND o.key = 'negocio'
		 ORDER BY r.created_at DESC
		 LIMIT 1`, orgID)
	if err != nil {
		return false, err
	}
	return automatiza == "vender", nil
}

// tiendaMeudimActiva responde si la organización tiene conectada su tienda. La
// conexión se nombra por clave y no por id —la identidad es (org_id, key)—, que
// es lo que permite que el fragmento comercial sea JSON fijo del repositorio.
func tiendaMeudimActiva(ctx context.Context, q seedQueryer, orgID string) (bool, error) {
	status, err := seedScalar(ctx, q,
		`SELECT status FROM external_connections
		 WHERE org_id = $1::uuid AND key = 'meudim'`, orgID)
	if err != nil {
		return false, err
	}
	// Se reutiliza el criterio de la propia conexión: si «activa» pasara a
	// significar otra cosa, la semilla no debe ser el sitio donde se olvide.
	return ExternalConnection{Status: status}.Active(), nil
}

// seedScalar devuelve el primer texto de la consulta, o "" si no hay filas. Que
// no haya fila no es un fallo: el cuestionario se puede abandonar y la conexión
// puede no existir, y las dos ausencias significan «núcleo pelado».
func seedScalar(ctx context.Context, q seedQueryer, sql string, args ...any) (string, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	value := ""
	for rows.Next() {
		if err := rows.Scan(&value); err != nil {
			return "", err
		}
	}
	return value, rows.Err()
}

// seedBotFlowTx inserta el flujo inicial del bot en la misma transacción que lo
// creó.
//
// Clave `principal`, disparador `message`/`any` e `is_fallback` —ocupa el hueco
// del índice único parcial uq_flows_bot_fallback, que solo admite uno vivo por
// bot— y **queda en borrador**: publicar es humano, y además un bot recién creado
// no tiene canal conectado, así que no le puede llegar un mensaje.
func seedBotFlowTx(ctx context.Context, tx pgx.Tx, botID string, draft json.RawMessage) error {
	if len(draft) == 0 {
		return nil
	}
	_, err := createFlowTx(ctx, tx, botID, NewFlow{
		Key:         "principal",
		Name:        "Atención principal",
		TriggerType: "message",
		IsFallback:  true,
		Draft:       draft,
	})
	return err
}
