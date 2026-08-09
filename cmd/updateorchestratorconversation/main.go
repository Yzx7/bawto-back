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
	dryRun := flag.Bool("dry-run", false, "valida sin escribir PostgreSQL")
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
	tag, err := tx.Exec(ctx, `UPDATE flows SET draft=$3::jsonb,updated_by='codex:conversational-orchestrator'
		WHERE id=$1::uuid AND bot_id=$2::uuid`, *flowID, *botID, updated)
	must(err)
	if tag.RowsAffected() != 1 {
		panic("no se actualizó el borrador")
	}
	must(tx.Commit(ctx))
	fmt.Printf("draft actualizado checksum=%s\n", nextChecksum)
}

// El parche parte del checksum vivo y conserva posiciones, orden de ramas y
// cualquier edición concurrente del panel que no pertenezca a este cambio.
func updateDocument(document map[string]any) {
	nodes := objectSlice(document, "nodes")
	orchestrator := node(nodes, "n_agente")
	delete(orchestrator, "tools")
	orchestrator["agentRole"] = "orchestrator"
	orchestrator["contextMode"] = "recent"
	orchestrator["silent"] = false
	orchestrator["replyOn"] = []any{"aclarar"}
	orchestrator["instruction"] = conversationalOrchestratorInstruction()

	keptNodes := nodes[:0]
	for _, item := range nodes {
		if textValue(item["id"]) != "n_topic_clarify" {
			keptNodes = append(keptNodes, item)
		}
	}
	document["nodes"] = keptNodes

	edges := objectSlice(document, "edges")
	keptEdges := edges[:0]
	for _, item := range edges {
		id := textValue(item["id"])
		if id == "e_clarify_wait" || textValue(item["source"]) == "n_topic_clarify" {
			continue
		}
		if id == "e_orchestrator_clarify" {
			item["target"] = "n_espera"
		}
		keptEdges = append(keptEdges, item)
	}
	document["edges"] = keptEdges
}

func conversationalOrchestratorInstruction() string {
	return strings.TrimSpace(`Eres el orquestador conversacional de Sistemuino. Tu trabajo es comprender qué necesita la persona antes de entregarla al especialista correcto. No ejecutas herramientas, no das precios, no inventas servicios y no intentas resolver todavía el tema especializado.

Lee el mensaje actual y todo el historial reciente. Elige exactamente una rama:

- bawto: automatización de WhatsApp, chatbot, agentes, flujos, planes, precios, capacidades, código de activación o compra de una suscripción Bawto.
- servicios: páginas web, ecommerce, infraestructura, IoT, tarjetas digitales u otro servicio tecnológico de Energy Company.
- pago: afirma que ya pagó, enviará un comprobante, desea registrar un pago o reporta un problema con un cobro. Si todavía está eligiendo un plan Bawto, usa bawto.
- asesor: pide explícitamente una persona, está frustrado o plantea una solución a medida que no puede ubicarse con seguridad en los temas anteriores.
- aclarar: aún falta contexto para elegir con seguridad.

Cuando elijas aclarar, responde al cliente con una sola pregunta breve, natural y concreta que reduzca la ambigüedad. Apóyate en lo que ya dijo, no repitas preguntas contestadas y no muestres un menú genérico si ya existe una pista. Puedes elegir aclarar en varios turnos hasta comprender el objetivo, pero cada pregunta debe aportar información nueva.

Cuando el tema ya esté definido, elige su rama inmediatamente. Tu texto no se enviará en esas ramas porque el especialista responderá en el mismo turno. Ante duda real, pregunta; no adivines.`)
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
