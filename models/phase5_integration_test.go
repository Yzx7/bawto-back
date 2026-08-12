package models

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// Fase 5: historial de runs y duplicado de flujos.
//
//	DATABASE_URL=... go test ./models -run Phase5 -v

func TestPhase5HistorialDeRuns(t *testing.T) {
	pool, ctx := flowTestPool(t)
	bot := botDePrueba(t, ctx, pool, "ph5_")

	raw := grafoValido("f_hist", "Historial", "hola")
	flow, err := CreateFlow(ctx, pool, bot.ID, NewFlow{
		Key: "historial", Name: "Historial", TriggerType: "message", Draft: raw, UserID: "tester",
	})
	if err != nil {
		t.Fatalf("CreateFlow: %v", err)
	}
	version := publica(t, ctx, pool, bot.ID, flow.ID, raw)

	object, err := CreateDataObjectByOrg(ctx, pool, bot.OrgID, "cobros5", "Cobro", "Cobros")
	if err != nil {
		t.Fatalf("CreateDataObject: %v", err)
	}
	if _, err = UpsertDataFieldByOrg(ctx, pool, bot.OrgID, object.ID, "monto", "Monto", "text", false); err != nil {
		t.Fatalf("UpsertDataField: %v", err)
	}
	record, err := CreateDataRecordByOrg(ctx, pool, bot.OrgID, object.ID, json.RawMessage(`{"monto":"90"}`))
	if err != nil {
		t.Fatalf("CreateDataRecord: %v", err)
	}
	contact, err := SaveContactByOrg(ctx, pool, bot.OrgID, "", "51999888777", "Cliente Cinco", "active", nil)
	if err != nil {
		t.Fatalf("UpsertContact: %v", err)
	}

	at := time.Now().UTC().Truncate(time.Minute)
	run, created, err := CreateFlowRun(ctx, pool, bot.ID, flow.ID, version.Version.ID,
		record.ID, contact.ID, "ph5-run-1", "schedule", at, json.RawMessage(`{"record_monto":"90"}`))
	if err != nil || !created {
		t.Fatalf("CreateFlowRun: created=%v err=%v", created, err)
	}

	// La lista trae el nombre del flujo y del contacto sin una consulta por fila.
	page, err := ListFlowRuns(ctx, pool, bot.ID, FlowRunFilter{})
	if err != nil {
		t.Fatalf("ListFlowRuns: %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("historial inesperado: total=%d items=%d", page.Total, len(page.Items))
	}
	item := page.Items[0]
	if item.FlowName != "Historial" || item.ContactPhone == nil || *item.ContactPhone != "51999888777" {
		t.Fatalf("join incompleto: %+v", item)
	}

	// Un filtro por estado que no coincide no debe devolver nada, y el contador
	// por estado debe seguir viéndose entero (si no, la UI se queda ciega en
	// cuanto filtras).
	empty, err := ListFlowRuns(ctx, pool, bot.ID, FlowRunFilter{Statuses: []string{"sent"}})
	if err != nil || empty.Total != 0 {
		t.Fatalf("filtro por estado: total=%d err=%v", empty.Total, err)
	}
	counts, err := CountFlowRunsByStatus(ctx, pool, bot.ID, FlowRunFilter{Statuses: []string{"sent"}})
	if err != nil || len(counts) != 1 || counts[0].Status != "queued" || counts[0].Count != 1 {
		t.Fatalf("contadores por estado: %+v err=%v", counts, err)
	}

	// Un run de otro bot no se ve ni por id: el id nunca es la autorización.
	otro := botDePrueba(t, ctx, pool, "ph5b_")
	if ajeno, err := GetFlowRun(ctx, pool, otro.ID, run.ID); err != nil || ajeno != nil {
		t.Fatalf("aislamiento entre bots roto: run=%+v err=%v", ajeno, err)
	}

	detail, err := GetFlowRun(ctx, pool, bot.ID, run.ID)
	if err != nil || detail == nil {
		t.Fatalf("GetFlowRun: %v", err)
	}
	if detail.FlowVersion == nil || *detail.FlowVersion != 1 {
		t.Fatalf("el detalle debe traer la versión congelada: %+v", detail.FlowVersion)
	}
	if detail.RecordMissing || len(detail.RecordData) == 0 {
		t.Fatalf("el detalle debe traer el registro: missing=%v data=%s", detail.RecordMissing, detail.RecordData)
	}

	// Un run en cola no se reintenta: lo hará el worker.
	if _, err := RequeueFlowRun(ctx, pool, bot.ID, run.ID, "tester"); !errors.Is(err, ErrRunNotRetryable) {
		t.Fatalf("esperaba ErrRunNotRetryable, got %v", err)
	}
	// Pero sí se cancela.
	cancelled, err := CancelFlowRunForBot(ctx, pool, bot.ID, run.ID, "prueba")
	if err != nil || cancelled == nil || cancelled.Status != "cancelled" {
		t.Fatalf("CancelFlowRunForBot: run=%+v err=%v", cancelled, err)
	}
	if cancelled.CancelReason == nil || *cancelled.CancelReason != "prueba" {
		t.Fatalf("el motivo de cancelación se perdió: %+v", cancelled.CancelReason)
	}
	// Cancelar dos veces es un conflicto explícito, no un no-op silencioso.
	if _, err := CancelFlowRunForBot(ctx, pool, bot.ID, run.ID, "prueba"); !errors.Is(err, ErrRunNotCancellable) {
		t.Fatalf("esperaba ErrRunNotCancellable, got %v", err)
	}
	// Y ahora sí se puede reencolar a mano.
	requeued, err := RequeueFlowRun(ctx, pool, bot.ID, run.ID, "tester")
	if err != nil || requeued == nil || requeued.Status != "queued" {
		t.Fatalf("RequeueFlowRun: run=%+v err=%v", requeued, err)
	}
	if requeued.Attempt != 0 || requeued.CancelReason != nil {
		t.Fatalf("el reencolado debe limpiar intento y motivo: %+v", requeued)
	}
}

func TestPhase5DuplicarFlujo(t *testing.T) {
	pool, ctx := flowTestPool(t)
	bot := botDePrueba(t, ctx, pool, "ph5d_")

	original := grafoValido("f_dup", "Atención", "hola")
	flow, err := CreateFlow(ctx, pool, bot.ID, NewFlow{
		Key: "atencion", Name: "Atención", TriggerType: "message",
		IsFallback: true, Draft: original, UserID: "tester",
	})
	if err != nil {
		t.Fatalf("CreateFlow: %v", err)
	}
	publica(t, ctx, pool, bot.ID, flow.ID, original)

	// El borrador se ensucia después de publicar: la copia debe traer lo
	// publicado, que es lo que el operador ve corriendo.
	sucio := grafoValido("f_dup", "Atención", "borrador a medias")
	snapshot, err := DraftSnapshotFromFlow(flow)
	if err != nil {
		t.Fatalf("DraftSnapshotFromFlow: %v", err)
	}
	if _, err := UpdateFlowDraft(ctx, pool, bot.ID, flow.ID, sucio, snapshot.Checksum, "tester"); err != nil {
		t.Fatalf("UpdateFlowDraft: %v", err)
	}

	copia, err := DuplicateFlow(ctx, pool, bot.ID, flow.ID, "tester")
	if err != nil || copia == nil {
		t.Fatalf("DuplicateFlow: %v", err)
	}
	if copia.Key != "atencion-copia" || copia.Name != "Atención (copia)" {
		t.Fatalf("clave o nombre de la copia: %+v", copia)
	}
	if copia.Status != "draft" || copia.PublishedVersionID != nil {
		t.Fatalf("la copia debe nacer en borrador: %+v", copia)
	}
	if copia.IsFallback {
		t.Fatal("la copia no debe heredar is_fallback: solo un message vivo puede serlo")
	}
	if !containsBody(t, copia.Draft, "hola") {
		t.Fatalf("la copia no trae el grafo publicado: %s", copia.Draft)
	}

	// Duplicar otra vez no colisiona con la copia anterior.
	segunda, err := DuplicateFlow(ctx, pool, bot.ID, flow.ID, "tester")
	if err != nil || segunda == nil {
		t.Fatalf("segunda copia: %v", err)
	}
	if segunda.Key != "atencion-copia-2" {
		t.Fatalf("la segunda copia debe buscar otra key: %q", segunda.Key)
	}

	// Archivar libera la key, así que la siguiente copia vuelve a la primera.
	if _, err := ArchiveFlow(ctx, pool, bot.ID, copia.ID, "tester"); err != nil {
		t.Fatalf("ArchiveFlow: %v", err)
	}
	tercera, err := DuplicateFlow(ctx, pool, bot.ID, flow.ID, "tester")
	if err != nil || tercera == nil || tercera.Key != "atencion-copia" {
		t.Fatalf("archivar debe liberar la key: %+v err=%v", tercera, err)
	}
}

// containsBody comprueba que el grafo copiado conserva el texto del nodo send.
func containsBody(t *testing.T, raw json.RawMessage, body string) bool {
	t.Helper()
	var doc struct {
		Nodes []struct {
			Body string `json:"body"`
		} `json:"nodes"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return false
	}
	for _, node := range doc.Nodes {
		if node.Body == body {
			return true
		}
	}
	return false
}

func TestPhase5FechasInvalidasSalenALaLuz(t *testing.T) {
	pool, ctx := flowTestPool(t)
	bot := botDePrueba(t, ctx, pool, "ph5f_")

	object, err := CreateDataObjectByOrg(ctx, pool, bot.OrgID, "recibos5", "Recibo", "Recibos")
	if err != nil {
		t.Fatalf("CreateDataObject: %v", err)
	}
	if _, err = UpsertDataFieldByOrg(ctx, pool, bot.OrgID, object.ID, "vence", "Vence", "date", false); err != nil {
		t.Fatalf("UpsertDataField: %v", err)
	}
	for _, raw := range []string{
		`{"vence":"2026-07-31"}`, // válida
		`{"vence":"31/07/2026"}`, // formato no ISO
		`{"vence":"2026-02-31"}`, // día imposible
		`{"vence":""}`,           // ausente, no rota
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO data_records(object_id,data) VALUES($1::uuid,$2::jsonb)`,
			object.ID, raw); err != nil {
			t.Fatalf("insertar %s: %v", raw, err)
		}
	}
	days := 3
	filter, _ := json.Marshal(DataFilter{Where: []DataFilterRule{{
		Field: "vence", Op: "date_eq_relative", FromDays: &days,
	}}})
	view, err := CreateDataViewByOrg(ctx, pool, bot.OrgID, object.ID, "Vence D+3", filter)
	if err != nil {
		t.Fatalf("CreateDataView: %v", err)
	}

	invalid, err := InvalidDateRecordsForView(ctx, pool, bot.ID, view.ID, 100)
	if err != nil {
		t.Fatalf("InvalidDateRecordsForView: %v", err)
	}
	if len(invalid) != 2 {
		t.Fatalf("esperaba las dos fechas rotas (y no la vacía), got %+v", invalid)
	}
	for _, record := range invalid {
		if record.Field != "vence" || record.Value == "" {
			t.Fatalf("el registro debe decir qué campo y qué valor: %+v", record)
		}
	}

	// Un bot de otra org no puede auditar esta vista.
	otro := botDePrueba(t, ctx, pool, "ph5g_")
	if ajenos, err := InvalidDateRecordsForView(ctx, pool, otro.ID, view.ID, 100); err != nil || ajenos != nil {
		t.Fatalf("aislamiento de la vista roto: %+v err=%v", ajenos, err)
	}
}
