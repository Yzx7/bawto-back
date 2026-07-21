package helpers

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
)

// Cipher cifra/descifra secretos en reposo con AES-256-GCM.
type Cipher struct {
	gcm cipher.AEAD
}

// NewCipher crea un Cipher a partir de una clave de 32 bytes en hex (64 chars).
func NewCipher(hexKey string) (*Cipher, error) {
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("crypto: la clave debe ser de 32 bytes (64 hex)")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{gcm: gcm}, nil
}

// Encrypt devuelve nonce||ciphertext.
func (c *Cipher) Encrypt(plain string) ([]byte, error) {
	nonce := make([]byte, c.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return c.gcm.Seal(nonce, nonce, []byte(plain), nil), nil
}

// Decrypt revierte Encrypt.
func (c *Cipher) Decrypt(ct []byte) (string, error) {
	ns := c.gcm.NonceSize()
	if len(ct) < ns {
		return "", errors.New("crypto: ciphertext demasiado corto")
	}
	nonce, data := ct[:ns], ct[ns:]
	plain, err := c.gcm.Open(nil, nonce, data, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
