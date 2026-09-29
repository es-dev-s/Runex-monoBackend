package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"deploy_platform/internal/config"
)

func TestSessionCookieParentDomain(t *testing.T) {
	s := &Server{cfg: config.Config{WebURL: "https://aivex.art", PublicURL: "https://api.aivex.art"}}
	cookie := s.sessionCookie("tok", time.Hour)
	if cookie.Domain != "aivex.art" {
		t.Fatalf("domain %q", cookie.Domain)
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Fatal("same-site siblings should use Lax")
	}
	if !cookie.Secure || !cookie.HttpOnly {
		t.Fatal("production cookies must be Secure HttpOnly")
	}
}

func TestSessionCookieCrossSiteNone(t *testing.T) {
	s := &Server{cfg: config.Config{
		WebURL:    "https://harbor-prod.up.railway.app",
		PublicURL: "https://api.aivex.art",
	}}
	cookie := s.sessionCookie("tok", time.Hour)
	if cookie.Domain != "" {
		t.Fatalf("cross-site must not set Domain, got %q", cookie.Domain)
	}
	if cookie.SameSite != http.SameSiteNoneMode {
		t.Fatal("cross-site cookies must be SameSite=None")
	}
	if !cookie.Secure {
		t.Fatal("SameSite=None requires Secure")
	}
}

func TestSessionCookieLocalDashboardToCloudAPI(t *testing.T) {
	s := &Server{cfg: config.Config{
		Env:         "production",
		WebURL:      "https://aivex.art",
		PublicURL:   "https://api.aivex.art",
		CORSOrigins: []string{"http://localhost:3000"},
	}}
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	cookie := s.sessionCookieFor(req, "tok", time.Hour)
	if cookie.SameSite != http.SameSiteNoneMode {
		t.Fatal("localhost dashboard talking to api.aivex.art must use SameSite=None")
	}
	if !cookie.Secure {
		t.Fatal("SameSite=None requires Secure")
	}
}

func TestSessionCookieHTTPSameOriginNotSecure(t *testing.T) {
	s := &Server{cfg: config.Config{
		Env:       "production",
		WebURL:    "http://deploy.salesradar.live",
		PublicURL: "http://deploy.salesradar.live",
	}}
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
	req.Header.Set("Origin", "http://deploy.salesradar.live")
	req.Header.Set("X-Forwarded-Proto", "http")
	cookie := s.sessionCookieFor(req, "tok", time.Hour)
	if cookie.Secure {
		t.Fatal("HTTP LAN cookies must not be Secure or the browser drops them")
	}
	if cookie.Domain != "" {
		t.Fatalf("same-host cookie must be host-only, got %q", cookie.Domain)
	}
}

func TestSessionCookieHTTPDashboardWithRailwayGitHubProxy(t *testing.T) {
	s := &Server{cfg: config.Config{
		Env:       "production",
		WebURL:    "http://deploy.salesradar.live",
		PublicURL: "https://harbor-proxy-production.up.railway.app",
	}}
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
	req.Header.Set("Origin", "http://deploy.salesradar.live")
	req.Header.Set("X-Forwarded-Proto", "http")
	cookie := s.sessionCookieFor(req, "tok", time.Hour)
	if cookie.Secure {
		t.Fatal("LAN HTTP session must not be Secure when GitHub uses a Railway proxy")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("same-origin LAN session should be Lax, got %v", cookie.SameSite)
	}
}

func TestSessionCookieLANDashboardWithoutForwardedProto(t *testing.T) {
	s := &Server{cfg: config.Config{
		Env:       "development",
		WebURL:    "https://app.runex.cloud",
		PublicURL: "https://app.runex.cloud",
	}}
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
	req.Header.Set("Origin", "http://192.168.1.111:3000")
	req.Header.Set("X-Forwarded-Host", "192.168.1.111:3000")
	cookie := s.sessionCookieFor(req, "tok", time.Hour)
	if cookie.Secure {
		t.Fatal("HTTP LAN cookies must not be Secure when the dev proxy omits X-Forwarded-Proto")
	}
	if cookie.Domain != "" {
		t.Fatalf("same-host cookie must be host-only, got %q", cookie.Domain)
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("same-origin LAN session should be Lax, got %v", cookie.SameSite)
	}
}

func TestSessionCookiePublicHostStaysSecureWithoutProto(t *testing.T) {
	s := &Server{cfg: config.Config{
		Env:       "development",
		WebURL:    "https://app.runex.cloud",
		PublicURL: "https://app.runex.cloud",
	}}
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/login", nil)
	req.Header.Set("Origin", "http://app.runex.cloud")
	req.Header.Set("X-Forwarded-Host", "app.runex.cloud")
	cookie := s.sessionCookieFor(req, "tok", time.Hour)
	if !cookie.Secure {
		t.Fatal("a public host must stay Secure when the scheme is only claimed by the client")
	}
}
