// Herramienta temporal para dejar montado el piloto D-3 de §19 del plan
// (pasos 5 a 9): factura de prueba vinculada al contacto autorizado, vistas
// "Facturas pendientes" y "Vencen en tres días", flujo `WAA — Recordatorio D-3`
// en borrador y preview del resultado.
//
//	go run ./cmd/seedpiloto -bot <uuid> -phone 51973021342 [-apply]
//
// Sin -apply no escribe nada: solo dice qué haría. Es idempotente: repetirlo
// reutiliza lo que ya exista y únicamente reajusta la fecha de vencimiento de la
// factura de prueba para que siga cayendo dentro de la ventana D-3.
//
// No forma parte del núcleo: el sistema es agnóstico al negocio y esto es
// configuración de un bot concreto. Se borra cuando el piloto quede montado.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/Yzx7/sacs-chatbots/engine"
	"github.com/Yzx7/sacs-chatbots/models"
	"github.com/Yzx7/sacs-chatbots/scheduler"
)

const (
	objectKey    = "facturas"
	numeroPrueba = "FAC-PILOTO-D3"
	vistaPend    = "Facturas pendientes"
	vistaD3      = "Vencen en tres días"
	flowKey      = "flow_waa_recordatorio_d3"
	flowName     = "WAA — Recordatorio D-3"
	templateName = "recordatorio_pago_proximo_v1"
	timezone     = "America/Lima"
	cronExpr     = "0 9 * * *"
	autor        = "seedpiloto"
)

func main() {
	_ = godotenv.Load()
	botID := flag.String("bot", "", "uuid del bot")
	phone := flag.String("phone", "", "teléfono normalizado del contacto autorizado")
	apply := flag.Bool("apply", false, "escribe de verdad; sin este flag solo informa")
	flag.Parse()
	if *botID == "" || *phone == "" {
		fail("se requieren -bot y -phone")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		fail("pool: %v", err)
	}
	defer pool.Close()
	fmt.Printf("base de datos: %s/%s\nmodo: %s\n\n",
		pool.Config().ConnConfig.Host, pool.Config().ConnConfig.Database, modo(*apply))

	loc, err := time.LoadLocation(timezone)
	if err != nil {
		fail("zona horaria: %v", err)
	}
	hoy := time.Now().In(loc)
	vence := time.Date(hoy.Year(), hoy.Month(), hoy.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 3)
	venceISO := vence.Format("2006-01-02")

	// El catálogo de datos es de la organización, no del bot: se resuelve una vez
	// y sirve para todo lo que sigue.
	orgID := ""
	if err := pool.QueryRow(ctx, `SELECT org_id::text FROM bots WHERE id=$1::uuid`, *botID).Scan(&orgID); err != nil {
		fail("bot %s: %v", *botID, err)
	}

	// 1 · objeto
	objects, err := models.ListDataObjectsByOrg(ctx, pool, orgID)
	if err != nil {
		fail("objetos: %v", err)
	}
	var object *models.DataObject
	for i := range objects {
		if objects[i].Key == objectKey {
			object = &objects[i]
		}
	}
	if object == nil {
		fail("el bot no tiene un objeto con key %q", objectKey)
	}
	fmt.Printf("objeto %s (%s)\n", object.Key, object.ID)

	// 2 · contacto autorizado
	contactID, contactName := "", ""
	err = pool.QueryRow(ctx, `SELECT id::text, COALESCE(name,'') FROM contacts
		WHERE phone_normalized = $1 AND org_id = (SELECT org_id FROM bots WHERE id = $2::uuid)`,
		*phone, *botID).Scan(&contactID, &contactName)
	if err != nil {
		fail("contacto %s: %v", *phone, err)
	}
	fmt.Printf("contacto %s <%s> (%s)\n", contactName, *phone, contactID)

	// 3 · factura de prueba con vencimiento a tres días
	records, err := models.ListDataRecordsByOrg(ctx, pool, orgID, object.ID)
	if err != nil {
		fail("registros: %v", err)
	}
	recordID := ""
	for _, r := range records {
		var data map[string]any
		if json.Unmarshal(r.Data, &data) == nil && fmt.Sprint(data["numero"]) == numeroPrueba {
			recordID = r.ID
		}
	}
	payload, _ := json.Marshal(map[string]any{
		"numero": numeroPrueba, "periodo": vence.Format("2006-01"), "vencimiento": venceISO,
		"importe": 89.9, "moneda": "PEN", "estado": "pendiente",
	})
	switch {
	case recordID == "" && *apply:
		record, err := models.CreateDataRecordByOrg(ctx, pool, orgID, object.ID, payload)
		if err != nil {
			fail("crear factura: %v", err)
		}
		recordID = record.ID
		fmt.Printf("factura %s creada, vence %s (%s)\n", numeroPrueba, venceISO, recordID)
	case recordID == "":
		fmt.Printf("factura %s: se crearía con vencimiento %s\n", numeroPrueba, venceISO)
	case *apply:
		// No hay UpdateDataRecord en el modelo; la fecha se reajusta aquí para que
		// el piloto siga siendo repetible cualquier día.
		if _, err := pool.Exec(ctx, `UPDATE data_records SET data = $2::jsonb WHERE id = $1::uuid`,
			recordID, payload); err != nil {
			fail("actualizar factura: %v", err)
		}
		fmt.Printf("factura %s reajustada, vence %s (%s)\n", numeroPrueba, venceISO, recordID)
	default:
		fmt.Printf("factura %s ya existe (%s); se reajustaría a %s\n", numeroPrueba, recordID, venceISO)
	}

	// 4 · vínculo factura → contacto
	if recordID != "" && *apply {
		if err := models.LinkRecordContactByOrg(ctx, pool, orgID, recordID, contactID, "primary"); err != nil {
			fail("vincular contacto: %v", err)
		}
		fmt.Println("vínculo primary asegurado")
	}

	// 5 · vistas
	views, err := models.ListDataViewsByOrg(ctx, pool, orgID, object.ID)
	if err != nil {
		fail("vistas: %v", err)
	}
	existentes := map[string]string{}
	for _, v := range views {
		existentes[v.Name] = v.ID
	}
	tres := 3
	filtroPend, _ := json.Marshal(models.DataFilter{Where: []models.DataFilterRule{
		{Field: "estado", Op: "eq", Value: "pendiente"},
	}})
	filtroD3, _ := json.Marshal(models.DataFilter{Where: []models.DataFilterRule{
		{Field: "estado", Op: "eq", Value: "pendiente"},
		{Field: "vencimiento", Op: "date_eq_relative", FromDays: &tres},
	}})
	viewPend := asegurarVista(ctx, pool, orgID, object.ID, vistaPend, filtroPend, existentes, *apply)
	viewD3 := asegurarVista(ctx, pool, orgID, object.ID, vistaD3, filtroD3, existentes, *apply)
	_ = viewPend

	if viewD3 == "" {
		fmt.Println("\nsin -apply no hay vista D-3 todavía: no se puede armar el flujo ni el preview")
		return
	}

	// 6 · flujo D-3 en borrador.
	// Se escribe como JSON literal y no con engine.Flow porque el tipo del motor
	// no tiene `pos`: serializarlo desde Go dejaría el grafo sin coordenadas y el
	// editor apilaría los nodos en el origen.
	definition := json.RawMessage(fmt.Sprintf(`{
  "id": %q,
  "name": %q,
  "trigger": {
    "type": "schedule",
    "cron": %q,
    "timezone": %q,
    "viewId": %q,
    "replyIntent": "payment_reminder_reply",
    "pos": { "x": 0, "y": 0 }
  },
  "nodes": [
    { "id": "aviso", "kind": "send", "pos": { "x": 320, "y": 0 },
      "templateName": %q, "templateLanguage": "es",
      "templateParams": ["{contact_name}", "{record_numero}", "{record_moneda} {record_importe}", "{record_vencimiento}"] },
    { "id": "fin", "kind": "action", "pos": { "x": 640, "y": 0 }, "action": "end" }
  ],
  "edges": [
    { "id": "e-trigger-aviso", "source": "trigger", "target": "aviso" },
    { "id": "e-aviso-fin", "source": "aviso", "target": "fin" }
  ]
}`, flowKey, flowName, cronExpr, timezone, viewD3, templateName))
	var graph engine.Flow
	_ = json.Unmarshal(definition, &graph)
	if err := engine.Validate(&graph); err != nil {
		fail("el grafo no valida: %v", err)
	}
	fmt.Println("\ngrafo válido para el motor")

	flows, err := models.ListFlows(ctx, pool, *botID, true)
	if err != nil {
		fail("flujos: %v", err)
	}
	var flow *models.Flow
	for i := range flows {
		if flows[i].Key == flowKey {
			flow = &flows[i]
		}
	}
	switch {
	case flow == nil && *apply:
		flow, err = models.CreateFlow(ctx, pool, *botID, models.NewFlow{
			Key: flowKey, Name: flowName, TriggerType: "schedule",
			Priority: 100, Draft: definition, UserID: autor,
		})
		if err != nil {
			fail("crear flujo: %v", err)
		}
		fmt.Printf("flujo %s creado en borrador (%s)\n", flowKey, flow.ID)
	case flow == nil:
		fmt.Printf("flujo %s: se crearía en borrador\n", flowKey)
		return
	case *apply:
		flow, err = models.UpdateFlowDraft(ctx, pool, *botID, flow.ID, definition, autor)
		if err != nil {
			fail("guardar borrador: %v", err)
		}
		fmt.Printf("flujo %s: borrador actualizado (%s)\n", flowKey, flow.ID)
	default:
		fmt.Printf("flujo %s ya existe (%s); se actualizaría el borrador\n", flowKey, flow.ID)
	}

	// 7 · validación de plantillas: lo que bloquea publicar hoy
	if _, err := models.ValidateFlowTemplates(ctx, pool, *botID, definition); err != nil {
		fmt.Printf("\npublicar está bloqueado: %v\n", err)
	} else {
		fmt.Println("\nplantillas válidas: el flujo se puede publicar")
	}

	// 8 · preview
	result, err := scheduler.PreviewFlow(ctx, pool, *botID, flow, definition, 2*time.Hour, time.Now())
	if err != nil {
		fail("preview: %v", err)
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	fmt.Printf("\n=== PREVIEW ===\n%s\n", out)
}

func asegurarVista(ctx context.Context, pool *pgxpool.Pool, orgID, objectID, name string,
	filter json.RawMessage, existentes map[string]string, apply bool) string {
	if id := existentes[name]; id != "" {
		fmt.Printf("vista %q ya existe (%s)\n", name, id)
		return id
	}
	if !apply {
		fmt.Printf("vista %q: se crearía\n", name)
		return ""
	}
	v, err := models.CreateDataViewByOrg(ctx, pool, orgID, objectID, name, filter)
	if err != nil {
		fail("crear vista %q: %v", name, err)
	}
	fmt.Printf("vista %q creada (%s)\n", name, v.ID)
	return v.ID
}

func modo(apply bool) string {
	if apply {
		return "APLICANDO cambios"
	}
	return "simulación (sin escribir)"
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
