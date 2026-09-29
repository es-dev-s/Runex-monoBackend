package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"deploy_platform/internal/store"
)

func TestWriteSecretErr(t *testing.T) {
	rec := httptest.NewRecorder()
	writeSecretErr(rec, store.ErrSecretInvalidBlob, "fallback")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	writeSecretErr(rec, store.ErrSecretLimit, "fallback")
	if rec.Code != http.StatusConflict {
		t.Fatalf("limit %d", rec.Code)
	}
}

func TestWriteSecretErrMissing(t *testing.T) {
	rec := httptest.NewRecorder()
	writeSecretErr(rec, store.ErrSecretItemMissing, "gone")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing %d", rec.Code)
	}
}

func TestVaultViewMissing(t *testing.T) {
	view := vaultView(store.SecretVault{}, false)
	if view["exists"] != false {
		t.Fatalf("%v", view)
	}
	view = vaultView(store.SecretVault{KDF: store.SecretKDF, Salt: "abc", WrapIV: "iv", WrappedDEK: "ct"}, true)
	if view["exists"] != true || view["salt"] != "abc" || view["dek"] != nil {
		t.Fatalf("%v", view)
	}
	session := vaultView(store.SecretVault{UnlockMode: store.SecretUnlockSession, KDF: store.SecretSessionKDF}, true)
	if session["unlock"] != store.SecretUnlockSession || session["wrappedDek"] != nil {
		t.Fatalf("%v", session)
	}
}
