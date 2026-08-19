package meudprov

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func clienteContra(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := New(server.URL, "clave-de-maquina", server.Client())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func responder(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// Las dos cabeceras son el contrato entero de la ruta: sin `X-Provision-Key`
// MEUD responde 401, y sin `Idempotency-Key` responde 400 —y si se mandara una
// distinta en cada intento, dos tiendas—. Se comprueba que salen, porque un
// header que se olvida no rompe nada aquí, rompe allá.
func TestCreateStoreMandaLaClaveYLaIdempotencia(t *testing.T) {
	var gotKey, gotIdem, gotBody string
	client := clienteContra(t, func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Provision-Key")
		gotIdem = r.Header.Get("Idempotency-Key")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		responder(w, http.StatusCreated,
			`{"ok":true,"msg":"creada","data":{"storeId":42,"created":true,"sk":"sk_live_abc"}}`)
	})

	store, err := client.CreateStore(context.Background(), "Panadería Rosa", "bawto:org:u-1")
	if err != nil {
		t.Fatalf("CreateStore: %v", err)
	}
	if gotKey != "clave-de-maquina" {
		t.Errorf("X-Provision-Key = %q", gotKey)
	}
	if gotIdem != "bawto:org:u-1" {
		t.Errorf("Idempotency-Key = %q", gotIdem)
	}
	if store.ID != 42 || !store.Created || store.SK != "sk_live_abc" {
		t.Errorf("tienda = %+v", *store)
	}
	// El dueño lo fija MEUD desde la credencial. Mandarlo aquí permitiría crear
	// tiendas a nombre de cualquiera a quien tuviera la clave.
	var body map[string]any
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("cuerpo ilegible %q: %v", gotBody, err)
	}
	if _, hay := body["owner"]; hay {
		t.Error("el cuerpo lleva dueño")
	}
	if _, hay := body["email"]; hay {
		t.Error("el cuerpo lleva un correo de cliente")
	}
}

// Sin clave de idempotencia no se llama siquiera: el fallo que evita —dos
// tiendas y un bot apuntando a la vacía— es caro y silencioso, así que se
// rechaza aquí y no se confía en que MEUD devuelva 400.
func TestCreateStoreExigeIdempotencia(t *testing.T) {
	llamado := false
	client := clienteContra(t, func(w http.ResponseWriter, r *http.Request) {
		llamado = true
		responder(w, http.StatusCreated, `{"ok":true,"data":{"storeId":1,"created":true,"sk":"sk_x"}}`)
	})
	if _, err := client.CreateStore(context.Background(), "Tienda", "  "); err == nil {
		t.Fatal("se aceptó crear una tienda sin Idempotency-Key")
	}
	if llamado {
		t.Error("se llegó a llamar a MEUD")
	}
}

// El reintento idempotente responde 200 con `sk: null`, y eso **no** es un
// fallo: la tienda existe. Quien llama tiene que poder distinguirlo, porque la
// clave ya no vuelve nunca y de ahí sale el único caso que se resuelve a mano.
func TestCreateStoreReintentoIdempotenteNoTraeClave(t *testing.T) {
	client := clienteContra(t, func(w http.ResponseWriter, r *http.Request) {
		responder(w, http.StatusOK,
			`{"ok":true,"msg":"La tienda ya existía","data":{"storeId":42,"created":false,"sk":null}}`)
	})
	store, err := client.CreateStore(context.Background(), "Panadería Rosa", "bawto:org:u-1")
	if err != nil {
		t.Fatalf("CreateStore: %v", err)
	}
	if store.ID != 42 || store.Created || store.SK != "" {
		t.Fatalf("tienda = %+v; se esperaba la existente y sin clave", *store)
	}
}

// El 404 de admins es un mensaje escrito para leérselo al cliente. Aplanarlo a
// «no se pudo» perdería justo lo único accionable: que cree su cuenta primero.
func TestAddAdminConservaElMensajeDelCorreoSinCuenta(t *testing.T) {
	client := clienteContra(t, func(w http.ResponseWriter, r *http.Request) {
		responder(w, http.StatusNotFound,
			`{"ok":false,"msg":"No existe un usuario registrado con ese correo. Pídele que cree su cuenta primero."}`)
	})
	already, err := client.AddAdmin(context.Background(), 42, "cliente@ejemplo.com")
	if already {
		t.Error("un correo sin cuenta no puede contar como miembro")
	}
	apiErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("error = %v; se esperaba *meudprov.Error", err)
	}
	if !apiErr.NotFound() {
		t.Errorf("status = %d", apiErr.StatusCode)
	}
	if apiErr.Message != "No existe un usuario registrado con ese correo. Pídele que cree su cuenta primero." {
		t.Errorf("mensaje = %q", apiErr.Message)
	}
}

// «Ya es miembro» es el estado deseado, no un error. Tratarlo como fallo haría
// que reintentar una operación que ya funcionó pareciera romperse.
func TestAddAdminTrataElConflictoComoExito(t *testing.T) {
	client := clienteContra(t, func(w http.ResponseWriter, r *http.Request) {
		responder(w, http.StatusConflict, `{"ok":false,"msg":"Ya es miembro de esta tienda"}`)
	})
	already, err := client.AddAdmin(context.Background(), 42, "cliente@ejemplo.com")
	if err != nil {
		t.Fatalf("AddAdmin: %v", err)
	}
	if !already {
		t.Error("no se informó de que ya era miembro")
	}
}

// La clave de aprovisionamiento vale para todas las tiendas de todos los
// clientes. Que salga en claro fuera de la máquina no es una preferencia de
// estilo: es la diferencia entre un secreto de loopback y uno en la red.
func TestNewRechazaHTTPFueraDeLoopback(t *testing.T) {
	if _, err := New("http://10.12.12.1:8865/internal/provision", "clave", nil); err == nil {
		t.Fatal("se aceptó http contra un host de la VPN")
	}
	if _, err := New("https://provision.meud.im/internal/provision", "clave", nil); err != nil {
		t.Fatalf("https debe aceptarse: %v", err)
	}
	if _, err := New("http://127.0.0.1:8865/internal/provision", "clave", nil); err != nil {
		t.Fatalf("loopback debe aceptarse: %v", err)
	}
	if _, err := New(DefaultBaseURL, "", nil); err == nil {
		t.Fatal("se aceptó un cliente sin clave")
	}
}
