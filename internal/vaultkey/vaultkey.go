// Package vaultkey wraps per-user data-encryption keys with a platform root key.
// The root key never leaves the API process. Each account has its own DEK.
package vaultkey

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

const RootSize = 32
const DEKSize = 32

var ErrInvalidRoot = errors.New("vault root key is invalid")
var ErrInvalidWrap = errors.New("vault wrap is invalid")

func Load(envValue, filePath string) ([]byte, error) {
	if key, err := parseRoot(envValue); err == nil {
		return key, nil
	} else if strings.TrimSpace(envValue) != "" {
		return nil, fmt.Errorf("PLATFORM_VAULT_KEY: %w", err)
	}
	if filePath != "" {
		if raw, err := os.ReadFile(filePath); err == nil {
			key, err := parseRoot(string(raw))
			if err != nil {
				return nil, fmt.Errorf("vault root file: %w", err)
			}
			return key, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		key := make([]byte, RootSize)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filePath, []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
			return nil, err
		}
		return key, nil
	}
	return nil, ErrInvalidRoot
}

func WrapDEK(root, dek []byte) (ivB64, ctB64 string, err error) {
	if len(root) != RootSize || len(dek) != DEKSize {
		return "", "", ErrInvalidRoot
	}
	aead, err := aesGCM(root)
	if err != nil {
		return "", "", err
	}
	iv := make([]byte, aead.NonceSize())
	if _, err := rand.Read(iv); err != nil {
		return "", "", err
	}
	ct := aead.Seal(nil, iv, dek, nil)
	return base64.StdEncoding.EncodeToString(iv), base64.StdEncoding.EncodeToString(ct), nil
}

func UnwrapDEK(root []byte, ivB64, ctB64 string) ([]byte, error) {
	if len(root) != RootSize {
		return nil, ErrInvalidRoot
	}
	iv, err := base64.StdEncoding.DecodeString(strings.TrimSpace(ivB64))
	if err != nil {
		return nil, ErrInvalidWrap
	}
	ct, err := base64.StdEncoding.DecodeString(strings.TrimSpace(ctB64))
	if err != nil {
		return nil, ErrInvalidWrap
	}
	aead, err := aesGCM(root)
	if err != nil {
		return nil, err
	}
	dek, err := aead.Open(nil, iv, ct, nil)
	if err != nil || len(dek) != DEKSize {
		return nil, ErrInvalidWrap
	}
	return dek, nil
}

func ParseDEK(raw string) ([]byte, error) {
	dek, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil || len(dek) != DEKSize {
		return nil, ErrInvalidWrap
	}
	return dek, nil
}

func parseRoot(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ErrInvalidRoot
	}
	if key, err := hex.DecodeString(raw); err == nil && len(key) == RootSize {
		return key, nil
	}
	if key, err := base64.StdEncoding.DecodeString(raw); err == nil && len(key) == RootSize {
		return key, nil
	}
	return nil, ErrInvalidRoot
}

func aesGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
