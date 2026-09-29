package store

import "testing"

func TestValidSecretB64(t *testing.T) {
	if ValidSecretB64("", 12, 12) {
		t.Fatal("empty")
	}
	if ValidSecretB64("%%%", 1, 16) {
		t.Fatal("not base64")
	}
	// 12 zero bytes
	if !ValidSecretB64("AAAAAAAAAAAAAAAA", 12, 12) {
		t.Fatal("12-byte iv")
	}
	if ValidSecretB64("AA==", 12, 12) {
		t.Fatal("too short")
	}
}

func TestNormalizeSecretVault(t *testing.T) {
	_, err := NormalizeSecretVault(SecretVault{KDFIters: 1000, Salt: "AAAAAAAAAAAAAAAAAAAAAA==", WrapIV: "AAAAAAAAAAAAAAAA", WrappedDEK: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"})
	if err != ErrSecretInvalidBlob {
		t.Fatalf("weak kdf: %v", err)
	}
	got, err := NormalizeSecretVault(SecretVault{
		KDF:        "PBKDF2-SHA256",
		KDFIters:   600_000,
		Salt:       "AAAAAAAAAAAAAAAAAAAAAA==",
		WrapIV:     "AAAAAAAAAAAAAAAA",
		WrappedDEK: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.KDF != SecretKDF || got.UnlockMode != SecretUnlockPass {
		t.Fatalf("kdf %s mode %s", got.KDF, got.UnlockMode)
	}
	session, err := NormalizeSecretVault(SecretVault{
		UnlockMode: SecretUnlockSession,
		KDF:        SecretSessionKDF,
		KDFIters:   1,
		Salt:       "AAAAAAAAAAAAAAAAAAAAAA==",
		WrapIV:     "AAAAAAAAAAAAAAAA",
		WrappedDEK: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !session.SessionUnlock() {
		t.Fatal("session")
	}
}

func TestNormalizeSecretCipher(t *testing.T) {
	if _, _, err := NormalizeSecretCipher("AAAAAAAAAAAAAAAA", "short", 4096); err != ErrSecretInvalidBlob {
		t.Fatalf("bad ct: %v", err)
	}
	if _, _, err := NormalizeSecretCipher("AAAAAAAAAAAAAAAA", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", 4096); err != nil {
		t.Fatal(err)
	}
}
