package edge

import (
	"crypto/tls"
	"net/http"
	"testing"

	"deploy_platform/internal/config"
)

func TestParseHost(t *testing.T) {
	slug, ok := ParseHost("svc-abc.localhost:8080", "localhost")
	if !ok || slug != "svc-abc" {
		t.Fatalf("got %q %v", slug, ok)
	}
	if _, ok := ParseHost("localhost:8080", "localhost"); ok {
		t.Fatal("bare localhost must stay on the API")
	}
	if _, ok := ParseHost("127.0.0.1:8080", "localhost"); ok {
		t.Fatal("loopback must stay on the API")
	}
	slug, ok = ParseHost("svc-abc.apps.company.local", "apps.company.local")
	if !ok || slug != "svc-abc" {
		t.Fatalf("https host got %q %v", slug, ok)
	}
}

func TestReservedSlug(t *testing.T) {
	if !ReservedSlug("www") || !ReservedSlug("API") {
		t.Fatal("platform hosts must not be treated as apps")
	}
	if !ReservedSlug("designflow") || !ReservedSlug("time") || !ReservedSlug("quality") {
		t.Fatal("existing public salesradar hosts must not be claimed by apps")
	}
	if !ReservedSlug("cname") {
		t.Fatal("the custom-domain CNAME target must not be claimed by an app")
	}
	if ReservedSlug("svc-abc") {
		t.Fatal("service slugs must remain routable")
	}
}

func TestAppURLUsesEdgeHost(t *testing.T) {
	cfg := config.Config{
		Addr:       "127.0.0.1:8080",
		PublicBase: "http://127.0.0.1",
		EdgeDomain: "localhost",
	}
	got := AppURL(cfg, "svc_abc", 21000)
	if got != "http://svc-abc.localhost:8080" {
		t.Fatalf("got %s", got)
	}
}

func TestAppURLHTTPSOmitsInternalPort(t *testing.T) {
	cfg := config.Config{
		Addr:       "127.0.0.1:8080",
		PublicBase: "https://app.localhost",
		EdgeDomain: "app.localhost",
	}
	got := AppURL(cfg, "svc_abc", 21000)
	if got != "https://svc-abc.app.localhost" {
		t.Fatalf("got %s", got)
	}
}

func TestAppURLNativeHTTPOmitsInternalPort(t *testing.T) {
	cfg := config.Config{
		Addr:       "127.0.0.1:8080",
		PublicBase: "http://app.localhost",
		EdgeDomain: "app.localhost",
	}
	got := AppURL(cfg, "svc_abc", 21000)
	if got != "http://svc-abc.app.localhost" {
		t.Fatalf("got %s", got)
	}
}

func TestAppURLFallsBackToPort(t *testing.T) {
	cfg := config.Config{
		PublicBase: "http://127.0.0.1",
		EdgeDomain: "",
	}
	got := AppURL(cfg, "svc_abc", 21000)
	if got != "http://127.0.0.1:21000" {
		t.Fatalf("got %s", got)
	}
}

func TestAppURLUsesProjectName(t *testing.T) {
	cfg := config.Config{
		PublicBase: "http://deploy.salesradar.live",
		EdgeDomain: "deploy.salesradar.live",
	}
	got := AppURL(cfg, "svc_2407e43fd0de3211", 21000, "next-frontend-boilerplate", "Test")
	if got != "https://next-frontend.deploy.salesradar.live" {
		t.Fatalf("got %s", got)
	}
}

func TestAppURLUsesOneLevelEdgeHost(t *testing.T) {
	cfg := config.Config{
		PublicBase: "http://deploy.salesradar.live",
		EdgeDomain: "salesradar.live",
	}
	got := AppURL(cfg, "svc_2407e43fd0de3211", 21000, "next-frontend-boilerplate", "Test")
	if got != "https://next-frontend.salesradar.live" {
		t.Fatalf("got %s", got)
	}
}

func TestAppURLPathPrefixOmitsPort(t *testing.T) {
	cfg := config.Config{
		PublicBase:    "http://deploy.salesradar.live",
		EdgeDomain:    "deploy.salesradar.live",
		AppPathPrefix: "/apps",
	}
	got := AppURL(cfg, "svc_2407e43fd0de3211", 21000, "Test")
	if got != "https://deploy.salesradar.live/apps/test" {
		t.Fatalf("got %s", got)
	}
}

func TestParseAppPath(t *testing.T) {
	slug, rest, ok := ParseAppPath("/apps/test/login", "/apps")
	if !ok || slug != "test" || rest != "/login" {
		t.Fatalf("got %q %q %v", slug, rest, ok)
	}
	if _, _, ok := ParseAppPath("/deployments/abc", "/apps"); ok {
		t.Fatal("dashboard paths must stay on the UI")
	}
}

func TestPublicSlugPrefersName(t *testing.T) {
	if got := PublicSlug("Next Frontend", "svc_abc"); got != "next-frontend" {
		t.Fatalf("got %s", got)
	}
	if got := PublicSlug("Service", "svc_abc"); got != "svc-abc" {
		t.Fatalf("generic title should fall back, got %s", got)
	}
}

func TestPublicSlugStripsBoilerplate(t *testing.T) {
	if got := PublicSlug("Next-Frontend-boilerplate", "Test"); got != "next-frontend" {
		t.Fatalf("got %s", got)
	}
	if !MatchesSlug("next-frontend-boilerplate", "Next-Frontend-boilerplate", "Test") {
		t.Fatal("full title must keep routing")
	}
	if !MatchesSlug("test", "Next-Frontend-boilerplate", "Test") {
		t.Fatal("project name must keep routing")
	}
}

func TestPublicSlugAllowsFrontendAndBackend(t *testing.T) {
	if got := PublicSlug("Frontend", "svc_abc"); got != "frontend" {
		t.Fatalf("got %s", got)
	}
	if got := PublicSlug("Backend", "svc_abc"); got != "backend" {
		t.Fatalf("got %s", got)
	}
}

func TestHostSlugsAreUnique(t *testing.T) {
	slugs := HostSlugs([]Named{
		{ID: "svc_a", Title: "Backend"},
		{ID: "svc_b", Title: "Backend"},
		{ID: "svc_c", Title: "Frontend"},
	})
	if slugs["svc_a"] != "backend-"+ShortID("svc_a") {
		t.Fatalf("automatic hostname should include the stable id, got %q", slugs["svc_a"])
	}
	if slugs["svc_b"] != "backend-"+ShortID("svc_b") {
		t.Fatalf("second service should keep its own id, got %q", slugs["svc_b"])
	}
	if slugs["svc_c"] != "frontend-"+ShortID("svc_c") {
		t.Fatalf("distinct title, got %q", slugs["svc_c"])
	}
	if slugs["svc_a"] == slugs["svc_b"] {
		t.Fatal("two backends must not share a hostname")
	}
}

func TestHostSlugFallsBackWhenMissingFromList(t *testing.T) {
	got := HostSlug("svc_new", "Docs", []Named{{ID: "svc_old", Title: "Docs"}})
	if got == "docs" {
		t.Fatal("new service must not steal an existing hostname")
	}
	if got != PublicSlug("Docs-"+ShortID("svc_new"), "svc_new") && got != Slug("svc_new") {
		t.Fatalf("got %s", got)
	}
}

func TestForwardedProtoTrustsLoopback(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "http://svc.apps.company.local/", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("X-Forwarded-Proto", "https")
	if got := ForwardedProto(req); got != "https" {
		t.Fatalf("got %s", got)
	}
}

func TestForwardedProtoIgnoresSpoofedHeader(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "http://svc.apps.company.local/", nil)
	req.RemoteAddr = "192.168.1.20:54321"
	req.Header.Set("X-Forwarded-Proto", "https")
	if got := ForwardedProto(req); got != "http" {
		t.Fatalf("got %s", got)
	}
}

func TestForwardedProtoUsesTLS(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://svc.apps.company.local/", nil)
	req.RemoteAddr = "192.168.1.20:54321"
	req.TLS = &tls.ConnectionState{}
	if got := ForwardedProto(req); got != "https" {
		t.Fatalf("got %s", got)
	}
}

func TestAllowDashboardFrameStripsClickjacking(t *testing.T) {
	header := http.Header{}
	header.Set("X-Frame-Options", "DENY")
	header.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
	AllowDashboardFrame(header, "http://localhost:3000")
	if header.Get("X-Frame-Options") != "" {
		t.Fatal("expected X-Frame-Options to be removed")
	}
	got := header.Values("Content-Security-Policy")
	if len(got) != 2 {
		t.Fatalf("policies=%v", got)
	}
	if got[0] != "default-src 'self'" {
		t.Fatalf("kept %q", got[0])
	}
	if got[1] != "frame-ancestors 'self' http://localhost:3000" {
		t.Fatalf("ancestors %q", got[1])
	}
}
