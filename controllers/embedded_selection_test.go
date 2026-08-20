package controllers

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Yzx7/sacs-chatbots/env"
	"github.com/Yzx7/sacs-chatbots/helpers"
)

func selectionController(t *testing.T) *Controller {
	t.Helper()
	cph, err := helpers.NewCipher(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return &Controller{Env: &env.Env{Cipher: cph}}
}

// El sobre lleva el access token del cliente al navegador y vuelve. Si se
// pudiera leer o reutilizar en otro bot, la eleccion de numero seria una via
// para conectar una cuenta ajena.
func TestSobreDeSeleccionSoloSirveParaSuBot(t *testing.T) {
	con := selectionController(t)
	const token = "EAALONGTOKEN"

	sealed, err := con.sealEmbeddedSelection(token, "bot-1")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if strings.Contains(sealed, token) {
		t.Fatalf("el token viaja en claro dentro del sobre: %s", sealed)
	}

	got, err := con.openEmbeddedSelection(sealed, "bot-1")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got != token {
		t.Fatalf("token esperado %q, got %q", token, got)
	}

	if _, err := con.openEmbeddedSelection(sealed, "bot-2"); err == nil {
		t.Fatal("un sobre de otro bot deberia rechazarse")
	}
	if _, err := con.openEmbeddedSelection("no-es-base64-valido!!", "bot-1"); err == nil {
		t.Fatal("un sobre corrupto deberia rechazarse")
	}
}

func TestSobreDeSeleccionCaduca(t *testing.T) {
	con := selectionController(t)
	// El TTL se compara contra el reloj, asi que se fabrica uno ya vencido con
	// las mismas piezas en vez de esperar diez minutos.
	payload, err := json.Marshal(embeddedSelection{
		Token: "EAALONGTOKEN",
		BotID: "bot-1",
		Exp:   time.Now().Add(-time.Minute).Unix(),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	enc, err := con.Env.Cipher.Encrypt(string(payload))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	sealed := base64.RawURLEncoding.EncodeToString(enc)

	if _, err := con.openEmbeddedSelection(sealed, "bot-1"); err == nil {
		t.Fatal("un sobre caducado deberia rechazarse")
	}
}

// El webhook desempata, pero no manda: la lista del token es la que autoriza.
func TestPreferRecentlyLinked(t *testing.T) {
	token := []string{"WABA_A", "WABA_B", "WABA_C"}

	t.Run("se queda con la recien vinculada", func(t *testing.T) {
		got := preferRecentlyLinked(token, []string{"WABA_B"})
		if len(got) != 1 || got[0] != "WABA_B" {
			t.Fatalf("esperaba solo WABA_B, got %v", got)
		}
	})

	t.Run("conserva el orden del webhook", func(t *testing.T) {
		got := preferRecentlyLinked(token, []string{"WABA_C", "WABA_A"})
		if len(got) != 2 || got[0] != "WABA_C" || got[1] != "WABA_A" {
			t.Fatalf("esperaba [WABA_C WABA_A], got %v", got)
		}
	})

	// Lo que el token no alcanza no puede entrar por el webhook: seria conectar
	// el bot a una cuenta que el cliente no autorizo en este flujo.
	t.Run("nunca añade una cuenta ajena al token", func(t *testing.T) {
		got := preferRecentlyLinked(token, []string{"WABA_INTRUSA"})
		if len(got) != len(token) {
			t.Fatalf("esperaba la lista del token intacta, got %v", got)
		}
		for _, waba := range got {
			if waba == "WABA_INTRUSA" {
				t.Fatal("se colo una cuenta que el token no autoriza")
			}
		}
	})

	t.Run("sin webhooks devuelve la lista del token", func(t *testing.T) {
		if got := preferRecentlyLinked(token, nil); len(got) != len(token) {
			t.Fatalf("esperaba la lista intacta, got %v", got)
		}
	})

	t.Run("con una sola cuenta no hay nada que desempatar", func(t *testing.T) {
		one := []string{"WABA_A"}
		if got := preferRecentlyLinked(one, []string{"WABA_B"}); len(got) != 1 || got[0] != "WABA_A" {
			t.Fatalf("esperaba [WABA_A], got %v", got)
		}
	})
}
