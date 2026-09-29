package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"deploy_platform/internal/config"
)

func TestUnauthorizedMeIncludesCORS(t *testing.T) {
	s := &Server{cfg: config.Config{WebURL: "http://localhost:3000"}}
	req := httptest.NewRequest(http.MethodGet, "/v1/auth/me", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Fatalf("CORS origin %q; a missing header makes the dashboard treat sign-out as downtime", got)
	}
	if rec.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatal("credentials must be allowed so the session cookie can be sent")
	}
}

func TestPreflightIsPublic(t *testing.T) {
	s := &Server{cfg: config.Config{WebURL: "http://localhost:3000"}}
	req := httptest.NewRequest(http.MethodOptions, "/v1/projects", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestOriginAllowedLoopback(t *testing.T) {
	s := &Server{cfg: config.Config{WebURL: "http://localhost:3000"}}
	allowed := []string{
		"http://localhost:3000",
		"http://127.0.0.1:3000",
		"http://localhost:3001",
		"http://127.0.0.1:3001",
		"http://localhost:4173",
	}
	for _, origin := range allowed {
		if !s.originAllowed(origin) {
			t.Fatalf("expected %s allowed", origin)
		}
	}
	if s.originAllowed("https://evil.example") {
		t.Fatal("foreign origin must be rejected")
	}
	if s.originAllowed("") {
		t.Fatal("empty origin must be rejected")
	}
}

func TestOriginAllowedProductionDashboard(t *testing.T) {
	s := &Server{cfg: config.Config{
		Env:       "production",
		WebURL:    "https://aivex.art",
		PublicURL: "https://api.aivex.art",
	}}
	for _, origin := range []string{"https://aivex.art", "https://www.aivex.art", "https://api.aivex.art"} {
		if !s.originAllowed(origin) {
			t.Fatalf("expected %s allowed", origin)
		}
	}
	if s.originAllowed("http://localhost:3000") {
		t.Fatal("production must not allow random localhost origins")
	}
	if s.originAllowed("https://evil.example") {
		t.Fatal("foreign origin must be rejected")
	}

	s.cfg.CORSOrigins = []string{"https://harbor-prod.up.railway.app"}
	if !s.originAllowed("https://harbor-prod.up.railway.app") {
		t.Fatal("explicit PLATFORM_CORS_ORIGINS must be allowed")
	}
}
