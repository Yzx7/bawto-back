package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/Yzx7/sacs-chatbots/engine"
	"github.com/Yzx7/sacs-chatbots/models"
)

func main() {
	_ = godotenv.Load(".env")
	_ = godotenv.Load("../.env")

	raw, err := os.ReadFile("db/flows/waa-tienda.json")
	if err != nil {
		raw, err = os.ReadFile("../db/flows/waa-tienda.json")
		if err != nil {
			panic(fmt.Sprintf("leer waa-tienda.json: %v", err))
		}
	}

	var flow engine.Flow
	if err := json.Unmarshal(raw, &flow); err != nil {
		panic(fmt.Sprintf("unmarshal waa-tienda: %v", err))
	}

	flow.ID = "flow_waa_creditos"
	flow.Name = "WAA · Tienda Meudim con Créditos Pay as you go"

	// Modificar los nodos para el nuevo modelo de Créditos Pay as you go
	for i := range flow.Nodes {
		node := &flow.Nodes[i]

		switch node.ID {
		case "n_agente":
			// Actualizar prompt del orquestador en la rama bawto y pago
			node.Instruction = strings.ReplaceAll(node.Instruction,
				"planes, precios, capacidades, código de activación o compra de una suscripción Bawto.",
				"recarga de créditos Pay as you go, precios de créditos, capacidades, código de activación o compra de créditos Bawto.")
			node.Instruction = strings.ReplaceAll(node.Instruction,
				"Si todavía está eligiendo un plan Bawto, usa bawto.",
				"Si todavía está consultando la recarga de créditos Bawto, usa bawto.")

		case "n_read_plans":
			// Consulta de catálogo o instrucciones comerciales
			node.Args = map[string]string{
				"limit":  "4",
				"object": "instrucciones_pago_bawto",
			}
			node.SaveAs = "payment_instructions"

		case "n_bawto_specialist":
			node.Instruction = `Eres el especialista comercial de Bawto. El orquestador ya definió este tema. No llames herramientas: explica el modelo de Créditos Pay as you go de Bawto.

Modelo de Créditos Pay as you go:
- Conversión fija: 5 Soles = 100 créditos (1 Sol = 20 créditos). 1 USD = 400 créditos.
- Paquetes de recarga sugeridos: S/ 5 (100 cr), S/ 20 (400 cr), S/ 50 (1,000 cr - Popular), S/ 100 (2,000 cr), S/ 200 (4,000 cr), o cualquier monto libre en Soles.
- Beneficios clave: Paga estrictamente por lo que consumes en IA (turnos de bots y Copilot de autoría), sin mensualidades forzosas, sin vencimiento de saldo, recarga instantánea por WhatsApp.

Antes de cobrar confirma:
1. Monto en Soles o cantidad de créditos deseada.
2. Código de activación alfanumérico de 10 caracteres (visible en Panel → Facturación / Organización).

Elige cobrar solo si el cliente confirma su código de activación y el monto que recargará, o si ya pagó y enviará el comprobante. Devuelve organizationCode y creditsAmount calculados (o amountPen); pide la captura completa del comprobante (Yape, Plin o transferencia) con fecha, importe y número de operación para su acreditación automática.

Elige conversar mientras falte contexto o algún dato. Elige asesor si pide atención humana personalizada o una solución fuera de créditos.

El cliente se llama {contact_name}. Si llega vacío o entre llaves, no uses nombre. Responde en español neutro del Perú, profesional, directo y sobrio; máximo cinco líneas, una pregunta por turno y sin emojis salvo que el cliente los use primero. No pidas contraseñas, PIN, CVV, códigos de seguridad, tokens ni claves privadas. {contexto_organizacion}`

			node.OutputFields = []engine.AgentOutputField{
				{
					Key:         "organizationCode",
					Type:        "string",
					Description: "Código de activación de 10 caracteres; vacío hasta que el cliente lo confirme.",
				},
				{
					Key:         "creditsAmount",
					Type:        "number",
					Description: "Cantidad de créditos a recargar calculada (monto en Soles * 20).",
				},
				{
					Key:         "amountPen",
					Type:        "number",
					Description: "Monto en Soles (PEN) acordado para la recarga.",
				},
			}

		case "n_services_specialist":
			node.Instruction = strings.ReplaceAll(node.Instruction,
				"deja organizationCode, planKey y billingCycle vacíos",
				"deja organizationCode, creditsAmount y amountPen vacíos")
			node.OutputFields = []engine.AgentOutputField{
				{
					Key:         "organizationCode",
					Type:        "string",
					Description: "Código de activación de 10 caracteres; vacío hasta que el cliente lo confirme.",
				},
				{
					Key:         "creditsAmount",
					Type:        "number",
					Description: "Cantidad de créditos a recargar.",
				},
			}

		case "n_payment_specialist":
			node.Instruction = `Eres el especialista de recepción de pagos de Sistemuino. El orquestador ya confirmó que el tema es un pago. No llames herramientas.

Si el historial confirma una recarga de créditos Bawto, conserva exactamente el código de activación y la cantidad de créditos acordados y devuelve organizationCode y creditsAmount. Si no existe esa recarga confirmada, trátalo como pago general y deja esos datos vacíos. Elige cobrar cuando el cliente ya pagó o enviará el comprobante e indica que enviarás las instrucciones exactas de pago a continuación; no inventes ni redactes números o cuentas. Elige conversar si todavía no queda claro que exista un pago. Elige asesor si disputa un cobro o solicita devolución.

El cliente se llama {contact_name}. Si llega vacío o entre llaves, no uses nombre. Responde en español neutro del Perú, profesional, directo y sobrio; máximo cinco líneas, una pregunta por turno y sin emojis salvo que el cliente los use primero. No pidas contraseñas, PIN, CVV, códigos de seguridad, tokens ni claves privadas. {contexto_organizacion}`

			node.OutputFields = []engine.AgentOutputField{
				{
					Key:         "organizationCode",
					Type:        "string",
					Description: "Código de activación de 10 caracteres; vacío hasta que el cliente lo confirme.",
				},
				{
					Key:         "creditsAmount",
					Type:        "number",
					Description: "Cantidad de créditos a recargar.",
				},
			}

		case "n_payment_needs_review":
			node.Expression = "!empty(sale.organizationCode)"

		case "n_activate_subscription":
			// Usar la nueva tool credit_recharge_activate
			node.ToolRef = "credit_recharge_activate"
			node.SaveAs = "credit_recharge"
			node.Args = map[string]string{
				"phone":           "{contact_phone}",
				"activationCode":  "{sale.organizationCode}",
				"creditsAmount":   "{sale.creditsAmount}",
				"paymentRecordId": "{payment_record.recordId}",
				"idempotencyKey":  "credits:{payment_record.recordId}",
			}

		case "n_payment_ok":
			node.Body = "¡Pago recepcionado y créditos acreditados con éxito! Se han abonado los créditos a tu monedero Pay as you go de Bawto. Puedes consultar tu saldo disponible y llamadas estimadas en el panel."

		case "n_payment_review":
			node.Body = "Pago recepcionado y registrado. Como este comprobante no venía con un código de activación de Bawto, conservé la evidencia en el sistema."
		}
	}

	// Validar el flujo con el engine
	if err := engine.Validate(&flow); err != nil {
		panic(fmt.Sprintf("validación del flujo duplicado falló: %v", err))
	}

	// Guardar a archivo
	outJSON, err := json.MarshalIndent(flow, "", "  ")
	if err != nil {
		panic(err)
	}

	for _, path := range []string{"db/flows/waa-creditos.json", "../db/flows/waa-creditos.json"} {
		_ = os.WriteFile(path, outJSON, 0644)
	}

	fmt.Println("Archivo db/flows/waa-creditos.json creado y validado con éxito.")

	// Insertar o actualizar en PostgreSQL
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		fmt.Println("DATABASE_URL no configurada; omitiendo inserción en BD.")
		return
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		panic(err)
	}
	defer pool.Close()

	// Buscar bot Lered
	var botID string
	err = pool.QueryRow(ctx, `SELECT id::text FROM bots WHERE lower(name) = 'lered' LIMIT 1`).Scan(&botID)
	if err != nil {
		err = pool.QueryRow(ctx, `SELECT b.id::text FROM flows f JOIN bots b ON b.id = f.bot_id WHERE f.key = 'flow_waa_tienda' LIMIT 1`).Scan(&botID)
	}
	if err != nil {
		panic(fmt.Sprintf("no se encontró bot: %v", err))
	}

	flowKey := "flow_waa_creditos"
	var existingFlowID string
	err = pool.QueryRow(ctx, `SELECT id::text FROM flows WHERE bot_id = $1::uuid AND key = $2`, botID, flowKey).Scan(&existingFlowID)

	var currentChecksum string
	if err != nil {
		// Crear nuevo flujo
		created, err := models.CreateFlow(ctx, pool, botID, models.NewFlow{
			Key:         flowKey,
			Name:        flow.Name,
			TriggerType: "message",
			Priority:    100,
			Draft:       outJSON,
		})
		if err != nil {
			panic(fmt.Sprintf("crear flujo en BD: %v", err))
		}
		fmt.Printf("Flujo creado en BD: id=%s key=%s bot_id=%s\n", created.ID, created.Key, botID)
		existingFlowID = created.ID
		_, currentChecksum, _ = engine.CanonicalChecksum(outJSON)
	} else {
		// Actualizar borrador existente
		flowObj, err := models.GetFlow(ctx, pool, botID, existingFlowID)
		if err != nil || flowObj == nil {
			panic(fmt.Sprintf("obtener flujo: %v", err))
		}
		snap, err := models.DraftSnapshotFromFlow(flowObj)
		if err != nil {
			panic(err)
		}
		updatedSnap, err := models.UpdateFlowDraft(ctx, pool, botID, existingFlowID, outJSON, snap.Checksum, "cli-duplicator")
		if err != nil {
			panic(fmt.Sprintf("actualizar draft en BD: %v", err))
		}
		currentChecksum = updatedSnap.Checksum
		fmt.Printf("Borrador de flujo actualizado en BD: id=%s key=%s\n", existingFlowID, flowKey)
	}

	// Publicar la versión
	published, err := models.PublishFlow(ctx, pool, botID, existingFlowID, currentChecksum, "cli-duplicator", true)
	if err != nil {
		fmt.Printf("Aviso al publicar: %v\n", err)
	} else {
		fmt.Printf("Flujo publicado exitosamente: version=%d (created=%v)\n", published.Version.Version, published.Created)
	}
}
