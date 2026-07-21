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
