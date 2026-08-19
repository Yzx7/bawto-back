package models

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
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
		OrgID:     orgID,
		ObjectKey: condition["object"],
		// El predicado pregunta por las filas **de este contacto**. Sin decirlo
		// explícitamente, un contacto sin teléfono evaluaría la condición contra la
		// tabla entera y quedaría dentro de la audiencia porque cumple otra
		// persona.
		LinkCurrentContact: true,
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
// Devuelve cuántas conversaciones se cortaron **y dónde están las que quedan**.
//
// Lo segundo no es un extra: un «no había nada que cortar» a secas es cierto y
// completamente inútil. Quien acaba de restringir un flujo y no ve el cambio
// tiene sus conversaciones selladas a OTRO flujo —normalmente el de reserva—, y
// sin decirle a cuál se queda mirando el botón que acaba de pulsar sin efecto.
func ResetChatsOfFlow(ctx context.Context, p *pgxpool.Pool, botID, flowID, orgID string, audience json.RawMessage) (int64, []ActiveChatsByFlow, error) {
	sql, args, err := resetChatsStatement(ctx, p, botID, flowID, orgID, audience)
	if err != nil {
		return 0, nil, err
	}
	cmd, err := p.Exec(ctx, sql, args...)
	if err != nil {
		return 0, nil, err
	}
	remaining, err := ActiveChats(ctx, p, botID)
	if err != nil {
		// El corte sí ocurrió; no poder contar el resto no lo invalida.
		return cmd.RowsAffected(), nil, nil
	}
	return cmd.RowsAffected(), remaining, nil
}

// resetChatsStatement decide **a quién** se corta.
//
// Con audiencia, la acción es quirúrgica: toca solo las conversaciones cuyo
// sellado contradice la condición, en los dos sentidos.
//
//	entró en la audiencia  → está sellado a OTRO flujo  → se corta
//	salió de la audiencia  → está sellado a ESTE flujo  → se corta
//	todo lo demás                                       → no se toca
//
// La primera línea es el caso que motiva la acción: restringir un flujo a un
// contacto no surte efecto mientras su conversación siga viva en el flujo
// anterior. Cortar «las de este flujo» no lo arreglaba —esas son cero— y cortar
// las del flujo de reserva arrastraba a **toda** la organización para mover a una
// persona. Ninguna de las dos es lo que el operador quiere.
//
// El conjunto de contactos sale de `audiencePreviewQuery`, la MISMA consulta que
// alimenta la tabla del formulario. Así lo que se corta es exactamente lo que la
// vista previa enseñó: no hay forma de que el botón afecte a alguien que no
// estaba en la lista.
//
// Sin audiencia el flujo atiende a todos, no hay condición que aplicar, y la
// única lectura posible es la literal: cortar las conversaciones selladas a él.
func resetChatsStatement(ctx context.Context, p *pgxpool.Pool, botID, flowID, orgID string, audience json.RawMessage) (string, []any, error) {
	condition, err := engine.ParseAudience(audience)
	if err != nil {
		return "", nil, err
	}
	if condition == nil {
		return `UPDATE chats SET current_layer = 'null'::jsonb, updated_at = NOW()
			WHERE bot_id = $1::uuid AND current_layer ->> 'flowId' = $2`,
			[]any{botID, flowID}, nil
	}

	base, args, err := audiencePreviewQuery(ctx, p, orgID, condition)
	if err != nil {
		return "", nil, err
	}
	// Los marcadores de `base` ya ocupan $1..$n; el bot y el flujo van detrás.
	args = append(args, botID, flowID)
	bot := "$" + strconv.Itoa(len(args)-1)
	flow := "$" + strconv.Itoa(len(args))

	// El chat apunta al contacto, así que la pertenencia se compara por id y no
	// por teléfono: desde los nombres de usuario de WhatsApp hay contactos sin
	// teléfono, y comparar cadenas los dejaba a todos fuera de toda audiencia.
	enLaAudiencia := `chats.contact_id IN (SELECT c.id ` + base + `)`

	return `UPDATE chats SET current_layer = 'null'::jsonb, updated_at = NOW()
		WHERE chats.bot_id = ` + bot + `::uuid
		  AND chats.current_layer ->> 'flowId' IS NOT NULL
		  AND (
		        (` + enLaAudiencia + ` AND chats.current_layer ->> 'flowId' <> ` + flow + `)
		     OR (NOT ` + enLaAudiencia + ` AND chats.current_layer ->> 'flowId' = ` + flow + `)
		  )`, args, nil
}

// ActiveChatsByFlow es el recuento de conversaciones vivas selladas a un flujo.
type ActiveChatsByFlow struct {
	FlowID string `db:"flow_id" json:"flowId"`
	Key    string `db:"key" json:"key"`
	Name   string `db:"name" json:"name"`
	Count  int    `db:"count" json:"count"`
}

// ActiveChats agrupa por flujo las conversaciones que siguen selladas.
//
// Un flujo borrado o archivado puede seguir apareciendo con su id y sin nombre:
// el estado guarda el `flowId`, no una FK, y esconder esas filas dejaría
// conversaciones invisibles que nadie podría cortar.
func ActiveChats(ctx context.Context, p *pgxpool.Pool, botID string) ([]ActiveChatsByFlow, error) {
	rows, err := p.Query(ctx,
		`SELECT c.current_layer ->> 'flowId' AS flow_id,
		        COALESCE(f.key, '(flujo desconocido)') AS key,
		        COALESCE(f.name, '(ya no existe)') AS name,
		        count(*)::int AS count
		 FROM chats c
		 LEFT JOIN flows f ON f.id::text = c.current_layer ->> 'flowId'
		 WHERE c.bot_id = $1::uuid AND c.current_layer ->> 'flowId' IS NOT NULL
		 GROUP BY 1, 2, 3
		 ORDER BY count(*) DESC, 2`, botID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[ActiveChatsByFlow])
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
