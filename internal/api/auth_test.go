package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestValidateUsername(t *testing.T) {
	if err := validateUsername("ab"); err == nil {
		t.Fatal("too short")
	}
	if err := validateUsername("ok_user-1"); err != nil {
		t.Fatal(err)
	}
	if err := validateUsername("Bad User"); err == nil {
		t.Fatal("spaces")
	}
}

func TestValidateEmail(t *testing.T) {
	if err := validateEmail("not-an-email"); err == nil {
		t.Fatal("invalid")
	}
	if err := validateEmail("dev@localhost"); err == nil {
		t.Fatal("missing dot")
	}
	if err := validateEmail("dev@example.com"); err != nil {
		t.Fatal(err)
	}
}

func TestValidatePassword(t *testing.T) {
	if err := validatePassword("short1"); err == nil {
		t.Fatal("too short")
	}
	if err := validatePassword("lettersonly"); err == nil {
		t.Fatal("need number")
	}
	if err := validatePassword("12345678"); err == nil {
		t.Fatal("need letter")
	}
	if err := validatePassword("secure9x"); err != nil {
		t.Fatal(err)
	}
}

func TestAuthPublic(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/register", nil)
	if !authPublic(req) {
		t.Fatal("register should be public")
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/projects", nil)
	if authPublic(req) {
		t.Fatal("projects must require a session")
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/github/webhook", nil)
	if !authPublic(req) {
		t.Fatal("webhook should stay public")
	}
}

func TestLimiter(t *testing.T) {
	l := newLimiter(2, time.Minute)
	if !l.allow("1.1.1.1") || !l.allow("1.1.1.1") || l.allow("1.1.1.1") {
		t.Fatal("expected third attempt blocked")
	}
	if !l.allow("2.2.2.2") {
		t.Fatal("other IP should be independent")
	}
}
