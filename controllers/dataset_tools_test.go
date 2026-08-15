package controllers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Yzx7/sacs-chatbots/connectors"
	"github.com/Yzx7/sacs-chatbots/connectors/datasetapi"
	"github.com/Yzx7/sacs-chatbots/env"
	"github.com/Yzx7/sacs-chatbots/helpers"
	"github.com/Yzx7/sacs-chatbots/models"
)

// --- Pruebas puras: no requieren PostgreSQL ---

// Un campo declarado que el registro no trae sale nulo, no ausente — la misma
// regla dura de data_query (CLAUDE.md §10). Con `fields` vacío se devuelve
// todo lo que entregó el dataset, porque el autor no acotó nada.
func TestProjectDatasetFieldsCampoAusenteSaleNulo(t *testing.T) {
	record := datasetapi.Record{"nombre": "Ana", "saldo": 120.5}

	proyectado := projectDatasetFields(record, []string{"nombre", "telefono"})
	if proyectado["nombre"] != "Ana" {
		t.Fatalf("nombre inesperado: %v", proyectado["nombre"])
	}
	if valor, existe := proyectado["telefono"]; !existe || valor != nil {
		t.Fatalf("un campo ausente en el registro debe salir nulo, no faltar: %v (existe=%v)", valor, existe)
	}
	if _, existe := proyectado["saldo"]; existe {
		t.Fatalf("fields acota la proyección: saldo no debía aparecer")
	}

	completo := projectDatasetFields(record, nil)
	if completo["saldo"] != 120.5 || completo["nombre"] != "Ana" {
		t.Fatalf("sin fields se esperaba el registro completo: %+v", completo)
	}
}

func TestDatasetRecordIDUsaLaClaveIdSiExiste(t *testing.T) {
	if id := datasetRecordID(datasetapi.Record{"id": float64(7), "nombre": "x"}); id != "7" {
		t.Fatalf("id inesperado: %q", id)
	}
	if id := datasetRecordID(datasetapi.Record{"nombre": "sin id"}); id != "" {
		t.Fatalf("sin clave id se esperaba vacío, no inventar uno: %q", id)
	}
}

// El fallo nunca debe leerse como «no hay datos»: es justo lo que distingue la
// rama `error` de found=false, y es la instrucción que evita que el modelo
// afirme la ausencia de información por una caída transitoria.
func TestDatasetFailureForModelNuncaAfirmaAusenciaDeDatos(t *testing.T) {
	casos := []error{
		errCatalogBudget,
		&datasetapi.Error{StatusCode: 429},
		&datasetapi.Error{Unreachable: true, Message: "dial tcp"},
		&datasetapi.Error{StatusCode: 500, Message: "boom"},
	}
	for _, err := range casos {
		mensaje := datasetFailureForModel(err)
		if mensaje == "" {
			t.Fatalf("sin mensaje para %v", err)
		}
		if strings.Contains(strings.ToLower(mensaje), "no hay información registrada") {
			t.Errorf("el mensaje de fallo afirma ausencia de datos (%v): %q", err, mensaje)
		}
	}
}

// --- Integración: conexión real en PostgreSQL contra un dataset simulado ---

type datasetFixture struct {
	con      *Controller
	bot      *models.BotChannel
	requests *int
}

func setupDatasetFixture(t *testing.T, handler http.HandlerFunc) datasetFixture {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL no seteada; se omite el test de integración")
	}
	resetCatalogCache()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		handler(w, r)
	}))
	t.Cleanup(server.Close)

	key := make([]byte, 32)
	_, _ = rand.Read(key)
	cipher, _ := helpers.NewCipher(hex.EncodeToString(key))
	con := New(&env.Env{
		Postgres:       pool,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		WhatsAppLogger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Cipher:         cipher,
	})

	uid := randHex("dtu_")
	if _, err := pool.Exec(ctx, `INSERT INTO "user"(id,name,email,"emailVerified") VALUES ($1,$2,$3,false)`,
		uid, "Dataset Owner", uid+"@test.local"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, uid) })

	org, err := models.CreateOrganization(ctx, pool, uid, "Dataset Org", nil, nil)
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	t.Cleanup(func() { models.DeleteOrganization(context.Background(), pool, org.ID) })

	// La URL apunta al servidor de prueba y no pasa por connectors.ValidateTarget:
	// esa lista protege lo que un administrador puede guardar desde el panel, no
	// lo que construye una prueba del ejecutor (mismo criterio que catalog_tools_test.go).
	credential, err := cipher.Encrypt("token-de-prueba")
	if err != nil {
		t.Fatalf("cifrar: %v", err)
	}
	if _, err := models.SaveExternalConnection(ctx, pool, models.ExternalConnectionInput{
		OrgID: org.ID, Key: "erp", Driver: connectors.DriverDatasetAPI,
		Label: "Dataset de prueba", BaseURL: server.URL, CredentialEnc: credential,
	}); err != nil {
		t.Fatalf("SaveExternalConnection: %v", err)
	}
	return datasetFixture{
		con:      con,
		bot:      &models.BotChannel{ID: randHex("bot_"), OrgID: org.ID},
		requests: &requests,
	}
}

func TestDatasetQueryDesdeElGrafo(t *testing.T) {
	fixture := setupDatasetFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/clientes" {
			t.Errorf("ruta inesperada: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token-de-prueba" {
			t.Errorf("credencial no llegó a la petición: %q", got)
		}
		w.Write([]byte(`{"items":[{"id":1,"nombre":"Ana","saldo":120.5}],"total":1}`))
	})

	raw, err := fixture.con.execDatasetQuery(context.Background(), fixture.bot, map[string]string{
		"connection": "erp", "resource": "clientes", "fields": "nombre,saldo",
	})
	if err != nil {
		t.Fatalf("execDatasetQuery: %v", err)
	}
	var result datasetResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("resultado ilegible: %v", err)
	}
	if !result.Found || result.Count != 1 || result.First == nil {
		t.Fatalf("resultado inesperado: %+v", result)
	}
	if result.First.RecordID != "1" || result.First.Data["nombre"] != "Ana" {
		t.Fatalf("primer registro inesperado: %+v", result.First)
	}
	if *fixture.requests != 1 {
		t.Fatalf("peticiones inesperadas: %d", *fixture.requests)
	}
}

// Cero coincidencias es `ok` con found=false: no puede salir por la rama
// `error`, o un Router no podría distinguir «no hay datos» de «el dataset no
// respondió» (CLAUDE.md §10).
func TestDatasetQuerySinCoincidenciasEsFoundFalse(t *testing.T) {
	fixture := setupDatasetFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"items":[],"total":0}`))
	})
	raw, err := fixture.con.execDatasetQuery(context.Background(), fixture.bot,
		map[string]string{"connection": "erp", "resource": "clientes"})
	if err != nil {
		t.Fatalf("una lista vacía no debe fallar: %v", err)
	}
	var result datasetResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("resultado ilegible: %v", err)
	}
	if result.Found || result.Count != 0 || result.First != nil {
		t.Fatalf("se esperaba found=false: %+v", result)
	}
}

// El dataset caído sí es la rama `error` del grafo.
func TestDatasetQueryCaidoSalePorError(t *testing.T) {
	fixture := setupDatasetFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"message":"boom"}`))
	})
	if _, err := fixture.con.execDatasetQuery(context.Background(), fixture.bot,
		map[string]string{"connection": "erp", "resource": "clientes"}); err == nil {
		t.Fatal("un dataset caído no debe salir por ok")
	}
}

// Las condiciones numeradas del bloque viajan como filtros al cliente HTTP, en
// el mismo orden en que el autor las declaró.
func TestDatasetQueryPropagaCondicionesNumeradas(t *testing.T) {
	var recibida string
	fixture := setupDatasetFixture(t, func(w http.ResponseWriter, r *http.Request) {
		recibida = r.URL.RawQuery
		w.Write([]byte(`{"items":[],"total":0}`))
	})
	if _, err := fixture.con.execDatasetQuery(context.Background(), fixture.bot, map[string]string{
		"connection": "erp", "resource": "clientes",
		"where.1.field": "documento", "where.1.op": "eq", "where.1.value": "12345678",
	}); err != nil {
		t.Fatalf("execDatasetQuery: %v", err)
	}
	if !strings.Contains(recibida, "where.1.field=documento") || !strings.Contains(recibida, "where.1.value=12345678") {
		t.Fatalf("condición no propagada: %q", recibida)
	}
}

// El agente solo puede filtrar por lo que el autor listó en filterFields —el
// mismo comportamiento que execAgentDataQuery: un campo fuera de la lista no
// es un error técnico, es una explicación que el modelo puede usar.
func TestDatasetQueryDesdeElAgenteRespetaFilterFields(t *testing.T) {
	fixture := setupDatasetFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"items":[{"id":1,"nombre":"Ana"}],"total":1}`))
	})
	ctx := context.Background()
	budget := newCatalogBudget()
	config := map[string]string{"connection": "erp", "resource": "clientes", "filterFields": "nombre"}

	args, _ := json.Marshal(map[string]any{
		"filters": []map[string]string{{"field": "saldo", "op": "eq", "value": "100"}},
	})
	out, err := fixture.con.execAgentDatasetQuery(ctx, fixture.bot, budget, config, args)
	if err != nil {
		t.Fatalf("no debía reventar el nodo: %v", err)
	}
	if !strings.Contains(out, "No puedes filtrar por") {
		t.Fatalf("se esperaba el rechazo explicado, llegó: %q", out)
	}
	if *fixture.requests != 0 {
		t.Fatalf("un filtro no permitido no debía alcanzar la red: %d peticiones", *fixture.requests)
	}

	args, _ = json.Marshal(map[string]any{
		"filters": []map[string]string{{"field": "nombre", "op": "contains", "value": "an"}},
	})
	out, err = fixture.con.execAgentDatasetQuery(ctx, fixture.bot, budget, config, args)
	if err != nil {
		t.Fatalf("búsqueda del agente: %v", err)
	}
	if !strings.Contains(out, "Ana") {
		t.Fatalf("se esperaba el resultado en el texto: %q", out)
	}
	if budget.remaining != maxCatalogCallsPerTurn-1 {
		t.Fatalf("la búsqueda válida debía consumir presupuesto: quedan %d", budget.remaining)
	}
}

// El presupuesto por turno es compartido con catálogo y lo consume solo el
// agente, nunca el grafo — igual que TestCatalogPresupuestoSoloLoConsumeElAgente.
func TestDatasetQueryPresupuestoSoloLoConsumeElAgente(t *testing.T) {
	fixture := setupDatasetFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"items":[{"id":1}],"total":1}`))
	})
	ctx := context.Background()
	budget := newCatalogBudget()
	budget.remaining = 0
	config := map[string]string{"connection": "erp", "resource": "clientes"}

	args, _ := json.Marshal(map[string]any{"query": "algo"})
	out, err := fixture.con.execAgentDatasetQuery(ctx, fixture.bot, budget, config, args)
	if err != nil {
		t.Fatalf("el presupuesto agotado no debía reventar el nodo: %v", err)
	}
	if !strings.Contains(out, "demasiadas veces") {
		t.Fatalf("mensaje inesperado al agotar el presupuesto: %q", out)
	}

	// El grafo sigue pudiendo consultar aunque el modelo se haya pasado.
	if _, err := fixture.con.execDatasetQuery(ctx, fixture.bot,
		map[string]string{"connection": "erp", "resource": "clientes"}); err != nil {
		t.Fatalf("el presupuesto del modelo bloqueó un bloque del grafo: %v", err)
	}
}

// El aislamiento entre organizaciones: la conexión se resuelve por la
// organización del bot.
func TestDatasetQueryNoCruzaOrganizaciones(t *testing.T) {
	fixture := setupDatasetFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("se llamó al dataset de otra organización")
	})
	ajeno := &models.BotChannel{ID: randHex("bot_"), OrgID: "00000000-0000-0000-0000-000000000000"}
	_, err := fixture.con.execDatasetQuery(context.Background(), ajeno,
		map[string]string{"connection": "erp", "resource": "clientes"})
	if err == nil || !strings.Contains(err.Error(), "no tiene una conexión") {
		t.Fatalf("esperaba que la conexión no resolviera para otra organización: %v", err)
	}
}
