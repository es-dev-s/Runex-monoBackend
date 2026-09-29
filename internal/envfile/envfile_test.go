package envfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseSkipsCommentsAndExports(t *testing.T) {
	got := Parse(`
# comment
export PORT=3000
DATABASE_URL=postgres://app:app@postgres:5432/app
EMPTY=
QUOTED="hello world"
WITH_COMMENT=value # trailing
INVALID
1BAD=no
`)
	want := map[string]string{
		"PORT":         "3000",
		"DATABASE_URL": "postgres://app:app@postgres:5432/app",
		"EMPTY":        "",
		"QUOTED":       "hello world",
		"WITH_COMMENT": "value",
	}
	if len(got) != len(want) {
		t.Fatalf("len=%d pairs=%+v", len(got), got)
	}
	for _, pair := range got {
		if want[pair.Key] != pair.Value {
			t.Fatalf("%s: got %q want %q", pair.Key, pair.Value, want[pair.Key])
		}
	}
}

func TestParseKeepsLastDuplicate(t *testing.T) {
	got := Parse("FOO=one\nFOO=two\n")
	if len(got) != 1 || got[0].Value != "two" {
		t.Fatalf("duplicate handling: %+v", got)
	}
}

func TestDiscoverReadsDummyEnvAndCode(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env.example"), []byte("API_KEY=\nDATABASE_URL=postgres://app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=real\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("os.Getenv(\"OPENAI_API_KEY\")\nos.Getenv(\"PATH\")\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := Discover(dir)
	keys := map[string]string{}
	for _, pair := range got {
		keys[pair.Key] = pair.Value
	}
	if keys["API_KEY"] != "" || keys["DATABASE_URL"] != "postgres://app" {
		t.Fatalf("dummy env: %+v", got)
	}
	if _, ok := keys["OPENAI_API_KEY"]; !ok {
		t.Fatalf("missing code key: %+v", got)
	}
	if _, ok := keys["SECRET"]; ok {
		t.Fatal("did not want secrets from .env")
	}
	if _, ok := keys["PATH"]; ok {
		t.Fatal("did not want PATH from code")
	}
}

func TestDummyEnvFileNames(t *testing.T) {
	if !isDummyEnvFile(".env.example") || !isDummyEnvFile(".env.sample") || !isDummyEnvFile("env.example") {
		t.Fatal("expected dummy env names")
	}
	if isDummyEnvFile(".env") || isDummyEnvFile(".env.local") || isDummyEnvFile("package.json") {
		t.Fatal("rejected live env files")
	}
}

func TestFormatRoundTrip(t *testing.T) {
	raw := Format([]Pair{{Key: "MSG", Value: "hello world"}, {Key: "PORT", Value: "80"}})
	got := Parse(raw)
	if len(got) != 2 {
		t.Fatalf("round trip: %q -> %+v", raw, got)
	}
}

func TestAuthSecretHelpers(t *testing.T) {
	if !IsAuthSecretKey("AUTH_SECRET") || !IsAuthSecretKey("NEXTAUTH_SECRET") {
		t.Fatal("expected auth secret keys")
	}
	if IsAuthSecretKey("DATABASE_URL") {
		t.Fatal("database url is not an auth secret")
	}
	secret := RandomSecret()
	if len(secret) != 64 {
		t.Fatalf("secret length %d", len(secret))
	}
	dir := t.TempDir()
	path := filepath.Join(dir, ".env.production")
	if err := Write(path, []Pair{{Key: "AUTH_SECRET", Value: secret}}); err != nil {
		t.Fatal(err)
	}
	got := Parse(string(mustRead(path)))
	if len(got) != 1 || got[0].Value != secret {
		t.Fatalf("wrote %+v", got)
	}
}

func mustRead(path string) []byte {
	raw, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	return raw
}
