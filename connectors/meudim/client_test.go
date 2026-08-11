package meudim

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// nuevoCliente apunta el cliente a un servidor de prueba. No pasa por
// connectors.ValidateTarget a propósito: esa lista protege lo que se **guarda**
// como conexión, y un httptest en loopback no se guarda en ninguna parte.
func nuevoCliente(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := New(server.URL, "pk_test_clave", server.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client, server
}

func TestSearchProductsImponeStatusActive(t *testing.T) {
	var gotQuery string
	var gotAuth string
	client, _ := nuevoCliente(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery, gotAuth = r.URL.RawQuery, r.Header.Get("Authorization")
		w.Header().Set("X-RateLimit-Remaining", "118")
		w.Write([]byte(`{"ok":true,"msg":"","data":[
			{"id":7,"name":"ESP32 DevKit","slug":"esp32-devkit-v1","price":49.9,
			 "stock_quantity":12,"track_inventory":true,"status":"active"}],
			"metadata":{"total":1,"limit":5,"offset":0}}`))
	})

	products, meta, err := client.SearchProducts(context.Background(), ProductQuery{
		Search: "ESP32", Limit: 5, Sort: "price-asc", MinPrice: 20,
	})
	if err != nil {
		t.Fatalf("SearchProducts: %v", err)
	}
	if len(products) != 1 || products[0].Name != "ESP32 DevKit" || products[0].Price != 49.9 {
		t.Fatalf("productos inesperados: %+v", products)
	}
	if meta.Total != 1 || meta.RateLimitRemaining != 118 {
		t.Fatalf("metadatos inesperados: %+v", meta)
	}
	if gotAuth != "Bearer pk_test_clave" {
		t.Fatalf("autorización inesperada: %q", gotAuth)
	}
	// Sin status=active un borrador a medio escribir del dueño acabaría
	// ofreciéndose a un cliente. Lo impone el cliente, no el llamador.
	for _, esperado := range []string{"status=active", "search=ESP32", "sort_by=price-asc", "limit=5", "min_price=20"} {
		if !strings.Contains(gotQuery, esperado) {
			t.Fatalf("falta %q en la query %q", esperado, gotQuery)
		}
	}
}

// Cero coincidencias no es un fallo: es la respuesta «no lo vendemos», y el
// flujo tiene que poder distinguirla de «no pude preguntar».
func TestListaVaciaNoEsError(t *testing.T) {
	client, _ := nuevoCliente(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true,"msg":"","data":[],"metadata":{"total":0,"limit":20,"offset":0}}`))
	})
	products, meta, err := client.SearchProducts(context.Background(), ProductQuery{Search: "nada"})
	if err != nil {
		t.Fatalf("una lista vacía se trató como error: %v", err)
	}
	if len(products) != 0 || meta.Total != 0 {
		t.Fatalf("esperaba cero productos: %+v", products)
	}
}

func TestErrorConservaElMensajeDeLaTienda(t *testing.T) {
	client, _ := nuevoCliente(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"ok":false,"msg":"stock insuficiente para 'ESP32' (disponible: 2, solicitado: 5)","data":null}`))
	})
	_, _, err := client.SearchProducts(context.Background(), ProductQuery{})
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("esperaba *Error, obtuve %T: %v", err, err)
	}
	if apiErr.Retryable() {
		t.Fatal("un 400 no debe reintentarse: la tienda ya decidió")
	}
	// El mensaje de dominio es lo que permite responder «quedan 2» en vez de
	// «error 400». Si se pierde aquí, no se recupera río arriba.
	if apiErr.Message != "stock insuficiente para 'ESP32' (disponible: 2, solicitado: 5)" {
		t.Fatalf("mensaje perdido: %q", apiErr.Message)
	}
}

// Un 200 con ok:false existe en el contrato de Meudim. Tratarlo como éxito
// devolvería el cero del tipo y el modelo hablaría de un producto sin precio.
func TestEnvelopeOKFalseEsError(t *testing.T) {
	client, _ := nuevoCliente(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":false,"msg":"clave revocada","data":null}`))
	})
	if _, _, err := client.Store(context.Background()); err == nil {
		t.Fatal("un envelope ok:false se aceptó como éxito")
	}
}

func TestRateLimitConservaRetryAfter(t *testing.T) {
	intentos := 0
	client, _ := nuevoCliente(t, func(w http.ResponseWriter, r *http.Request) {
		intentos++
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"ok":false,"msg":"rate limit","data":null}`))
	})
	_, _, err := client.SearchProducts(context.Background(), ProductQuery{})
	var apiErr *Error
	if !errors.As(err, &apiErr) || !apiErr.RateLimited() {
		t.Fatalf("esperaba un 429 clasificado: %v", err)
	}
	if apiErr.RetryAfter != 12*time.Second {
		t.Fatalf("Retry-After perdido: %v", apiErr.RetryAfter)
	}
	// Un 429 no se reintenta: reintentar es justo lo que agota el cubo.
	if intentos != 1 {
		t.Fatalf("el 429 se reintentó %d veces", intentos)
	}
}

func TestGetSeReintentaUnaVezAnteUn500(t *testing.T) {
	intentos := 0
	client, _ := nuevoCliente(t, func(w http.ResponseWriter, r *http.Request) {
		intentos++
		if intentos == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"ok":false,"msg":"boom","data":null}`))
			return
		}
		w.Write([]byte(`{"ok":true,"msg":"","data":{"id":42,"name":"Sistemuino"}}`))
	})
	store, _, err := client.Store(context.Background())
	if err != nil {
		t.Fatalf("el reintento no ocurrió: %v", err)
	}
	if intentos != 2 || store.Name != "Sistemuino" {
		t.Fatalf("intentos=%d store=%+v", intentos, store)
	}
}

// La regla que evita cobrar dos veces al mismo cliente: un POST sin
// Idempotency-Key no se repite, porque el primero pudo haber creado la orden
// aunque no llegara la respuesta.
func TestPostSoloSeReintentaConIdempotencyKey(t *testing.T) {
	casos := []struct {
		nombre           string
		clave            string
		intentosEsperado int
	}{
		{"sin clave no se repite", "", 1},
		{"con clave se repite", "bawto:org:wamid.1", 2},
	}
	for _, caso := range casos {
		t.Run(caso.nombre, func(t *testing.T) {
			intentos := 0
			var gotKey string
			client, _ := nuevoCliente(t, func(w http.ResponseWriter, r *http.Request) {
				intentos++
				gotKey = r.Header.Get("Idempotency-Key")
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`{"ok":false,"msg":"boom","data":null}`))
			})
			_, err := client.do(context.Background(), request{
				method: http.MethodPost, path: "/v1/orders",
				body: map[string]any{"items": []any{}}, idempotencyKey: caso.clave,
			}, nil)
			if err == nil {
				t.Fatal("esperaba error")
			}
			if intentos != caso.intentosEsperado {
				t.Fatalf("intentos=%d, esperado=%d", intentos, caso.intentosEsperado)
			}
			if gotKey != caso.clave {
				t.Fatalf("Idempotency-Key=%q, esperado=%q", gotKey, caso.clave)
			}
		})
	}
}

func TestSinRespuestaEsUnreachable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	client, err := New(server.URL, "pk_test_clave", server.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	server.Close() // nadie escucha: no hay respuesta que clasificar

	_, _, err = client.Store(context.Background())
	var apiErr *Error
	if !errors.As(err, &apiErr) || !apiErr.Unreachable || !apiErr.Retryable() {
		t.Fatalf("esperaba un fallo de conexión reintentable: %v", err)
	}
}

func TestProductoDetalleTraeImagenes(t *testing.T) {
	client, _ := nuevoCliente(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/products/slug/esp32-devkit-v1" {
			t.Errorf("ruta inesperada: %s", r.URL.Path)
		}
		w.Write([]byte(`{"ok":true,"msg":"","data":{"id":7,"name":"ESP32","slug":"esp32-devkit-v1",
			"price":49.9,"status":"active","track_inventory":true,"stock_quantity":3,
			"images":[{"url":"https://cdn/x.jpg","alt_text":"frente"}],
			"specifications":{"wifi":"2.4GHz"}}}`))
	})
	product, _, err := client.ProductBySlug(context.Background(), "esp32-devkit-v1")
	if err != nil {
		t.Fatalf("ProductBySlug: %v", err)
	}
	if len(product.Images) != 1 || product.Images[0].URL != "https://cdn/x.jpg" {
		t.Fatalf("imágenes perdidas: %+v", product.Images)
	}
	if string(product.Specifications) != `{"wifi":"2.4GHz"}` {
		t.Fatalf("especificaciones perdidas: %s", product.Specifications)
	}
}

// Un servicio sin control de inventario tiene stock 0 y se vende igual. Tratar
// ese 0 como agotado escondería medio catálogo sin un solo error.
func TestAvailableIgnoraElStockSinInventario(t *testing.T) {
	casos := []struct {
		nombre  string
		product Product
		quiere  bool
	}{
		{"activo con stock", Product{Status: "active", TrackInventory: true, StockQuantity: 3}, true},
		{"activo sin stock", Product{Status: "active", TrackInventory: true}, false},
		{"activo sin inventario", Product{Status: "active"}, true},
		{"borrador", Product{Status: "draft", StockQuantity: 9}, false},
	}
	for _, caso := range casos {
		if got := caso.product.Available(); got != caso.quiere {
			t.Errorf("%s: Available()=%v, esperado=%v", caso.nombre, got, caso.quiere)
		}
	}
}

func TestNewExigeCredencialYURL(t *testing.T) {
	if _, err := New("https://api.meud.im", "  ", nil); err == nil {
		t.Fatal("se aceptó una credencial vacía")
	}
	if _, err := New("", "pk_test", nil); err == nil {
		t.Fatal("se aceptó una URL vacía")
	}
}
