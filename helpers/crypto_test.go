package helpers

import (
	"crypto/rand"
	"encoding/hex"
	"testing"
)

func TestCipherRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	c, err := NewCipher(hex.EncodeToString(key))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	ct, err := c.Encrypt("secreto-123")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	pt, err := c.Decrypt(ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if pt != "secreto-123" {
		t.Fatalf("round-trip: got %q", pt)
	}
	if _, err := NewCipher("abcd"); err == nil {
		t.Fatal("esperaba error por clave de tamaño inválido")
	}
}
