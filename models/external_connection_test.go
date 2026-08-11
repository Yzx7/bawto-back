package models

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func randConnHex(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func TestMaskCredentialDejaReconocerSinRevelar(t *testing.T) {
	casos := map[string]string{
		"pk_live_51HxAbCdEfGhIjKlMnObc84": "pk_live_…bc84",
		"sk_test_abcdefghijklmnop":        "sk_test_…mnop",
		"corta":                           "•••••",
		"":                                "",
		// Sin los dos guiones bajos del prefijo no hay prefijo que mostrar; se
		// conservan solo los últimos cuatro.
		"unaclavesinprefijo": "…fijo",
	}
	for credential, quiere := range casos {
		if got := MaskCredential(credential); got != quiere {
			t.Errorf("MaskCredential(%q) = %q, esperado %q", credential, got, quiere)
		}
	}
}

// El footgun de la pantalla de conexiones: al editar el nombre o la URL, el
// panel no vuelve a enviar la credencial —no la tiene—. Si el upsert la
// sobrescribiera con vacío, corregir una errata dejaría la conexión muerta y el
// bot mudo sobre el catálogo, sin un solo error visible hasta la siguiente
// conversación.
func TestSaveExternalConnectionConservaLaCredencialAlActualizar(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL no seteada; se omite el test de integración")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	uid := randConnHex("connu_")
	if _, err := pool.Exec(ctx, `INSERT INTO "user"(id,name,email,"emailVerified") VALUES ($1,$2,$3,false)`,
		uid, "Conn Owner", uid+"@test.local"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	defer pool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, uid)

	org, err := CreateOrganization(ctx, pool, uid, "Conn Org", nil, nil)
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	defer DeleteOrganization(context.Background(), pool, org.ID)

	original := []byte("cifrado-original")
	created, err := SaveExternalConnection(ctx, pool, ExternalConnectionInput{
		OrgID: org.ID, Key: "meudim", Driver: "meudim", Label: "Tienda",
		BaseURL: "https://api.meud.im", CredentialEnc: original,
	})
	if err != nil {
		t.Fatalf("alta: %v", err)
	}
	if string(created.CredentialEnc) != string(original) {
		t.Fatalf("la credencial no se guardó: %q", created.CredentialEnc)
	}

	// Actualizar sin credencial: cambia la etiqueta, conserva la clave.
	updated, err := SaveExternalConnection(ctx, pool, ExternalConnectionInput{
		OrgID: org.ID, Key: "meudim", Driver: "meudim", Label: "Tienda Sistemuino",
		BaseURL: "https://api.meud.im",
	})
	if err != nil {
		t.Fatalf("actualización: %v", err)
	}
	if updated.Label != "Tienda Sistemuino" {
		t.Fatalf("la etiqueta no cambió: %q", updated.Label)
	}
	if string(updated.CredentialEnc) != string(original) {
		t.Fatalf("la actualización borró la credencial: %q", updated.CredentialEnc)
	}
	if updated.ID != created.ID {
		t.Fatalf("la identidad es (org, clave): se creó una fila nueva %s", updated.ID)
	}

	// Rotar sí la reemplaza.
	rotada, err := SaveExternalConnection(ctx, pool, ExternalConnectionInput{
		OrgID: org.ID, Key: "meudim", Driver: "meudim", Label: "Tienda Sistemuino",
		BaseURL: "https://api.meud.im", CredentialEnc: []byte("cifrado-nuevo"),
	})
	if err != nil {
		t.Fatalf("rotación: %v", err)
	}
	if string(rotada.CredentialEnc) != "cifrado-nuevo" {
		t.Fatalf("la rotación no aplicó: %q", rotada.CredentialEnc)
	}

	// Un alta sin credencial no puede pasar: la columna es NOT NULL y una
	// conexión sin clave no serviría para nada.
	if _, err := SaveExternalConnection(ctx, pool, ExternalConnectionInput{
		OrgID: org.ID, Key: "otra", Driver: "meudim", Label: "Otra",
		BaseURL: "https://api.meud.im",
	}); err == nil {
		t.Fatal("se aceptó un alta sin credencial")
	}

	// Y el resultado se anota sin tocar la credencial.
	if err := RecordExternalConnectionResult(ctx, pool, rotada.ID, nil); err != nil {
		t.Fatalf("anotar acierto: %v", err)
	}
	leida, err := ExternalConnectionByKey(ctx, pool, org.ID, "meudim")
	if err != nil || leida == nil {
		t.Fatalf("relectura: %v", err)
	}
	if leida.LastOKAt == nil || leida.LastError != nil {
		t.Fatalf("estado tras el acierto: ok=%v err=%v", leida.LastOKAt, leida.LastError)
	}

	deleted, err := DeleteExternalConnection(ctx, pool, org.ID, "meudim")
	if err != nil || !deleted {
		t.Fatalf("borrado: deleted=%v err=%v", deleted, err)
	}
	gone, err := ExternalConnectionByKey(ctx, pool, org.ID, "meudim")
	if err != nil {
		t.Fatalf("relectura tras borrar: %v", err)
	}
	if gone != nil {
		t.Fatal("la conexión sobrevivió al borrado")
	}
}
