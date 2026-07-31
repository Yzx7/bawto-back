package models

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Yzx7/sacs-chatbots/engine"
)

// Tests de integración de la tabla `flows` (fase 1 del plan multiflujos).
// Mismo gateado que el resto de models: requiere DATABASE_URL apuntando a una
// base **desechable** con schema.sql aplicado.
//
//	DATABASE_URL=... go test ./models -run Flow -v

// grafoValido devuelve un grafo mínimo que engine.Validate acepta.
func grafoValido(id, name, body string) json.RawMessage {
	f := map[string]any{
		"id":      id,
		"name":    name,
		"trigger": map[string]any{"type": "message", "match": "any"},
		"nodes": []any{
			map[string]any{"id": "n1", "kind": "send", "body": body, "pos": map[string]any{"x": 20, "y": 80}},
			map[string]any{"id": "n2", "kind": "action", "action": "end", "pos": map[string]any{"x": 220, "y": 80}},
		},
		"edges": []any{
			map[string]any{"id": "e0", "source": "trigger", "target": "n1"},
			map[string]any{"id": "e1", "source": "n1", "target": "n2"},
		},
	}
	raw, _ := json.Marshal(f)
	return raw
}

// publica valida + normaliza + publica, igual que hace el controlador.
func publica(t *testing.T, ctx context.Context, p *pgxpool.Pool, botID, flowID string, raw json.RawMessage) *PublishResult {
	t.Helper()
	var parsed engine.Flow
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal grafo: %v", err)
	}
	if err := engine.Validate(&parsed); err != nil {
		t.Fatalf("engine.Validate rechazó el grafo de prueba: %v", err)
	}
	def, sum, err := engine.CanonicalChecksum(raw)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	res, err := PublishFlow(ctx, p, botID, flowID, def, sum, "tester")
	if err != nil {
		t.Fatalf("PublishFlow: %v", err)
	}
	if res == nil {
		t.Fatal("PublishFlow devolvió nil (¿flujo de otro bot?)")
	}
	return res
}

func flowTestPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL no seteada; se omite el test de integración")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

// botDePrueba crea usuario + org + bot y los borra al terminar.
func botDePrueba(t *testing.T, ctx context.Context, pool *pgxpool.Pool, prefijo string) *Bot {
	t.Helper()
	u := randID(prefijo)
	mustUser(t, ctx, pool, u, "Flow Owner", u+"@test.local")
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, u) })

	org, err := CreateOrganization(ctx, pool, u, "Org flows", nil, nil)
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	t.Cleanup(func() { DeleteOrganization(ctx, pool, org.ID) })

	bot, err := CreateBot(ctx, pool, org.ID, "Bot flows", "")
	if err != nil {
		t.Fatalf("CreateBot: %v", err)
	}
	return bot
}

// Publicar dos veces el mismo grafo no crea dos versiones (§5.2, paso 4), ni
// siquiera si el JSON llega con las claves en otro orden.
func TestFlowPublicarDosVecesEsNoOp(t *testing.T) {
	pool, ctx := flowTestPool(t)
	bot := botDePrueba(t, ctx, pool, "fl1_")

	raw := grafoValido("f_atencion", "Atención", "hola")
	flow, err := CreateFlow(ctx, pool, bot.ID, NewFlow{
		Key: "atencion", Name: "Atención", TriggerType: "message", Draft: raw, UserID: "tester",
	})
	if err != nil {
		t.Fatalf("CreateFlow: %v", err)
	}

	first := publica(t, ctx, pool, bot.ID, flow.ID, raw)
	if !first.Created || first.Version.Version != 1 {
		t.Fatalf("primera publicación: created=%v version=%d", first.Created, first.Version.Version)
	}
	if first.Flow.Status != "published" || first.Flow.PublishedVersionID == nil {
		t.Fatalf("el flujo no quedó publicado: %+v", first.Flow)
	}

	// Mismo grafo, distinto orden de claves: sigue siendo el mismo flujo.
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	reordenado, _ := json.Marshal(doc)

	second := publica(t, ctx, pool, bot.ID, flow.ID, reordenado)
	if second.Created {
		t.Fatal("republicar el mismo grafo creó una versión nueva")
	}
	if second.Version.ID != first.Version.ID {
		t.Fatalf("el no-op debe devolver la versión existente: %s vs %s", second.Version.ID, first.Version.ID)
	}
	versions, err := ListFlowVersions(ctx, pool, bot.ID, flow.ID)
	if err != nil || len(versions) != 1 {
		t.Fatalf("ListFlowVersions: err=%v len=%d (se esperaba 1)", err, len(versions))
	}
}

// Restaurar una versión antigua y republicarla funciona: es la razón de que
// flow_versions NO tenga UNIQUE (flow_id, checksum) (§5.2).
func TestFlowRestaurarYRepublicar(t *testing.T) {
	pool, ctx := flowTestPool(t)
	bot := botDePrueba(t, ctx, pool, "fl2_")

	v1raw := grafoValido("f_r", "Recordatorio", "versión uno")
	flow, err := CreateFlow(ctx, pool, bot.ID, NewFlow{
		Key: "recordatorio", Name: "Recordatorio", TriggerType: "message", Draft: v1raw, UserID: "tester",
	})
	if err != nil {
		t.Fatalf("CreateFlow: %v", err)
	}
	v1 := publica(t, ctx, pool, bot.ID, flow.ID, v1raw)

	v2raw := grafoValido("f_r", "Recordatorio", "versión dos")
	if _, err := UpdateFlowDraft(ctx, pool, bot.ID, flow.ID, v2raw, "tester"); err != nil {
		t.Fatalf("UpdateFlowDraft: %v", err)
	}
	v2 := publica(t, ctx, pool, bot.ID, flow.ID, v2raw)
	if !v2.Created || v2.Version.Version != 2 {
		t.Fatalf("segunda publicación: created=%v version=%d", v2.Created, v2.Version.Version)
	}

	// Restaurar deja la definición vieja en el borrador, sin publicar.
	restored, err := RestoreFlowVersion(ctx, pool, bot.ID, flow.ID, v1.Version.ID, "tester")
	if err != nil || restored == nil {
		t.Fatalf("RestoreFlowVersion: err=%v flow=%v", err, restored)
	}
	if *restored.PublishedVersionID != v2.Version.ID {
		t.Fatal("restaurar no debe cambiar la versión publicada")
	}
	_, restoredSum, _ := engine.CanonicalChecksum(restored.Draft)
	if restoredSum != v1.Version.Checksum {
		t.Fatal("el borrador restaurado no coincide con la versión 1")
	}

	// Republicar la definición vieja crea la versión 3, con el checksum de la 1.
	v3 := publica(t, ctx, pool, bot.ID, flow.ID, restored.Draft)
	if !v3.Created || v3.Version.Version != 3 {
		t.Fatalf("republicar lo restaurado: created=%v version=%d", v3.Created, v3.Version.Version)
	}
	if v3.Version.Checksum != v1.Version.Checksum {
		t.Fatal("la versión 3 debería tener el mismo checksum que la 1")
	}
	if v3.Version.ID == v1.Version.ID {
		t.Fatal("una versión publicada es inmutable: la 3 no puede ser la misma fila que la 1")
	}
}

// Archivar libera la key: duplicar un recordatorio archivado no debe obligar a
// inventar "recordatorio-d3-v2" (§5.1).
func TestFlowArchivarLiberaLaKey(t *testing.T) {
	pool, ctx := flowTestPool(t)
	bot := botDePrueba(t, ctx, pool, "fl3_")

	raw := grafoValido("f_d3", "D-3", "vence en tres días")
	first, err := CreateFlow(ctx, pool, bot.ID, NewFlow{
		Key: "recordatorio-d3", Name: "D-3", TriggerType: "message", Draft: raw, UserID: "tester",
	})
	if err != nil {
		t.Fatalf("CreateFlow: %v", err)
	}
	publica(t, ctx, pool, bot.ID, first.ID, raw)

	// Con el primero vivo, la key está tomada.
	if _, err := CreateFlow(ctx, pool, bot.ID, NewFlow{
		Key: "recordatorio-d3", Name: "D-3 bis", TriggerType: "message", UserID: "tester",
	}); err != ErrFlowKeyTaken {
		t.Fatalf("se esperaba ErrFlowKeyTaken, got %v", err)
	}

	if _, err := ArchiveFlow(ctx, pool, bot.ID, first.ID, "tester"); err != nil {
		t.Fatalf("ArchiveFlow: %v", err)
	}

	// Un flujo archivado deja de ejecutarse.
	if def, err := PublishedFlowDefinition(ctx, pool, bot.ID, "message"); err != nil || def != nil {
		t.Fatalf("un flujo archivado no debe resolverse: err=%v def=%s", err, def)
	}
	// Y no aparece en la lista operativa, pero sí en el historial.
	if list, err := ListFlows(ctx, pool, bot.ID, false); err != nil || len(list) != 0 {
		t.Fatalf("ListFlows(vivos): err=%v len=%d", err, len(list))
	}
	if list, err := ListFlows(ctx, pool, bot.ID, true); err != nil || len(list) != 1 {
		t.Fatalf("ListFlows(con archivados): err=%v len=%d", err, len(list))
	}

	second, err := CreateFlow(ctx, pool, bot.ID, NewFlow{
		Key: "recordatorio-d3", Name: "D-3 nuevo", TriggerType: "message", Draft: raw, UserID: "tester",
	})
	if err != nil {
		t.Fatalf("la key no se liberó al archivar: %v", err)
	}
	if second.ID == first.ID {
		t.Fatal("se esperaba un flujo nuevo")
	}
	// El archivado ya no admite ediciones.
	if f, err := UpdateFlowDraft(ctx, pool, bot.ID, first.ID, raw, "tester"); err != nil || f != nil {
		t.Fatalf("un flujo archivado no debe poder editarse: err=%v flow=%v", err, f)
	}
}

// Aislamiento entre organizaciones: los flujos de un bot son invisibles e
// intocables desde otro bot, aunque se conozca el UUID.
func TestFlowAislamientoEntreOrganizaciones(t *testing.T) {
	pool, ctx := flowTestPool(t)
	botA := botDePrueba(t, ctx, pool, "fla_")
	botB := botDePrueba(t, ctx, pool, "flb_")

	raw := grafoValido("f_a", "De la org A", "privado")
	flowA, err := CreateFlow(ctx, pool, botA.ID, NewFlow{
		Key: "privado", Name: "De la org A", TriggerType: "message", Draft: raw, UserID: "tester",
	})
	if err != nil {
		t.Fatalf("CreateFlow: %v", err)
	}
	pub := publica(t, ctx, pool, botA.ID, flowA.ID, raw)

	if got, err := GetFlow(ctx, pool, botB.ID, flowA.ID); err != nil || got != nil {
		t.Fatalf("GetFlow cruzado devolvió %v (err=%v)", got, err)
	}
	if list, err := ListFlows(ctx, pool, botB.ID, true); err != nil || len(list) != 0 {
		t.Fatalf("ListFlows del bot B: err=%v len=%d", err, len(list))
	}
	if v, err := GetFlowVersion(ctx, pool, botB.ID, flowA.ID, pub.Version.ID); err != nil || v != nil {
		t.Fatalf("GetFlowVersion cruzado devolvió %v (err=%v)", v, err)
	}
	if vs, err := ListFlowVersions(ctx, pool, botB.ID, flowA.ID); err != nil || len(vs) != 0 {
		t.Fatalf("ListFlowVersions cruzado: err=%v len=%d", err, len(vs))
	}
	if f, err := UpdateFlowDraft(ctx, pool, botB.ID, flowA.ID, raw, "intruso"); err != nil || f != nil {
		t.Fatalf("UpdateFlowDraft cruzado modificó algo: %v (err=%v)", f, err)
	}
	if f, err := ArchiveFlow(ctx, pool, botB.ID, flowA.ID, "intruso"); err != nil || f != nil {
		t.Fatalf("ArchiveFlow cruzado archivó algo: %v (err=%v)", f, err)
	}
	if res, err := PublishFlow(ctx, pool, botB.ID, flowA.ID, raw, "x", "intruso"); err != nil || res != nil {
		t.Fatalf("PublishFlow cruzado publicó algo: %v (err=%v)", res, err)
	}
	if def, err := PublishedFlowDefinition(ctx, pool, botB.ID, "message"); err != nil || def != nil {
		t.Fatalf("el bot B no debe resolver el flujo del bot A: %s (err=%v)", def, err)
	}
	// El original sigue intacto.
	if got, err := GetFlow(ctx, pool, botA.ID, flowA.ID); err != nil || got == nil || got.ArchivedAt != nil {
		t.Fatalf("el flujo del bot A fue alterado: %+v (err=%v)", got, err)
	}
}


// Pausar deja de resolver el flujo sin perder la versión publicada (§17).
func TestFlowPausarYReactivar(t *testing.T) {
	pool, ctx := flowTestPool(t)
	bot := botDePrueba(t, ctx, pool, "flp_")

	raw := grafoValido("f_p", "Pausable", "hola")
	flow, err := CreateFlow(ctx, pool, bot.ID, NewFlow{
		Key: "pausable", Name: "Pausable", TriggerType: "message", Draft: raw, UserID: "tester",
	})
	if err != nil {
		t.Fatalf("CreateFlow: %v", err)
	}
	pub := publica(t, ctx, pool, bot.ID, flow.ID, raw)

	if _, err := PauseFlow(ctx, pool, bot.ID, flow.ID, "tester"); err != nil {
		t.Fatalf("PauseFlow: %v", err)
	}
	if def, err := PublishedFlowDefinition(ctx, pool, bot.ID, "message"); err != nil || def != nil {
		t.Fatalf("un flujo pausado no debe ejecutarse: err=%v def=%s", err, def)
	}
	resumed, err := ResumeFlow(ctx, pool, bot.ID, flow.ID, "tester")
	if err != nil || resumed == nil {
		t.Fatalf("ResumeFlow: err=%v flow=%v", err, resumed)
	}
	if *resumed.PublishedVersionID != pub.Version.ID {
		t.Fatal("pausar no puede perder la versión publicada")
	}
	if def, err := PublishedFlowDefinition(ctx, pool, bot.ID, "message"); err != nil || def == nil {
		t.Fatalf("tras reactivar debe volver a resolverse: err=%v", err)
	}
}
