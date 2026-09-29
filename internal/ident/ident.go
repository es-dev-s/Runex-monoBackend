package ident

import (
	"crypto/rand"
	"encoding/hex"
)

func New(prefix string) string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(buf)
}

// Secret is a URL-safe password for isolated Postgres/Redis sidecars.
func Secret(bytes int) string {
	if bytes < 16 {
		bytes = 24
	}
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf)
}
