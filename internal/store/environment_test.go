package store

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestNormalizeEnv(t *testing.T) {
	if got := NormalizeEnv(""); got != EnvProduction {
		t.Fatalf("empty: %s", got)
	}
	if got := NormalizeEnv("Production"); got != EnvProduction {
		t.Fatalf("production: %s", got)
	}
	if got := NormalizeEnv("staging"); got != EnvStaging {
		t.Fatalf("staging: %s", got)
	}
	if got := NormalizeEnv("preview"); got != EnvStaging {
		t.Fatalf("preview: %s", got)
	}
}

func TestFamilyOf(t *testing.T) {
	p := Project{ID: "prj_a"}
	if got := FamilyOf(p); got != "prj_a" {
		t.Fatalf("id fallback: %s", got)
	}
	p.FamilyID = "prj_b"
	if got := FamilyOf(p); got != "prj_b" {
		t.Fatalf("family: %s", got)
	}
}

func TestIsUniqueViolation(t *testing.T) {
	if IsUniqueViolation(errors.New("nope")) {
		t.Fatal("plain error")
	}
	if !IsUniqueViolation(&pgconn.PgError{Code: "23505"}) {
		t.Fatal("unique")
	}
}
