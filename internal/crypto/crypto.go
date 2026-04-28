package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// LoadKey reads ENCRYPTION_KEY env var (64 hex chars = 32 bytes) and returns the raw key.
func LoadKey() ([]byte, error) {
	raw := os.Getenv("ENCRYPTION_KEY")
	if raw == "" {
		return nil, nil // encryption disabled
	}
	key, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("ENCRYPTION_KEY debe ser hex de 64 caracteres: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("ENCRYPTION_KEY debe ser de 32 bytes (64 hex chars), tiene %d bytes", len(key))
	}
	return key, nil
}

// Encrypt encrypts plaintext with AES-256-GCM. Returns base64(nonce||ciphertext).
func Encrypt(key []byte, plaintext string) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt reverses Encrypt.
func Decrypt(key []byte, encoded string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("base64 decode: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(data) < gcm.NonceSize() {
		return "", fmt.Errorf("ciphertext demasiado corto")
	}
	nonce, ciphertext := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("descifrar: %w", err)
	}
	return string(plaintext), nil
}
