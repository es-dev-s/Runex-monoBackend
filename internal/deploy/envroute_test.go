package deploy

import "testing"

func TestRewriteRailwayPostgresURL(t *testing.T) {
	got, ok := rewritePrivateServiceURL(
		"postgres://postgres:secret@postgres.railway.internal:5432/railway",
		&SidecarLink{Host: "postgres", User: "app", Password: "app", Database: "app", Port: "5432"},
		nil,
	)
	if !ok {
		t.Fatal("expected rewrite")
	}
	if got != "postgres://app:app@postgres:5432/app" {
		t.Fatalf("got %s", got)
	}
}

func TestPinSidecarOverridesPlatformDatabaseURL(t *testing.T) {
	env := map[string]string{
		"PLATFORM_DATABASE_URL": "postgres://postgres:x@postgres.railway.internal:5432/railway",
		"REDIS_URL":             "redis://redis.railway.internal:6379",
	}
	changed := pinSidecarURLs(env,
		&SidecarLink{Host: "postgres", User: "app", Password: "app", Database: "app", Port: "5432"},
		&SidecarLink{Host: "redis", Port: "6379"},
	)
	if env["PLATFORM_DATABASE_URL"] != "postgres://app:app@postgres:5432/app" {
		t.Fatalf("postgres: %s", env["PLATFORM_DATABASE_URL"])
	}
	if env["REDIS_URL"] != "redis://redis:6379/0" {
		t.Fatalf("redis: %s", env["REDIS_URL"])
	}
	if len(changed) == 0 {
		t.Fatal("expected changed keys")
	}
}

func TestUnreachablePrivateKeys(t *testing.T) {
	env := map[string]string{
		"PLATFORM_DATABASE_URL": "postgres://postgres:x@postgres.railway.internal:5432/railway",
		"PORT":                  "8080",
	}
	keys := unreachablePrivateKeys(env)
	if len(keys) != 1 || keys[0] != "PLATFORM_DATABASE_URL" {
		t.Fatalf("keys=%v", keys)
	}
	if _, ok := rewritePrivateServiceURL(env["PLATFORM_DATABASE_URL"], nil, nil); ok {
		t.Fatal("must not rewrite without a sidecar")
	}
}
