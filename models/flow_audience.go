package models

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Yzx7/sacs-chatbots/engine"
)

// Pertenencia a la audiencia de un flujo
// (PLAN-FLUJOS-POR-AUDIENCIA-Y-PERMISOS §2.2).
//
// La validación de la condición vive en `engine`, que es donde está el validador
// de `data_query`; la ejecución vive aquí, que es el lado que puede hablar con
// Postgres. Es el mismo reparto que ya impone que `models` importe `engine` y no
// al revés.

var (
	// ErrFlowAudienceOnMessage: solo un flujo de conversación puede restringirse.
	// Un `schedule` resuelve destinatario por `data_view`, no por el contacto de
	// un mensaje: aceptar la audiencia ahí la dejaría sin efecto en silencio.
	ErrFlowAudienceOnMessage = errors.New("solo un flujo de conversación puede tener audiencia")
	// ErrFlowAudienceFallback: el fallback atiende cuando ningún trigger reconoce
	// el mensaje. Restringirlo deja sin nada a todos los contactos de fuera.
	ErrFlowAudienceFallback = errors.New("el flujo de reserva no puede tener audiencia: dejaría sin atender a todos los demás contactos")
)

// AudienceVerdict es el resultado de evaluar la condición para un contacto.
//
// `Reason` no es decorativo: sin él, un flujo restringido que no responde se
// depura a ciegas. Distingue "no cumple la condición" —que es funcionamiento
// normal— de "no se pudo evaluar" —que es una avería que alguien debe mirar—,
// aunque las dos terminen igual para el contacto.
type AudienceVerdict struct {
	Serves bool
	Reason string
}

// audienceServes es el ÚNICO punto del que sale un "sí, atiende".
//
// Todo lo demás —cero coincidencias, error de SQL, objeto inexistente, JSON
// corrupto en la columna— cae en "no atiende". La asimetría es deliberada y es
// lo más importante de este fichero: `data_query` trata cero coincidencias como
// `ok` con `found=false` a propósito, para que un Router distinga "no tiene
// perfil" de "la lectura falló". Esa distinción NO debe propagarse aquí. Un
// `error` traducido a "no hay restricción que aplicar" convertiría una caída de
// la base en un flujo piloto atendiendo a la organización entera, que es
// exactamente el modo de fallo caro que este diseño existe para evitar.
var audienceServes = AudienceVerdict{Serves: true, Reason: "cumple la condición"}

// ContactMatchesAudience decide si el contacto entra en la audiencia del flujo.
//
// `raw` es el contenido de `flows.audience`: vacío significa sin restricción, y
// entonces atiende a todos, que es el comportamiento de siempre.
func ContactMatchesAudience(ctx context.Context, p *pgxpool.Pool, orgID, phone string, raw json.RawMessage) AudienceVerdict {
	condition, err := engine.ParseAudience(raw)
	if err != nil {
		// Una condición guardada que ya no valida es una avería, no un "no
		// cumple": alguien escribió en la columna por fuera del endpoint, o el
		// validador se endureció después. En cualquier caso, no atiende.
		return AudienceVerdict{Serves: false, Reason: "condición inválida: " + err.Error()}
	}
	if condition == nil {
		return AudienceVerdict{Serves: true, Reason: "sin audiencia"}
	}
	// Sin teléfono no hay contacto con el que comparar, y `linkCurrentContact`
	// —obligatorio en toda condición— no tendría a quién atarse.
	if strings.TrimSpace(phone) == "" {
		return AudienceVerdict{Serves: false, Reason: "el mensaje no trae contacto que comprobar"}
	}

	// Misma lectura que usa la vista previa: si una de las dos interpretara las
	// reglas de otra forma, el panel enseñaría un conjunto y el bot atendería otro.
	rules := audienceRules(condition)

	// `contacts` no es un `data_object`: es la tabla especial del producto, con
	// sus columnas propias y su JSONB de campos personalizados. Y es la más
	// natural para una audiencia —«los que tienen `piloto = si`»—, así que se
	// resuelve contra ella en vez de obligar a crear una tabla paralela y
	// vincular fila por fila lo que el campo de contacto ya dice.
	if condition["object"] == engine.AudienceContactsObject {
		matches, err := contactMatches(ctx, p, orgID, phone, rules)
		if err != nil {
			return AudienceVerdict{Serves: false, Reason: "no se pudo evaluar la audiencia: " + err.Error()}
		}
		if !matches {
			return AudienceVerdict{Serves: false, Reason: "no cumple la condición"}
		}
		return audienceServes
	}

	input := DataQueryInput{
		OrgID:              orgID,
		ObjectKey:          condition["object"],
		LinkedContactPhone: phone,
		Where:              rules,
		// El predicado solo responde sí o no: basta con saber si existe una fila.
		Limit: 1,
	}

	result, err := QueryDataRecords(ctx, p, input)
	if err != nil {
		return AudienceVerdict{Serves: false, Reason: "no se pudo evaluar la audiencia: " + err.Error()}
	}
	if result == nil || !result.Found {
		return AudienceVerdict{Serves: false, Reason: "no cumple la condición"}
	}
	return audienceServes
}

// SetFlowAudience asigna o retira la condición de audiencia.
//
// Es un endpoint propio y no un campo del PATCH de metadatos por una razón de
// seguridad, no de estética: si viajara junto al nombre, un `member` —que sí
// puede renombrar— podría quitarse la restricción y publicar después sin
// audiencia, saltándose el permiso que este diseño impone.
//
// `raw` vacío o nulo retira la restricción.
func SetFlowAudience(ctx context.Context, p *pgxpool.Pool, botID, flowID string, raw json.RawMessage, userID string) (*Flow, error) {
	condition, err := engine.ParseAudience(raw)
	if err != nil {
		return nil, err
	}
	var stored any
	if condition != nil {
		encoded, err := json.Marshal(condition)
		if err != nil {
			return nil, err
		}
		stored = encoded
	}

	rows, err := p.Query(ctx,
		`UPDATE flows SET audience = $3::jsonb, updated_by = COALESCE(NULLIF($4,''), updated_by)
		 WHERE id = $1::uuid AND bot_id = $2::uuid AND archived_at IS NULL
		 RETURNING `+flowCols, flowID, botID, stored, userID)
	if err != nil {
		return nil, translateAudienceConflict(err)
	}
	f, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[Flow])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, translateAudienceConflict(err)
	}
	return &f, nil
}

// ResetChatsOfFlow corta las conversaciones que quedaron selladas a este flujo,
// devolviéndolas al despacho normal en su próximo mensaje.
//
// Existe por la consecuencia deliberada de la regla 1 del dispatcher: una
// conversación a medias sigue en SU flujo aunque el contacto haya entrado o
// salido de la audiencia. Comprobar la audiencia también al reanudar arreglaría
// la mitad del problema y rompería algo peor —perder el nodo y las variables de
// quien está a mitad de un `wait`—, así que la salida es esta: un botón que el
// operador pulsa a sabiendas, no una expulsión automática que nadie pidió.
//
// Se aplica **por flujo** y no "por audiencia" a propósito, y así sirve para los
// dos sentidos: quien salió de la audiencia se corta desde el flujo restringido;
// quien entró y sigue atrapado en el general, desde el general. Una acción por
// audiencia solo habría resuelto el primero.
//
// Devuelve cuántas conversaciones se cortaron: sin ese número el operador no
// sabe si la acción hizo algo o si no había nadie a quien cortar.
func ResetChatsOfFlow(ctx context.Context, p *pgxpool.Pool, botID, flowID string) (int64, error) {
	cmd, err := p.Exec(ctx,
		`UPDATE chats SET current_layer = 'null'::jsonb, updated_at = NOW()
		 WHERE bot_id = $1::uuid
		   AND current_layer ->> 'flowId' = $2`, botID, flowID)
	if err != nil {
		return 0, err
	}
	return cmd.RowsAffected(), nil
}

// translateAudienceConflict convierte los CHECK de la migración 019 en errores
// de dominio. Los invariantes viven en la base porque condicionan el despacho,
// no la interfaz; el controlador necesita poder responder 400 y no un 500 opaco.
func translateAudienceConflict(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.ConstraintName {
	case "flows_audience_not_fallback_check":
		return ErrFlowAudienceFallback
	case "flows_audience_only_message_check":
		return ErrFlowAudienceOnMessage
	}
	return err
}
