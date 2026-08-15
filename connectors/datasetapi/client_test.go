package datasetapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// El endpoint real todavía no existe, así que estas pruebas corren contra un
// servidor de prueba (httptest), nunca contra la red: es exactamente lo que
// pide el §3 de PLAN-HACKATON-MARCA-BLANCA-Y-MCP.md.

func TestQueryToleraLasTresFormasDeRespuesta(t *testing.T) {
	casos := []struct {
		nombre string
		cuerpo string
	}{
		{"items con total", `{"items":[{"id":1,"nombre":"a"}],"total":1}`},
		{"data con total en metadata (estilo Meudim)", `{"data":[{"id":1,"nombre":"a"}],"metadata":{"total":1}}`},
		{"array desnudo", `[{"id":1,"nombre":"a"}]`},
	}
	for _, caso := range casos {
		t.Run(caso.nombre, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(caso.cuerpo))
			}))
			defer server.Close()

			client, err := New(server.URL, "", nil)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			records, meta, err := client.Query(context.Background(), Query{Resource: "clientes"})
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if len(records) != 1 || records[0]["nombre"] != "a" {
				t.Fatalf("registros inesperados: %+v", records)
			}
			if meta.Total != 1 {
				t.Fatalf("total inesperado: %d", meta.Total)
			}
		})
	}
}

// Cero coincidencias no es un error: la tool que envuelve este cliente
// necesita poder devolver found=false sin pasar por la rama `error`.
func TestQueryCeroCoincidenciasNoEsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"items":[],"total":0}`))
	}))
	defer server.Close()

	client, err := New(server.URL, "", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	records, meta, err := client.Query(context.Background(), Query{Resource: "clientes"})
	if err != nil {
		t.Fatalf("una lista vacía no debe fallar: %v", err)
	}
	if len(records) != 0 || meta.Total != 0 {
		t.Fatalf("se esperaba cero registros: %+v (%+v)", records, meta)
	}
}

func TestQueryFormaDeRespuestaIrreconocibleFalla(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true,"result":"no es una lista"}`))
	}))
	defer server.Close()

	client, err := New(server.URL, "", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, _, err := client.Query(context.Background(), Query{Resource: "clientes"}); err == nil {
		t.Fatal("una respuesta sin items ni data debía fallar, no inventar una lista vacía")
	}
}

// El 5xx y el fallo de red son la rama `error`: la tool debe poder distinguir
// «el dataset no respondió» de «no hay coincidencias».
func TestQueryStatusDeErrorSeReportaComoError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"message":"boom"}`))
	}))
	defer server.Close()

	client, err := New(server.URL, "", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, _, err = client.Query(context.Background(), Query{Resource: "clientes"})
	if err == nil {
		t.Fatal("un 500 no debe tratarse como éxito")
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("se esperaba *Error, llegó: %T", err)
	}
	if apiErr.Message != "boom" || !apiErr.Retryable() {
		t.Fatalf("error inesperado: %+v", apiErr)
	}
}

// Los filtros van como `where.<n>.field/op/value` numerados, no como
// `field:op:value` en un único valor: así un `value` con `:` (una fecha ISO)
// no se confunde con el separador de la condición.
func TestQueryCodificaFiltrosCampoOperadorValorPorSeparado(t *testing.T) {
	var recibida string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recibida = r.URL.RawQuery
		w.Write([]byte(`{"items":[],"total":0}`))
	}))
	defer server.Close()

	client, err := New(server.URL, "clave", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, _, err = client.Query(context.Background(), Query{
		Resource: "cobros",
		Text:     "yape",
		Sort:     "-fecha",
		Limit:    5,
		Fields:   []string{"monto", "fecha"},
		Where: []Filter{
			{Field: "fecha", Op: "gte", Value: "2026-08-01T00:00:00Z"},
		},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	query := mustParseQuery(t, recibida)
	if query.Get("q") != "yape" || query.Get("sort") != "-fecha" || query.Get("limit") != "5" {
		t.Fatalf("query string inesperada: %q", recibida)
	}
	if query.Get("fields") != "monto,fecha" {
		t.Fatalf("fields inesperado: %q", query.Get("fields"))
	}
	if query.Get("where.1.field") != "fecha" || query.Get("where.1.op") != "gte" ||
		query.Get("where.1.value") != "2026-08-01T00:00:00Z" {
		t.Fatalf("condición codificada incorrectamente: %q", recibida)
	}
}

func TestQueryUsaResourceEnLaRuta(t *testing.T) {
	var ruta string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ruta = r.URL.Path
		w.Write([]byte(`{"items":[]}`))
	}))
	defer server.Close()

	client, err := New(server.URL, "", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, _, err := client.Query(context.Background(), Query{Resource: "productos"}); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if ruta != "/productos" {
		t.Fatalf("ruta inesperada: %q", ruta)
	}
}

func TestNewExigeURL(t *testing.T) {
	if _, err := New("", "clave", nil); err == nil {
		t.Fatal("una URL vacía debía rechazarse")
	}
}

func mustParseQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	values, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("query string ilegible %q: %v", raw, err)
	}
	return values
}
