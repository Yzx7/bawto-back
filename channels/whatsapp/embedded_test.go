package whatsapp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegisterPhone(t *testing.T) {
	var gotPath, gotAuth, gotContentType, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		gotBody = string(body)
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer srv.Close()

	if err := RegisterPhone(context.Background(), srv.URL, "v21.0", "PNID123", "TKN", "123456", srv.Client()); err != nil {
		t.Fatalf("RegisterPhone: %v", err)
	}
	if gotPath != "/v21.0/PNID123/register" {
		t.Fatalf("path incorrecto: %q", gotPath)
	}
	if gotAuth != "Bearer TKN" {
		t.Fatalf("auth incorrecto: %q", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Fatalf("content-type incorrecto: %q", gotContentType)
	}
	for _, part := range []string{`"messaging_product":"whatsapp"`, `"pin":"123456"`} {
		if !strings.Contains(gotBody, part) {
			t.Fatalf("body sin %s: %s", part, gotBody)
		}
	}
}

func TestExchangeCode(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"access_token":"EAALONGTOKEN","token_type":"bearer"}`))
	}))
	defer srv.Close()

	tok, err := ExchangeCode(context.Background(), srv.URL, "v21.0", "APPID", "SECRET", "CODE123", srv.Client())
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if tok != "EAALONGTOKEN" {
		t.Fatalf("token esperado EAALONGTOKEN, got %q", tok)
	}
	if gotPath != "/v21.0/oauth/access_token" {
		t.Fatalf("path incorrecto: %q", gotPath)
	}
	for _, s := range []string{"client_id=APPID", "client_secret=SECRET", "code=CODE123"} {
		if !strings.Contains(gotQuery, s) {
			t.Fatalf("query sin %s: %s", s, gotQuery)
		}
	}
}

func TestSubscribeWABA(t *testing.T) {
	var gotPath, gotAuth, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotMethod = r.URL.Path, r.Header.Get("Authorization"), r.Method
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer srv.Close()

	if err := SubscribeWABA(context.Background(), srv.URL, "v21.0", "WABA123", "TKN", srv.Client()); err != nil {
		t.Fatalf("SubscribeWABA: %v", err)
	}
	if gotPath != "/v21.0/WABA123/subscribed_apps" {
		t.Fatalf("path incorrecto: %q", gotPath)
	}
	if gotAuth != "Bearer TKN" {
		t.Fatalf("auth incorrecto: %q", gotAuth)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("método incorrecto: %q", gotMethod)
	}
}

// El caso real que motivo DiscoverWABAs: una conexion hecha desde el navegador
// del movil llega al backend sin waba_id, porque el postMessage del popup no
// alcanza al panel. El token sí sabe a que cuenta pertenece.
func TestDiscoverWABAsLeeSoloElScopeDeManagement(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"data":{"granular_scopes":[
		  {"scope":"whatsapp_business_messaging","target_ids":["999NUMERO"]},
		  {"scope":"whatsapp_business_management","target_ids":["1721895455603060","1721895455603060"]}
		]}}`))
	}))
	defer srv.Close()

	wabas, err := DiscoverWABAs(context.Background(), srv.URL, "v21.0", "APPID", "SECRET", "TKN", srv.Client())
	if err != nil {
		t.Fatalf("DiscoverWABAs: %v", err)
	}
	if len(wabas) != 1 || wabas[0] != "1721895455603060" {
		t.Fatalf("wabas inesperadas: %v", wabas)
	}
	if gotPath != "/v21.0/debug_token" {
		t.Fatalf("path incorrecto: %q", gotPath)
	}
	// El app token va como client_id|secret; sin el, Meta no revela los scopes.
	for _, part := range []string{"input_token=TKN", "access_token=APPID%7CSECRET"} {
		if !strings.Contains(gotQuery, part) {
			t.Fatalf("query sin %s: %s", part, gotQuery)
		}
	}
}

func TestListPhoneNumbers(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[{"id":"1302561912931418","display_phone_number":"+51 999 888 777","verified_name":"Tienda"}]}`))
	}))
	defer srv.Close()

	nums, err := ListPhoneNumbers(context.Background(), srv.URL, "v21.0", "WABA1", "TKN", srv.Client())
	if err != nil {
		t.Fatalf("ListPhoneNumbers: %v", err)
	}
	if len(nums) != 1 || nums[0].ID != "1302561912931418" || nums[0].DisplayPhoneNumber != "+51 999 888 777" {
		t.Fatalf("numeros inesperados: %+v", nums)
	}
	if gotPath != "/v21.0/WABA1/phone_numbers" {
		t.Fatalf("path incorrecto: %q", gotPath)
	}
	if gotAuth != "Bearer TKN" {
		t.Fatalf("auth incorrecto: %q", gotAuth)
	}
}
