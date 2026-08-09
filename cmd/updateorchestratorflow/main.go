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
	dryRun := flag.Bool("dry-run", false, "valida y calcula checksum sin escribir PostgreSQL")
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
	if *dryRun {
		fmt.Printf("borrador válido checksum=%s\n", nextChecksum)
		return
	}

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
	tag, err := tx.Exec(ctx, `UPDATE flows SET draft=$3::jsonb,updated_by='codex:minimax-orchestrator'
		WHERE id=$1::uuid AND bot_id=$2::uuid`, *flowID, *botID, updated)
	must(err)
	if tag.RowsAffected() != 1 {
		panic("no se actualizó el borrador")
	}
	must(tx.Commit(ctx))
	fmt.Printf("draft actualizado checksum=%s\n", nextChecksum)
}

// Se trabaja como JSON genérico para conservar `pos` y futuras extensiones del
// editor. engine.Flow se usa únicamente como vista de validación antes del write.
func updateDocument(document map[string]any) {
	nodes := objectSlice(document, "nodes")
	orchestrator := node(nodes, "n_agente")
	delete(orchestrator, "tools")
	delete(orchestrator, "saveAs")
	delete(orchestrator, "outputFields")
	orchestrator["agentRole"] = "orchestrator"
	orchestrator["silent"] = true
	orchestrator["contextMode"] = "recent"
	orchestrator["outputs"] = []any{"bawto", "servicios", "pago", "aclarar", "asesor"}
	orchestrator["instruction"] = orchestratorInstruction()

	// El clasificador de comprobantes sigue siendo visual. Marcarlo como
	// especialista documenta su responsabilidad, aunque `accepts:image` tenga
	// precedencia y lo enrute a MiniMax-M3.
	node(nodes, "n_classify_payment")["agentRole"] = "specialist"

	outputFields := []any{
		map[string]any{"key": "organizationCode", "type": "string", "description": "Código de activación de 10 caracteres; vacío hasta que el cliente lo confirme."},
		map[string]any{"key": "planKey", "type": "string", "description": "Exactamente inicio, base, crece o pro; vacío hasta confirmar el plan."},
		map[string]any{"key": "billingCycle", "type": "string", "description": "Exactamente monthly o quarterly; vacío hasta confirmar el ciclo."},
	}
	nodes = append(nodes,
		map[string]any{
			"id": "n_read_plans", "kind": "tool", "toolRef": "data_query", "saveAs": "plan_catalog",
			"pos":  map[string]any{"x": -1050.0, "y": -610.0},
			"args": map[string]any{"object": "planes_bawto", "limit": "8"},
		},
		map[string]any{
			"id": "n_bawto_specialist", "kind": "agent", "agentRole": "specialist", "contextMode": "recent",
			"saveAs": "sale", "silent": false, "outputs": []any{"cobrar", "asesor", "conversar"},
			"outputFields": outputFields, "instruction": bawtoInstruction(),
			"pos": map[string]any{"x": -700.0, "y": -610.0},
		},
		map[string]any{
			"id": "n_read_services", "kind": "tool", "toolRef": "data_query", "saveAs": "service_catalog",
			"pos":  map[string]any{"x": -1050.0, "y": -340.0},
			"args": map[string]any{"object": "servicios", "limit": "8"},
		},
		map[string]any{
			"id": "n_services_specialist", "kind": "agent", "agentRole": "specialist", "contextMode": "recent",
			"saveAs": "sale", "silent": false, "outputs": []any{"cobrar", "asesor", "conversar"},
			"outputFields": outputFields, "instruction": servicesInstruction(),
			"pos": map[string]any{"x": -700.0, "y": -340.0},
		},
		map[string]any{
			"id": "n_payment_specialist", "kind": "agent", "agentRole": "specialist", "contextMode": "recent",
			"saveAs": "sale", "silent": false, "outputs": []any{"cobrar", "asesor", "conversar"},
			"outputFields": outputFields, "instruction": paymentInstruction(),
			"pos": map[string]any{"x": -940.0, "y": -850.0},
		},
		map[string]any{
			"id": "n_topic_clarify", "kind": "send", "pos": map[string]any{"x": -940.0, "y": -80.0},
			"body": "Hola. Puedo ayudarte a automatizar ventas y atención por WhatsApp con Bawto, orientarte sobre servicios tecnológicos o registrar un pago. ¿Qué deseas resolver primero?",
		},
	)
	document["nodes"] = nodes

	edges := objectSlice(document, "edges")
	kept := edges[:0]
	for _, edge := range edges {
		if textValue(edge["source"]) != "n_agente" {
			kept = append(kept, edge)
		}
	}
	edges = append(kept,
		edge("e_orchestrator_bawto", "n_agente", "bawto", "n_read_plans"),
		edge("e_orchestrator_services", "n_agente", "servicios", "n_read_services"),
		edge("e_orchestrator_payment", "n_agente", "pago", "n_payment_specialist"),
		edge("e_orchestrator_clarify", "n_agente", "aclarar", "n_topic_clarify"),
		edge("e_orchestrator_advisor", "n_agente", "asesor", "n_derivar"),
		edge("e_read_plans_ok", "n_read_plans", "ok", "n_bawto_specialist"),
		edge("e_read_plans_error", "n_read_plans", "error", "n_derivar"),
		edge("e_bawto_charge", "n_bawto_specialist", "cobrar", "n_payment_wait"),
		edge("e_bawto_advisor", "n_bawto_specialist", "asesor", "n_derivar"),
		edge("e_bawto_talk", "n_bawto_specialist", "conversar", "n_espera"),
		edge("e_read_services_ok", "n_read_services", "ok", "n_services_specialist"),
		edge("e_read_services_error", "n_read_services", "error", "n_derivar"),
		edge("e_services_charge", "n_services_specialist", "cobrar", "n_payment_wait"),
		edge("e_services_advisor", "n_services_specialist", "asesor", "n_derivar"),
		edge("e_services_talk", "n_services_specialist", "conversar", "n_espera"),
		edge("e_payment_charge", "n_payment_specialist", "cobrar", "n_payment_wait"),
		edge("e_payment_advisor", "n_payment_specialist", "asesor", "n_derivar"),
		edge("e_payment_talk", "n_payment_specialist", "conversar", "n_espera"),
		edge("e_clarify_wait", "n_topic_clarify", "", "n_espera"),
	)
	document["edges"] = edges
}

func orchestratorInstruction() string {
	return strings.TrimSpace(`Eres el orquestador silencioso de Sistemuino. No respondas al cliente, no vendas y no ejecutes herramientas. Lee el mensaje actual y el historial solo para elegir exactamente un tema.

bawto: automatización de WhatsApp, chatbot, agentes, flujos, planes, precios, capacidades, código de activación o compra de una suscripción Bawto.
servicios: páginas web, ecommerce, infraestructura, IoT, tarjetas digitales u otro servicio tecnológico del catálogo de Energy Company.
pago: el cliente afirma que ya pagó, que enviará un comprobante o pide registrar un pago; si todavía está eligiendo un plan Bawto, usa bawto.
asesor: pide explícitamente una persona, está frustrado o plantea una solución a medida que no encaja en los dos temas.
aclarar: saludo, texto vacío, mensaje ambiguo o tema que todavía no puede asignarse con seguridad.

No deduzcas precios ni datos comerciales. Ante duda elige aclarar.`)
}

func commonStyle() string {
	return `El cliente se llama {contact_name}. Si llega vacío o entre llaves, no uses nombre. Responde en español neutro del Perú, profesional, directo y sobrio; máximo cinco líneas, una pregunta por turno y sin emojis salvo que el cliente los use primero. No pidas contraseñas, PIN, CVV, códigos de seguridad, tokens ni claves privadas. {contexto_organizacion}`
}

func bawtoInstruction() string {
	return strings.TrimSpace(`Eres el especialista comercial de Bawto. El orquestador ya definió este tema. No llames herramientas: el catálogo vigente está en este JSON y es tu única fuente para nombres, precios, ciclos, capacidad y beneficios:
{plan_catalog}

Puedes vender Inicio, Base, Crece y Pro. Explica mensual y trimestral: precio_trimestral_mensual es equivalente mensual y monto_trimestral es el cobro total de tres meses. No prometas llamadas exactas; usa la aproximación del catálogo.

Antes de cobrar confirma plan, ciclo monthly o quarterly y código de activación alfanumérico de 10 caracteres, visible en Panel → Organización → Plan y acceso. Elige cobrar solo si los tres están confirmados y el cliente continuará con el pago, ya pagó o enviará el comprobante. Devuelve organizationCode, planKey y billingCycle normalizados; menciona el importe exacto del catálogo y pide captura completa con fecha, importe y operación. La captura se presume válida y activa acceso, sujeta a verificación posterior.

Elige conversar mientras falte contexto o un dato. Elige asesor si pide una persona, una solución fuera de planes o un medio de pago ausente del catálogo.

` + commonStyle())
}

func servicesInstruction() string {
	return strings.TrimSpace(`Eres el especialista comercial de servicios tecnológicos de Energy Company. El orquestador ya definió este tema. No llames herramientas y recomienda únicamente registros presentes en este catálogo:
{service_catalog}

Aclara alcance con una sola pregunta y no inventes precios, plazos, capacidades ni servicios. Elige conversar para orientar o completar contexto. Elige asesor cuando el cliente pida una cotización, una persona o una solución a medida. Elige cobrar solo si afirma que ya hizo un pago general a Sistemuino; deja organizationCode, planKey y billingCycle vacíos y pide la captura completa para registrar evidencia sin modificar accesos.

` + commonStyle())
}

func paymentInstruction() string {
	return strings.TrimSpace(`Eres el especialista de recepción de pagos de Sistemuino. El orquestador ya confirmó que el tema es un pago. No llames herramientas.

Si el historial confirma una compra Bawto, conserva exactamente plan, ciclo y código de activación ya acordados y devuelve organizationCode, planKey y billingCycle normalizados. Si no existe esa compra confirmada, trátalo como pago general y deja esos tres datos vacíos. Elige cobrar cuando el cliente ya pagó o enviará el comprobante y pide una captura completa y nítida con fecha, importe y operación. Elige conversar si todavía no queda claro que exista un pago. Elige asesor si disputa un cobro o solicita devolución.

` + commonStyle())
}

func edge(id, source, handle, target string) map[string]any {
	value := map[string]any{"id": id, "source": source, "target": target}
	if handle != "" {
		value["sourceHandle"] = handle
	}
	return value
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

func textValue(value any) string {
	text, _ := value.(string)
	return text
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
