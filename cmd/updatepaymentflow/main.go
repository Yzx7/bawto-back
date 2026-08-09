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
	baseFile := flag.String("base-file", "", "exportación fresca del borrador")
	baseChecksum := flag.String("base-checksum", "", "checksum de la exportación")
	expected := flag.String("expected-checksum", "", "checksum actual en PostgreSQL")
	outputFile := flag.String("output-file", "", "archivo opcional para inspeccionar el resultado")
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
	updated, err := json.MarshalIndent(document, "", "  ")
	must(err)
	var validationFlow engine.Flow
	must(json.Unmarshal(updated, &validationFlow))
	must(engine.Validate(&validationFlow))
	_, nextChecksum, err := engine.CanonicalChecksum(updated)
	must(err)
	if *outputFile != "" {
		must(os.WriteFile(*outputFile, updated, 0o600))
	}
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
	tag, err := tx.Exec(ctx, `UPDATE flows SET draft=$3::jsonb,updated_by='codex:deterministic-payment-instructions'
		WHERE id=$1::uuid AND bot_id=$2::uuid`, *flowID, *botID, updated)
	must(err)
	if tag.RowsAffected() != 1 {
		panic("no se actualizó el borrador")
	}
	must(tx.Commit(ctx))
	fmt.Printf("draft actualizado checksum=%s\n", nextChecksum)
}

// Trabaja sobre el JSON genérico para conservar `pos` y extensiones del editor.
func updateDocument(document map[string]any) {
	nodes := objectSlice(document, "nodes")
	managedNodes := map[string]bool{
		"n_read_payment_method": true, "n_payment_method_router": true,
		"n_render_payment_methods":    true,
		"n_send_payment_instructions": true, "n_payment_method_unavailable": true,
		"n_payment_method_handoff": true,
	}
	keptNodes := nodes[:0]
	for _, item := range nodes {
		if !managedNodes[textValue(item["id"])] {
			keptNodes = append(keptNodes, item)
		}
	}
	nodes = keptNodes
	for _, id := range []string{"n_bawto_specialist", "n_services_specialist", "n_payment_specialist"} {
		item := node(nodes, id)
		instruction := textValue(item["instruction"])
		item["instruction"] = strings.ReplaceAll(instruction,
			"pide una captura completa con fecha, importe y operación.",
			"indica que enviarás las instrucciones exactas de pago a continuación; no inventes ni redactes números o cuentas.")
		item["instruction"] = strings.ReplaceAll(textValue(item["instruction"]),
			"pide la captura completa para registrar evidencia sin modificar accesos.",
			"indica que enviarás las instrucciones exactas de pago a continuación; no inventes ni redactes números o cuentas.")
		item["instruction"] = strings.ReplaceAll(textValue(item["instruction"]),
			"pide una captura completa y nítida con fecha, importe y operación.",
			"indica que enviarás las instrucciones exactas de pago a continuación; no inventes ni redactes números o cuentas.")
	}

	nodes = append(nodes,
		map[string]any{"id": "n_render_payment_methods", "kind": "tool", "toolRef": "payment_methods_render", "saveAs": "payment_options", "pos": map[string]any{"x": -300.0, "y": -1260.0}, "args": map[string]any{}},
		map[string]any{"id": "n_payment_method_router", "kind": "router", "pos": map[string]any{"x": 10.0, "y": -1260.0}, "cases": []any{
			map[string]any{"id": "configured", "label": "Métodos activos", "expression": "payment_options.found == true && !empty(payment_options.message)"},
		}},
		map[string]any{"id": "n_send_payment_instructions", "kind": "send", "pos": map[string]any{"x": 330.0, "y": -1420.0}, "body": "{payment_options.message}"},
		map[string]any{"id": "n_payment_method_unavailable", "kind": "send", "pos": map[string]any{"x": 330.0, "y": -1100.0}, "body": "Ahora mismo no tengo métodos de pago activos que pueda darte con seguridad. Te derivaré con el equipo para continuar sin exponerte a datos incorrectos."},
		map[string]any{"id": "n_payment_method_handoff", "kind": "action", "action": "handoff", "pos": map[string]any{"x": 650.0, "y": -1100.0}},
	)
	node(nodes, "n_payment_wait")["pos"] = map[string]any{"x": 650.0, "y": -1420.0}
	document["nodes"] = nodes

	edges := objectSlice(document, "edges")
	managedEdges := map[string]bool{
		"e_payment_method_read": true, "e_payment_method_read_error": true,
		"e_payment_method_configured": true, "e_payment_method_missing": true,
		"e_payment_instructions_sent": true, "e_payment_method_handoff": true,
		"e_payment_handoff_end": true,
	}
	keptEdges := edges[:0]
	for _, edge := range edges {
		if !managedEdges[textValue(edge["id"])] {
			keptEdges = append(keptEdges, edge)
		}
	}
	edges = keptEdges
	for _, edge := range edges {
		switch textValue(edge["id"]) {
		case "e_bawto_charge", "e_services_charge", "e_payment_charge":
			edge["target"] = "n_render_payment_methods"
		}
	}
	edges = append(edges,
		map[string]any{"id": "e_payment_method_read", "source": "n_render_payment_methods", "target": "n_payment_method_router", "sourceHandle": "ok"},
		map[string]any{"id": "e_payment_method_read_error", "source": "n_render_payment_methods", "target": "n_payment_method_unavailable", "sourceHandle": "error"},
		map[string]any{"id": "e_payment_method_configured", "source": "n_payment_method_router", "target": "n_send_payment_instructions", "sourceHandle": "configured"},
		map[string]any{"id": "e_payment_method_missing", "source": "n_payment_method_router", "target": "n_payment_method_unavailable", "sourceHandle": "default"},
		map[string]any{"id": "e_payment_instructions_sent", "source": "n_send_payment_instructions", "target": "n_payment_wait"},
		map[string]any{"id": "e_payment_method_handoff", "source": "n_payment_method_unavailable", "target": "n_payment_method_handoff"},
		map[string]any{"id": "e_payment_handoff_end", "source": "n_payment_method_handoff", "target": "n_fin"},
	)
	document["edges"] = edges
}

func objectSlice(document map[string]any, key string) []map[string]any {
	switch raw := document[key].(type) {
	case []map[string]any:
		return raw
	case []any:
		items := make([]map[string]any, 0, len(raw))
		for _, value := range raw {
			item, ok := value.(map[string]any)
			if !ok {
				panic(key + " contiene un elemento inválido")
			}
			items = append(items, item)
		}
		return items
	default:
		panic(key + " inválido")
	}
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
