package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// newGCM builds an AES-256-GCM AEAD; key must be exactly 32 bytes.
func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("store: key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// seal encrypts plaintext with a random nonce prepended to the ciphertext.
// An empty value is stored as an empty blob.
func seal(aead cipher.AEAD, plaintext string) ([]byte, error) {
	if plaintext == "" {
		return []byte{}, nil
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

// open reverses seal.
func open(aead cipher.AEAD, blob []byte) (string, error) {
	if len(blob) == 0 {
		return "", nil
	}
	n := aead.NonceSize()
	if len(blob) < n {
		return "", errors.New("store: ciphertext too short")
	}
	pt, err := aead.Open(nil, blob[:n], blob[n:], nil)
	if err != nil {
		return "", fmt.Errorf("store: decrypt: %w", err)
	}
	return string(pt), nil
}
