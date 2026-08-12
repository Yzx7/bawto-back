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
	"github.com/Yzx7/sacs-chatbots/connectors/meudim"
	"github.com/Yzx7/sacs-chatbots/env"
	"github.com/Yzx7/sacs-chatbots/helpers"
	"github.com/Yzx7/sacs-chatbots/models"
)

func TestCatalogBudgetAcotaElTurno(t *testing.T) {
	budget := newCatalogBudget()
	for i := 0; i < maxCatalogCallsPerTurn; i++ {
		if err := budget.consume(); err != nil {
			t.Fatalf("la llamada %d se rechazó dentro del presupuesto: %v", i+1, err)
		}
	}
	if err := budget.consume(); err == nil {
		t.Fatal("el presupuesto no cortó la llamada sobrante")
	}
	// Un bloque sin presupuesto (por ejemplo una prueba del grafo) no debe
	// quedarse sin poder consultar.
	var sinPresupuesto *catalogBudget
	if err := sinPresupuesto.consume(); err != nil {
		t.Fatalf("un presupuesto nil debe permitir la llamada: %v", err)
	}
}

func TestProductDataProyectaLasClavesDelFlujo(t *testing.T) {
	product := meudim.Product{
		ID: 56, Name: "Pantalla OLED", Slug: "pantalla-oled-096-i2c", SKU: "OLED-096",
		Price: 19.9, StockQuantity: 10, TrackInventory: true, Status: "active",
		CategoryName: "Pantallas", ShortDescription: "0.96 pulgadas I2C",
		MainImageURL: "https://cdn/oled.jpg",
	}
	data := productData(product, "PEN", "https://sistemuino.com/productos/{slug}")

	esperado := map[string]any{
		"id": int64(56), "nombre": "Pantalla OLED", "precio": 19.9, "stock": 10,
		"disponible": true, "moneda": "PEN", "categoria": "Pantallas",
		"url": "https://sistemuino.com/productos/pantalla-oled-096-i2c",
	}
	for clave, quiere := range esperado {
		if data[clave] != quiere {
			t.Errorf("%s = %v, esperado %v", clave, data[clave], quiere)
		}
	}
}

// Sin plantilla no se compone una URL: la API no conoce las rutas del
// storefront, y adivinarlas le daría al cliente un enlace roto.
func TestProductURLSinPlantillaNoInventaEnlace(t *testing.T) {
	product := meudim.Product{ID: 7, Slug: "esp32"}
	if url := productURL("", product); url != "" {
		t.Fatalf("se inventó un enlace: %q", url)
	}
	if url := productURL("https://tienda.com/p/{id}", product); url != "https://tienda.com/p/7" {
		t.Fatalf("plantilla por id: %q", url)
	}
}

// La propiedad de seguridad del fallo: cuando no se puede consultar, el modelo
// tiene que entender que **no sabe**, no que el producto no existe. Un texto que
// se preste a lo segundo convierte una caída de la tienda en «no lo vendemos».
func TestCatalogFailureForModelNuncaNiegaElProducto(t *testing.T) {
	casos := []error{
		errCatalogBudget,
		&meudim.Error{StatusCode: 429, Message: "rate limit"},
		&meudim.Error{Unreachable: true, Message: "dial tcp"},
		&meudim.Error{StatusCode: 500, Message: "boom"},
	}
	for _, err := range casos {
		mensaje := catalogFailureForModel(err)
		if mensaje == "" {
			t.Fatalf("sin mensaje para %v", err)
		}
		for _, prohibido := range []string{"no existe el producto", "no lo vendemos", "sin resultados"} {
			if strings.Contains(strings.ToLower(mensaje), prohibido) {
				t.Errorf("el mensaje de fallo niega el producto (%v): %q", err, mensaje)
			}
		}
	}
	// El presupuesto agotado es el único caso donde no hay que ofrecer asesor:
	// hubo datos, simplemente no se puede volver a buscar.
	if strings.Contains(catalogFailureForModel(errCatalogBudget), "asesor") {
		t.Error("el presupuesto agotado no debe derivar: ya se obtuvieron resultados")
	}
}

// --- Integración: conexión real en PostgreSQL contra una tienda simulada ---

type catalogFixture struct {
	con      *Controller
	bot      *models.BotChannel
	requests *int
}

func setupCatalogFixture(t *testing.T, handler http.HandlerFunc) catalogFixture {
	return setupCatalogFixtureWithCredential(t, handler, "sk_test_clave")
}

func setupCatalogFixtureWithCredential(t *testing.T, handler http.HandlerFunc, plainCredential string) catalogFixture {
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

	uid := randHex("catu_")
	if _, err := pool.Exec(ctx, `INSERT INTO "user"(id,name,email,"emailVerified") VALUES ($1,$2,$3,false)`,
		uid, "Catalog Owner", uid+"@test.local"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, uid) })

	org, err := models.CreateOrganization(ctx, pool, uid, "Catalog Org", nil, nil)
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	t.Cleanup(func() { models.DeleteOrganization(context.Background(), pool, org.ID) })

	// La URL apunta al servidor de prueba y no pasa por connectors.ValidateTarget.
	// Es deliberado: esa lista protege lo que un administrador puede **guardar**
	// desde el panel, no lo que construye una prueba del ejecutor.
	credential, err := cipher.Encrypt(plainCredential)
	if err != nil {
		t.Fatalf("cifrar: %v", err)
	}
	if _, err := models.SaveExternalConnection(ctx, pool, models.ExternalConnectionInput{
		OrgID: org.ID, Key: "meudim", Driver: connectors.DriverMeudim,
		Label: "Tienda de prueba", BaseURL: server.URL, CredentialEnc: credential,
	}); err != nil {
		t.Fatalf("SaveExternalConnection: %v", err)
	}
	return catalogFixture{
		con:      con,
		bot:      &models.BotChannel{ID: randHex("bot_"), OrgID: org.ID},
		requests: &requests,
	}
}

func TestCatalogConnectionRechazaClavePublicableGuardada(t *testing.T) {
	fixture := setupCatalogFixtureWithCredential(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("una credencial publicable no debe alcanzar la red")
	}, "pk_test_legada")

	_, _, err := fixture.con.catalogConnection(context.Background(), fixture.bot, "meudim")
	if err == nil || !strings.Contains(err.Error(), "clave secreta") {
		t.Fatalf("se esperaba rechazo explícito de pk_, llegó: %v", err)
	}
	if *fixture.requests != 0 {
		t.Fatalf("la conexión inválida alcanzó la red: %d peticiones", *fixture.requests)
	}
}

func TestCatalogSearchDesdeElGrafo(t *testing.T) {
	fixture := setupCatalogFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/store"):
			w.Write([]byte(`{"ok":true,"data":{"id":6,"name":"Tienda","settings":{"currency":"PEN"}}}`))
		default:
			if !strings.Contains(r.URL.RawQuery, "status=active") {
				t.Errorf("la búsqueda no impuso status=active: %q", r.URL.RawQuery)
			}
			w.Write([]byte(`{"ok":true,"data":[
				{"id":56,"name":"Pantalla OLED","slug":"pantalla-oled","price":19.9,
				 "stock_quantity":10,"track_inventory":true,"status":"active"}],
				"metadata":{"total":1}}`))
		}
	})

	raw, err := fixture.con.execCatalogSearch(context.Background(), fixture.bot, map[string]string{
		"connection": "meudim", "query": "oled", "urlTemplate": "https://tienda.com/productos/{slug}",
	})
	if err != nil {
		t.Fatalf("execCatalogSearch: %v", err)
	}
	var result catalogResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("resultado ilegible: %v", err)
	}
	if !result.Found || result.Count != 1 || result.First == nil {
		t.Fatalf("resultado inesperado: %+v", result)
	}
	if result.First.RecordID != "56" || result.First.Data["nombre"] != "Pantalla OLED" {
		t.Fatalf("primer registro inesperado: %+v", result.First)
	}
	if result.First.Data["moneda"] != "PEN" {
		t.Fatalf("la moneda no llegó desde la tienda: %+v", result.First.Data)
	}
	if result.First.Data["url"] != "https://tienda.com/productos/pantalla-oled" {
		t.Fatalf("enlace inesperado: %v", result.First.Data["url"])
	}

	// Dos peticiones: el catálogo y la tienda (la moneda).
	peticionesTrasLaPrimera := *fixture.requests
	if peticionesTrasLaPrimera != 2 {
		t.Fatalf("peticiones tras la primera búsqueda: %d", peticionesTrasLaPrimera)
	}
	// La misma búsqueda dentro del turno no vuelve a salir a la red.
	if _, err := fixture.con.execCatalogSearch(context.Background(), fixture.bot, map[string]string{
		"connection": "meudim", "query": "oled", "urlTemplate": "https://tienda.com/productos/{slug}",
	}); err != nil {
		t.Fatalf("segunda búsqueda: %v", err)
	}
	if *fixture.requests != peticionesTrasLaPrimera {
		t.Fatalf("la caché no evitó la segunda llamada: %d peticiones", *fixture.requests)
	}
}

// Quien decide gastar es quien paga: el presupuesto acota lo que pide el modelo.
//
// Un bloque del grafo no lo consume —lo puso el autor y el grafo fija cuántas
// veces corre—, y compartirlo tenía una consecuencia inaceptable: el cliente
// confirmaba la compra y el pedido fallaba porque el agente había buscado de
// más. La consulta de la moneda tampoco consume: no la pide el modelo.
func TestCatalogPresupuestoSoloLoConsumeElAgente(t *testing.T) {
	fixture := setupCatalogFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/store") {
			w.Write([]byte(`{"ok":true,"data":{"id":6,"name":"Tienda","settings":{"currency":"PEN"}}}`))
			return
		}
		w.Write([]byte(`{"ok":true,"data":[{"id":56,"name":"Pantalla OLED","slug":"oled",
			"price":19.9,"stock_quantity":10,"track_inventory":true,"status":"active"}],
			"metadata":{"total":1}}`))
	})
	ctx := context.Background()
	budget := newCatalogBudget()
	config := map[string]string{"connection": "meudim"}

	args, _ := json.Marshal(map[string]any{"query": "oled"})
	if _, err := fixture.con.execAgentCatalogSearch(ctx, fixture.bot, budget, config, args); err != nil {
		t.Fatalf("búsqueda del agente: %v", err)
	}
	// Dos peticiones —catálogo y tienda—, pero solo la del modelo se cobra.
	if *fixture.requests != 2 {
		t.Fatalf("peticiones: %d", *fixture.requests)
	}
	if budget.remaining != maxCatalogCallsPerTurn-1 {
		t.Fatalf("la consulta de la tienda consumió presupuesto: quedan %d", budget.remaining)
	}

	// Un acierto de caché no consume.
	args, _ = json.Marshal(map[string]any{"query": "oled"})
	if _, err := fixture.con.execAgentCatalogSearch(ctx, fixture.bot, budget, config, args); err != nil {
		t.Fatalf("segunda búsqueda del agente: %v", err)
	}
	if budget.remaining != maxCatalogCallsPerTurn-1 {
		t.Fatalf("un acierto de caché consumió presupuesto: quedan %d", budget.remaining)
	}

	// Agotado el presupuesto, el modelo recibe una instrucción, no un error: el
	// nodo no revienta y el bot responde con lo que ya obtuvo.
	budget.remaining = 0
	args, _ = json.Marshal(map[string]any{"query": "otra cosa"})
	out, err := fixture.con.execAgentCatalogSearch(ctx, fixture.bot, budget, config, args)
	if err != nil {
		t.Fatalf("el presupuesto agotado reventó el nodo: %v", err)
	}
	if !strings.Contains(out, "demasiadas veces") {
		t.Fatalf("mensaje inesperado al agotar el presupuesto: %q", out)
	}

	// Y el grafo sigue pudiendo consultar aunque el modelo se haya pasado.
	if _, err := fixture.con.execCatalogSearch(ctx, fixture.bot, map[string]string{
		"connection": "meudim", "query": "otra cosa",
	}); err != nil {
		t.Fatalf("el presupuesto del modelo bloqueó un bloque del grafo: %v", err)
	}
}

// Una consulta que no encuentra nada sale por `ok` con found=false. Si saliera
// por `error`, el Router no podría distinguir «no lo vendemos» de «la tienda no
// responde», que es justo la decisión que tiene que tomar.
func TestCatalogProductInexistenteEsFoundFalse(t *testing.T) {
	fixture := setupCatalogFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"ok":false,"msg":"Producto no encontrado","data":null}`))
	})
	raw, err := fixture.con.execCatalogProduct(context.Background(), fixture.bot,
		map[string]string{"connection": "meudim", "slug": "no-existe"})
	if err != nil {
		t.Fatalf("un 404 se trató como fallo técnico: %v", err)
	}
	var result catalogResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("resultado ilegible: %v", err)
	}
	if result.Found || result.Count != 0 {
		t.Fatalf("esperaba found=false: %+v", result)
	}
}

// La tienda caída sí es la rama `error` del grafo: es lo que permite derivar en
// vez de decir que el producto no existe.
func TestCatalogTiendaCaidaSalePorError(t *testing.T) {
	fixture := setupCatalogFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"ok":false,"msg":"boom","data":null}`))
	})
	if _, err := fixture.con.execCatalogSearch(context.Background(), fixture.bot,
		map[string]string{"connection": "meudim", "query": "oled"}); err == nil {
		t.Fatal("una tienda caída no debe salir por ok")
	}
}

// El aislamiento entre organizaciones: la conexión se resuelve por la
// organización del bot, así que un bot de otra empresa no alcanza esta tienda
// aunque use la misma clave técnica.
func TestCatalogConexionNoCruzaOrganizaciones(t *testing.T) {
	fixture := setupCatalogFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("se llamó a la tienda de otra organización")
	})
	ajeno := &models.BotChannel{ID: randHex("bot_"), OrgID: "00000000-0000-0000-0000-000000000000"}
	_, err := fixture.con.execCatalogSearch(context.Background(), ajeno,
		map[string]string{"connection": "meudim", "query": "oled"})
	if err == nil || !strings.Contains(err.Error(), "no tiene una conexión") {
		t.Fatalf("esperaba que la conexión no resolviera para otra organización: %v", err)
	}
}
