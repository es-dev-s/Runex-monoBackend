package vaultkey

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestWrapRoundTrip(t *testing.T) {
	root := bytes.Repeat([]byte{0x11}, RootSize)
	dek := bytes.Repeat([]byte{0x22}, DEKSize)
	iv, ct, err := WrapDEK(root, dek)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnwrapDEK(root, iv, ct)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, dek) {
		t.Fatal("dek mismatch")
	}
	if _, err := UnwrapDEK(bytes.Repeat([]byte{0x33}, RootSize), iv, ct); err != ErrInvalidWrap {
		t.Fatalf("wrong root: %v", err)
	}
}

func TestLoadFileGeneratesOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".vault-root")
	first, err := Load("", path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Load("", path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("generated key was not stable")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("perm %v", info.Mode().Perm())
	}
}

func TestLoadEnvHex(t *testing.T) {
	raw := hex.EncodeToString(bytes.Repeat([]byte{0xab}, RootSize))
	key, err := Load(raw, "")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(key, bytes.Repeat([]byte{0xab}, RootSize)) {
		t.Fatal(key)
	}
}
