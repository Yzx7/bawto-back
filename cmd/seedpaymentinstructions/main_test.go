package main

import (
	"strings"
	"testing"
)

func TestPaymentMessageIsDeterministicAndComplete(t *testing.T) {
	message := paymentMessage(" Yape ", " 999 111 222 ", " Sistemuino SAC ", "No colocar descripción.")
	for _, expected := range []string{
		"usa Yape", "Destino: 999 111 222", "Titular: Sistemuino SAC",
		"Moneda: PEN", "No colocar descripción.", "captura completa y nítida",
	} {
		if !strings.Contains(message, expected) {
			t.Fatalf("falta %q en %q", expected, message)
		}
	}
}
