package githubapp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestVerifyWebhook(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main"}`)
	mac := hmac.New(sha256.New, []byte("secret"))
	mac.Write(body)
	header := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !VerifyWebhook("secret", header, body) {
		t.Fatal("expected valid signature")
	}
	if VerifyWebhook("secret", header+"x", body) {
		t.Fatal("expected invalid signature")
	}
	if VerifyWebhook("", header, body) {
		t.Fatal("empty secret must fail")
	}
}

func TestParsePushBranch(t *testing.T) {
	evt, err := ParsePush([]byte(`{
		"ref":"refs/heads/main",
		"after":"abc1234deadbeef",
		"deleted":false,
		"installation":{"id":99},
		"repository":{"full_name":"acme/app","default_branch":"main"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if evt.Branch != "main" || evt.FullName != "acme/app" || evt.InstallationID != 99 {
		t.Fatalf("%+v", evt)
	}
}

func TestUserStateRoundTrip(t *testing.T) {
	state := EncodeUserState("secret", "usr_abc", "/projects/prj_1")
	userID, path, err := DecodeUserState("secret", state)
	if err != nil {
		t.Fatal(err)
	}
	if userID != "usr_abc" || path != "/projects/prj_1" {
		t.Fatalf("got %q %q", userID, path)
	}
	if _, _, err := DecodeUserState("other", state); err == nil {
		t.Fatal("expected signature failure")
	}
}

func TestStateRoundTrip(t *testing.T) {
	state := EncodeState("secret", "/projects/prj_1")
	path, err := DecodeState("secret", state)
	if err != nil {
		t.Fatal(err)
	}
	if path != "/projects/prj_1" {
		t.Fatalf("got %q", path)
	}
	if _, err := DecodeState("other", state); err == nil {
		t.Fatal("expected signature failure")
	}
}

func TestIntentStateRoundTrip(t *testing.T) {
	state := EncodeIntentState("secret", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	id, err := DecodeIntentState("secret", state)
	if err != nil {
		t.Fatal(err)
	}
	if id != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("got %q", id)
	}
	if _, err := DecodeIntentState("other", state); err == nil {
		t.Fatal("expected signature failure")
	}
}

func TestSafeReturnPath(t *testing.T) {
	if SafeReturnPath("/deployments") != "/deployments" {
		t.Fatal("deployments")
	}
	if SafeReturnPath("https://evil.test") != "/deployments" {
		t.Fatal("url")
	}
	if SafeReturnPath("//evil") != "/deployments" {
		t.Fatal("protocol-relative")
	}
}
