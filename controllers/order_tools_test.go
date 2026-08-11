package controllers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// captura conserva lo que se le mandó a la tienda. Lo que **no** aparece en el
// cuerpo es tan importante como lo que aparece.
type captura struct {
	path           string
	body           map[string]any
	idempotencyKey string
}

func tiendaQueCaptura(t *testing.T, destino *captura, responder func(w http.ResponseWriter)) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/store") {
			w.Write([]byte(`{"ok":true,"data":{"id":6,"name":"Tienda","settings":{"currency":"PEN"}}}`))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		destino.path = r.URL.Path
		destino.idempotencyKey = r.Header.Get("Idempotency-Key")
		_ = json.Unmarshal(raw, &destino.body)
		responder(w)
	}
}

func TestOrderCreateEnviaLineasSinPrecioYConIdempotencia(t *testing.T) {
	var got captura
	fixture := setupCatalogFixture(t, tiendaQueCaptura(t, &got, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"ok":true,"data":{"id":101,"order_number":"ORD-1","status":"pending",
			"subtotal":39.8,"discount":0,"total":39.8,"currency":"PEN",
			"items":[{"product_id":56,"product_name":"Pantalla OLED","quantity":2,
			          "unit_price":19.9,"total_price":39.8}]}}`))
	}))

	raw, err := fixture.con.execOrderCreate(context.Background(), fixture.bot, "wamid.abc", map[string]string{
		"connection": "meudim", "customerEmail": "ana@example.com", "customerName": "Ana",
		"item.1.productId": "56", "item.1.quantity": "2",
	})
	if err != nil {
		t.Fatalf("execOrderCreate: %v", err)
	}

	if got.path != "/v1/orders" {
		t.Fatalf("ruta inesperada: %s", got.path)
	}
	// Sin clave explícita, la identidad del intento es el mensaje que lo provocó.
	// Meta reintenta webhooks: sin esto, el reintento cobra dos veces.
	if got.idempotencyKey != "message:wamid.abc" {
		t.Fatalf("Idempotency-Key inesperada: %q", got.idempotencyKey)
	}
	items, _ := got.body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("líneas enviadas: %+v", got.body["items"])
	}
	linea, _ := items[0].(map[string]any)
	if linea["product_id"] != float64(56) || linea["quantity"] != float64(2) {
		t.Fatalf("línea inesperada: %+v", linea)
	}
	// El precio no viaja y no debe empezar a viajar: la tienda lo lee de su
	// catálogo dentro de la transacción y descarta el que se le mande. Enviarlo
	// haría creer que el flujo controla el importe.
	if _, existe := linea["unit_price"]; existe {
		t.Fatal("se envió un precio calculado por el flujo")
	}
	// Sin dirección completa no se manda dirección: Meudim la trata como
	// opcional, pero a medias rechaza el pedido entero.
	if _, existe := got.body["shipping_address"]; existe {
		t.Fatal("se envió una dirección vacía")
	}

	var result orderResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("resultado ilegible: %v", err)
	}
	if result.OrderID != 101 || result.Total != 39.8 || result.ItemCount != 1 {
		t.Fatalf("resultado inesperado: %+v", result)
	}
	// El resumen lo compone el backend con las líneas reales: es lo que permite
	// confirmarle la compra al cliente sin que la IA redacte cantidades.
	if result.Summary != "2 × Pantalla OLED (S/ 39.80)" {
		t.Fatalf("resumen inesperado: %q", result.Summary)
	}
}

func TestOrderCreateEnviaLaDireccionSoloSiEstaCompleta(t *testing.T) {
	var got captura
	fixture := setupCatalogFixture(t, tiendaQueCaptura(t, &got, func(w http.ResponseWriter) {
		w.Write([]byte(`{"ok":true,"data":{"id":102,"status":"pending","currency":"PEN","items":[]}}`))
	}))
	args := map[string]string{
		"connection": "meudim", "customerEmail": "ana@example.com",
		"item.1.productId": "56", "item.1.quantity": "1",
		"shipping.name": "Ana Pérez", "shipping.address": "Av. Siempre Viva 123",
		"shipping.city": "Lima", "shipping.state": "Lima",
		"shipping.postalCode": "15001", "shipping.country": "Perú",
	}
	if _, err := fixture.con.execOrderCreate(context.Background(), fixture.bot, "wamid.dir", args); err != nil {
		t.Fatalf("execOrderCreate: %v", err)
	}
	address, _ := got.body["shipping_address"].(map[string]any)
	if address["city"] != "Lima" || address["postalCode"] != "15001" {
		t.Fatalf("dirección inesperada: %+v", got.body["shipping_address"])
	}

	// Quitar un campo obligatorio la deja incompleta: se omite entera.
	delete(args, "shipping.postalCode")
	got = captura{}
	if _, err := fixture.con.execOrderCreate(context.Background(), fixture.bot, "wamid.dir2", args); err != nil {
		t.Fatalf("execOrderCreate sin código postal: %v", err)
	}
	if _, existe := got.body["shipping_address"]; existe {
		t.Fatal("se envió una dirección incompleta y la tienda habría rechazado el pedido")
	}
}

// Un 400 de la tienda no es un fallo técnico: es «quedan 2». Ese mensaje tiene
// que llegar hasta el flujo para poder decírselo al cliente.
func TestOrderCreateConservaElMensajeDeStock(t *testing.T) {
	fixture := setupCatalogFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"ok":false,"msg":"stock insuficiente para 'Pantalla OLED' (disponible: 2, solicitado: 5)","data":null}`))
	})
	_, err := fixture.con.execOrderCreate(context.Background(), fixture.bot, "wamid.stock", map[string]string{
		"connection": "meudim", "customerEmail": "ana@example.com",
		"item.1.productId": "56", "item.1.quantity": "5",
	})
	if err == nil || !strings.Contains(err.Error(), "disponible: 2") {
		t.Fatalf("el mensaje de la tienda se perdió: %v", err)
	}
}

func TestPaymentIntentComponeElMensajeConLasCuentasDeLaTienda(t *testing.T) {
	fixture := setupCatalogFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"ok":true,"data":{
			"payment":{"id":11,"order_id":101,"provider":"manual","status":"pending",
			           "amount":39.8,"currency":"PEN"},
			"instructions":{"provider":"manual","configured":true,
			  "instructions":"Transfiere el total y declara tu operación.",
			  "accounts":[{"bank":"Yape","holder":"Sistemuino SAC","number":"+51 999 888 777"},
			              {"bank":"BCP","holder":"Sistemuino SAC","number":"191-123","cci":"00219100"}],
			  "qr_urls":[]}}}`))
	})
	raw, err := fixture.con.execPaymentIntentCreate(context.Background(), fixture.bot,
		map[string]string{"connection": "meudim", "orderId": "101"})
	if err != nil {
		t.Fatalf("execPaymentIntentCreate: %v", err)
	}
	var result paymentIntentResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("resultado ilegible: %v", err)
	}
	if result.PaymentID != 11 || result.Amount != 39.8 {
		t.Fatalf("resultado inesperado: %+v", result)
	}
	// El mensaje lo compone el backend con los datos exactos de la tienda. La IA
	// no redacta números de cuenta: mismo criterio que payment_methods_render.
	for _, esperado := range []string{
		"S/ 39.80", "Yape", "+51 999 888 777", "BCP", "191-123", "CCI: 00219100",
		"Transfiere el total", "número de operación",
	} {
		if !strings.Contains(result.Message, esperado) {
			t.Errorf("el mensaje no contiene %q:\n%s", esperado, result.Message)
		}
	}
}

// Una tienda sin datos de pago sale por `error`, no con un mensaje a medias:
// unas instrucciones vacías dejan al cliente sin saber a dónde pagar, y eso es
// peor que derivar a una persona.
func TestPaymentIntentSinConfigurarSalePorError(t *testing.T) {
	fixture := setupCatalogFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"ok":true,"data":{
			"payment":{"id":12,"order_id":101,"status":"pending","amount":39.8,"currency":"PEN"},
			"instructions":{"provider":"manual","configured":false,
			  "warning":"la tienda no configuró settings.payments.manual","accounts":[]}}}`))
	})
	_, err := fixture.con.execPaymentIntentCreate(context.Background(), fixture.bot,
		map[string]string{"connection": "meudim", "orderId": "101"})
	if err == nil || !strings.Contains(err.Error(), "datos de pago") {
		t.Fatalf("esperaba derivar por falta de configuración: %v", err)
	}
}

func TestPaymentSubmitDeclaraLaOperacion(t *testing.T) {
	var got captura
	fixture := setupCatalogFixture(t, tiendaQueCaptura(t, &got, func(w http.ResponseWriter) {
		w.Write([]byte(`{"ok":true,"data":{"id":11,"order_id":101,"status":"submitted",
			"reference":"00123456","amount":39.8,"currency":"PEN"}}`))
	}))
	raw, err := fixture.con.execPaymentSubmit(context.Background(), fixture.bot, map[string]string{
		"connection": "meudim", "paymentId": "11", "reference": "00123456",
	})
	if err != nil {
		t.Fatalf("execPaymentSubmit: %v", err)
	}
	if got.path != "/v1/payments/11/submit" {
		t.Fatalf("ruta inesperada: %s", got.path)
	}
	// Los ceros iniciales del número de operación se conservan: es texto, no un
	// número, y perderlos rompe la conciliación con el extracto del banco.
	if got.body["reference"] != "00123456" {
		t.Fatalf("referencia enviada: %v", got.body["reference"])
	}
	var result paymentSubmitResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("resultado ilegible: %v", err)
	}
	if result.Status != "submitted" || result.Reference != "00123456" {
		t.Fatalf("resultado inesperado: %+v", result)
	}
}

func TestMoneyFormateaLaMoneda(t *testing.T) {
	casos := map[string]string{
		"PEN": "S/ 19.90", "USD": "US$ 19.90", "": "19.90", "EUR": "19.90 EUR",
	}
	for currency, quiere := range casos {
		if got := money(19.9, currency); got != quiere {
			t.Errorf("money(19.9, %q) = %q, esperado %q", currency, got, quiere)
		}
	}
}
