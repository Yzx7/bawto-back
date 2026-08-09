package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/Yzx7/sacs-chatbots/engine"
)

func main() {
	_ = godotenv.Load()
	botID := flag.String("bot-id", "", "bot")
	flowID := flag.String("flow-id", "", "flow")
	baseFile := flag.String("base-file", "", "exportación del borrador que se fusionará")
	baseChecksum := flag.String("base-checksum", "", "checksum de la exportación base")
	expected := flag.String("expected-checksum", "", "checksum del borrador actual en PostgreSQL")
	flag.Parse()
	if *botID == "" || *flowID == "" || *baseFile == "" || *baseChecksum == "" || *expected == "" {
		panic("faltan flags")
	}

	baseRaw, err := os.ReadFile(*baseFile)
	must(err)
	_, actualBaseChecksum, err := engine.CanonicalChecksum(baseRaw)
	must(err)
	if actualBaseChecksum != *baseChecksum {
		panic(fmt.Sprintf("la exportación base cambió: esperado %s, actual %s", *baseChecksum, actualBaseChecksum))
	}
	var document map[string]any
	must(json.Unmarshal(baseRaw, &document))
	updateDocument(document)
	updated, err := json.Marshal(document)
	must(err)
	var validationFlow engine.Flow
	must(json.Unmarshal(updated, &validationFlow))
	must(engine.Validate(&validationFlow))
	_, nextChecksum, err := engine.CanonicalChecksum(updated)
	must(err)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	must(err)
	defer pool.Close()
	tx, err := pool.Begin(ctx)
	must(err)
	defer tx.Rollback(ctx)
	var currentRaw json.RawMessage
	must(tx.QueryRow(ctx, `SELECT draft FROM flows WHERE id=$1::uuid AND bot_id=$2::uuid FOR UPDATE`, *flowID, *botID).Scan(&currentRaw))
	_, currentChecksum, err := engine.CanonicalChecksum(currentRaw)
	must(err)
	if currentChecksum != *expected {
		panic(fmt.Sprintf("checksum cambió: esperado %s, actual %s", *expected, currentChecksum))
	}
	tag, err := tx.Exec(ctx, `UPDATE flows SET draft=$3::jsonb,updated_by='codex:subscriptions'
		WHERE id=$1::uuid AND bot_id=$2::uuid`, *flowID, *botID, updated)
	must(err)
	if tag.RowsAffected() != 1 {
		panic("no se actualizó el borrador")
	}
	must(tx.Commit(ctx))
	fmt.Printf("draft actualizado checksum=%s\n", nextChecksum)
}

// El documento se modifica como JSON genérico porque `pos` pertenece al editor,
// no al motor. Convertirlo a engine.Flow y volverlo a serializar eliminaría ese
// campo y cualquier extensión futura que el backend todavía no conozca.
func updateDocument(document map[string]any) {
	nodes := objectSlice(document, "nodes")
	agent := node(nodes, "n_agente")
	agent["saveAs"] = "sale"
	agent["tools"] = []any{map[string]any{
		"ref":    "data_query",
		"config": map[string]any{"objects": "servicios,planes_bawto", "maxLimit": "8"},
	}}
	agent["outputFields"] = []any{
		map[string]any{"key": "organizationCode", "type": "string", "description": "Código de activación de 10 caracteres; vacío hasta que el cliente lo confirme."},
		map[string]any{"key": "planKey", "type": "string", "description": "Exactamente inicio, base, crece o pro; vacío hasta confirmar el plan."},
		map[string]any{"key": "billingCycle", "type": "string", "description": "Exactamente monthly o quarterly; vacío hasta confirmar el ciclo."},
	}
	agent["instruction"] = salesInstruction()

	save := node(nodes, "n_save_payment")
	object(save, "args")["field.estado"] = "aceptado"
	node(nodes, "n_payment_needs_review")["expression"] = "!empty(sale.organizationCode) && !empty(sale.planKey) && !empty(sale.billingCycle)"
	node(nodes, "n_payment_ok")["body"] = "Pago recepcionado y acceso activado. Tu organización ya tiene el plan {sale.planKey}; la vigencia puede verse en Organización dentro del panel."
	node(nodes, "n_payment_review")["body"] = "Pago recepcionado y registrado. Como este comprobante no venía de una compra iniciada aquí, conservé la evidencia sin cambiar el plan de ninguna organización."

	nodes = append(nodes, map[string]any{
		"id": "n_activate_subscription", "kind": "tool", "toolRef": "subscription_activate",
		"saveAs": "subscription", "pos": map[string]any{"x": 2005.0, "y": -208.0},
		"args": map[string]any{
			"activationCode": "{sale.organizationCode}", "planKey": "{sale.planKey}",
			"billingCycle": "{sale.billingCycle}", "paymentRecordId": "{payment_record.recordId}",
			"phone": "{contact_phone}", "idempotencyKey": "subscription:{payment_record.recordId}",
		},
	})
	document["nodes"] = nodes

	edges := objectSlice(document, "edges")
	for _, edge := range edges {
		switch textValue(edge["id"]) {
		case "e_payment_review":
			edge["target"] = "n_activate_subscription"
		case "e_payment_confirmed":
			edge["target"] = "n_payment_review"
		case "e_payment_review_handoff":
			edge["target"] = "n_fin"
		}
	}
	edges = append(edges,
		map[string]any{"id": "e_subscription_activated", "source": "n_activate_subscription", "target": "n_payment_ok", "sourceHandle": "ok"},
		map[string]any{"id": "e_subscription_error", "source": "n_activate_subscription", "target": "n_payment_error", "sourceHandle": "error"},
	)
	document["edges"] = edges
}

func objectSlice(document map[string]any, key string) []map[string]any {
	raw, ok := document[key].([]any)
	if !ok {
		panic(key + " inválido")
	}
	items := make([]map[string]any, 0, len(raw))
	for _, value := range raw {
		item, ok := value.(map[string]any)
		if !ok {
			panic(key + " contiene un elemento inválido")
		}
		items = append(items, item)
	}
	return items
}

func node(nodes []map[string]any, id string) map[string]any {
	for _, item := range nodes {
		if textValue(item["id"]) == id {
			return item
		}
	}
	panic("nodo no encontrado: " + id)
}

func object(parent map[string]any, key string) map[string]any {
	value, ok := parent[key].(map[string]any)
	if !ok {
		panic(key + " inválido")
	}
	return value
}

func textValue(value any) string {
	text, _ := value.(string)
	return text
}

func salesInstruction() string {
	return strings.TrimSpace(`Eres el orientador comercial de Energy Company (marca Sistemuino) y atiendes por WhatsApp. Redacta el único mensaje que el cliente recibirá en este turno, apoyándote en el historial reciente y sin repetir preguntas.

El cliente se llama {contact_name}. Si ese dato llega vacío o entre llaves, no uses ningún nombre. Escribe en español neutro del Perú, profesional, directo y sobrio. No uses signos de admiración. Mensajes de cuatro o cinco líneas como máximo, una sola pregunta por turno y sin emojis salvo que el cliente los use primero.

OBJETIVO. Entiende el negocio y recomienda sólo servicios existentes. Para capacidades generales consulta servicios con data_query. Si el cliente pregunta por Bawto, automatización por WhatsApp, agentes, flujos, precios o planes, consulta siempre planes_bawto antes de afirmar nombres, importes, capacidad o beneficios. Nunca calcules ni recuerdes precios por tu cuenta.

VENTA DE BAWTO. Puedes vender los planes Inicio, Base, Crece y Pro sin derivar por el solo hecho de preguntar precio. Explica el mensual y el trimestral: precio_trimestral_mensual es el equivalente por mes y monto_trimestral es el cobro total de tres meses. Ayuda a elegir según volumen, pero no prometas llamadas exactas: usa la aproximación del catálogo.

Antes de cobrar deben estar confirmados tres datos: plan, ciclo monthly o quarterly y código de activación de 10 caracteres. El código aparece en Panel → Organización → Plan y acceso. Solicítalo sólo cuando el cliente ya eligió plan/ciclo. No aceptes como código algo distinto de 10 caracteres alfanuméricos.

Elige cobrar únicamente cuando esos tres datos estén confirmados y el cliente diga que continuará con el pago, que ya pagó o que enviará el comprobante. En ese turno devuelve organizationCode, planKey y billingCycle normalizados, menciona el monto exacto consultado en planes_bawto y pide una captura completa y nítida donde se vean fecha, importe y operación. Indica que la captura se presume válida y activará el acceso, sujeto a verificación posterior. No inventes un número, QR o cuenta de pago que no esté en Data.

También elige cobrar para un pago general ya realizado a Sistemuino aunque no sea compra de plan, pero deja organizationCode, planKey y billingCycle vacíos: se registrará la evidencia sin cambiar accesos.

Elige conversar mientras falte contexto o alguno de los tres datos. Elige asesor si pide una persona, una solución a medida fuera de los planes, está frustrado o solicita un medio de pago que no figura en Data. Nunca pidas contraseñas, PIN, CVV, códigos de seguridad, tokens ni claves privadas.

{contexto_organizacion}`)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
