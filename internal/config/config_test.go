package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOriginURLAndHome(t *testing.T) {
	if got := originURL("deployment-frontend-production.up.railway.app"); got != "https://deployment-frontend-production.up.railway.app" {
		t.Fatalf("web url %s", got)
	}
	if got := originURL("http://localhost:3000"); got != "http://localhost:3000" {
		t.Fatalf("localhost %s", got)
	}
	if got := expandHome("/abs/key.pem"); got != "/abs/key.pem" {
		t.Fatalf("abs %s", got)
	}
}

// The previous implementation rewrote the control-plane Redis URL to port 6379,
// which is the one nothing listens on, so every boot burned five failed dials.
func TestLoadRedisURLHonoursConfiguredValue(t *testing.T) {
	t.Setenv("REDIS_URL", "")

	t.Setenv("PLATFORM_REDIS_URL", "redis://127.0.0.1:56379/0")
	if got := loadRedisURL(); got != "redis://127.0.0.1:56379/0" {
		t.Fatalf("configured control-plane redis was rewritten to %q", got)
	}

	t.Setenv("PLATFORM_REDIS_URL", "redis://cache.internal:6379/2")
	if got := loadRedisURL(); got != "redis://cache.internal:6379/2" {
		t.Fatalf("external redis was rewritten to %q", got)
	}

	t.Setenv("PLATFORM_REDIS_URL", "")
	if got := loadRedisURL(); got != IsolatedRedisURL {
		t.Fatalf("default should be the provisioned control-plane redis, got %q", got)
	}
}

func TestValidateFlagsUnreadableGitHubKey(t *testing.T) {
	cfg := Config{
		DatabaseURL:            "postgres://x/y",
		PortMin:                21000,
		PortMax:                21999,
		GitHubAppID:            "123",
		GitHubAppWebhookSecret: "shh",
		GitHubAppPrivateKey:    "/nonexistent/key.pem",
		WebURL:                 "http://localhost:3000",
		PublicURL:              "https://tunnel.example.dev",
	}
	if !hasProblem(cfg.Validate(), "cannot be read") {
		t.Fatalf("a missing App key must be reported: %v", cfg.Validate())
	}

	dir := t.TempDir()
	key := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(key, []byte("-----BEGIN RSA PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.GitHubAppPrivateKey = key
	if problems := cfg.Validate(); len(problems) != 0 {
		t.Fatalf("valid config reported problems: %v", problems)
	}

	cfg.GitHubAppPrivateKey = "-----BEGIN RSA PRIVATE KEY-----\nabc\n"
	if problems := cfg.Validate(); len(problems) != 0 {
		t.Fatalf("inline PEM should be accepted: %v", problems)
	}
}

// Leaving PublicURL equal to WebURL points GitHub at the dashboard, so pushes
// never reach this API and auto-deploy silently does nothing.
func TestValidateFlagsWebhookPointingAtDashboard(t *testing.T) {
	cfg := Config{
		DatabaseURL:            "postgres://x/y",
		PortMin:                21000,
		PortMax:                21999,
		GitHubAppID:            "123",
		GitHubAppWebhookSecret: "shh",
		GitHubAppPrivateKey:    "-----BEGIN RSA PRIVATE KEY-----\n",
		WebURL:                 "http://localhost:3000",
		PublicURL:              "http://localhost:3000",
	}
	if !hasProblem(cfg.Validate(), "PLATFORM_PUBLIC_URL equals PLATFORM_WEB_URL") {
		t.Fatalf("webhook misdirection must be reported: %v", cfg.Validate())
	}

	cfg.WebURL = "https://aivex.art"
	cfg.PublicURL = "https://aivex.art"
	if hasProblem(cfg.Validate(), "PLATFORM_PUBLIC_URL equals PLATFORM_WEB_URL") {
		t.Fatalf("a shared production origin is valid when Caddy sends /v1 to this API: %v", cfg.Validate())
	}
}

func TestValidateFlagsInvertedPortRange(t *testing.T) {
	cfg := Config{DatabaseURL: "postgres://x/y", PortMin: 30000, PortMax: 21000}
	if !hasProblem(cfg.Validate(), "must be below") {
		t.Fatalf("inverted port range must be reported: %v", cfg.Validate())
	}
}

func TestListenAddrKeepsLoopback(t *testing.T) {
	if got := listenAddr("127.0.0.1:8080"); got != "127.0.0.1:8080" {
		t.Fatalf("loopback was rewritten to %s", got)
	}
	if got := listenAddr("0.0.0.0:8080"); got != "0.0.0.0:8080" {
		t.Fatalf("all-interfaces bind %s", got)
	}
	if got := listenAddr(":8080"); got != "0.0.0.0:8080" {
		t.Fatalf("empty host should mean all interfaces, got %s", got)
	}
}

func TestProductionRejectsLaptopOrigins(t *testing.T) {
	cfg := Config{
		Env:                    "production",
		DatabaseURL:            "postgres://x/y",
		PortMin:                21000,
		PortMax:                21999,
		GitHubAppID:            "123",
		GitHubAppWebhookSecret: "shh",
		GitHubAppPrivateKey:    "-----BEGIN RSA PRIVATE KEY-----\n",
		WebURL:                 "http://localhost:3000",
		PublicURL:              "https://example.ngrok-free.dev",
		PublicBase:             "http://aivex.art",
		EdgeDomain:             "aivex.art",
		RedisURL:               "redis://redis.railway.internal:6379",
	}
	problems := cfg.Validate()
	for _, needle := range []string{
		"PLATFORM_WEB_URL is still a local origin",
		"must be https in production so session cookies are Secure",
		"still points at ngrok",
		"PLATFORM_REDIS_URL uses *.railway.internal",
		"PLATFORM_PUBLIC_BASE must be https",
	} {
		if !hasProblem(problems, needle) {
			t.Fatalf("missing %q in %v", needle, problems)
		}
	}

	cfg.WebURL = "https://aivex.art"
	cfg.PublicURL = "https://api.aivex.art"
	cfg.PublicBase = "https://aivex.art"
	cfg.RedisURL = IsolatedRedisURL
	if problems := cfg.Validate(); len(problems) != 0 {
		t.Fatalf("valid production config reported %v", problems)
	}

	cfg.PublicURL = "https://aivex.art"
	if !hasProblem(cfg.Validate(), "GitHub webhooks would hit Railway") {
		t.Fatal("apex PUBLIC_URL must be rejected when the dashboard is on Railway")
	}
}

func TestSharedCookieDomain(t *testing.T) {
	if got := SharedCookieDomain("https://aivex.art", "https://api.aivex.art"); got != "aivex.art" {
		t.Fatalf("parent domain %q", got)
	}
	if got := SharedCookieDomain("http://localhost:3000", "https://api.aivex.art"); got != "" {
		t.Fatalf("loopback must not set Domain, got %q", got)
	}
	if got := SharedCookieDomain("https://harbor.up.railway.app", "https://api.aivex.art"); got != "" {
		t.Fatalf("cross-site must not set Domain, got %q", got)
	}
	if !CrossSiteCookies("https://harbor.up.railway.app", "https://api.aivex.art") {
		t.Fatal("railway.app vs aivex.art is cross-site")
	}
	if CrossSiteCookies("https://aivex.art", "https://api.aivex.art") {
		t.Fatal("sibling subdomains are same-site")
	}
	if got := SharedCookieDomain("http://deploy.salesradar.live", "http://deploy.salesradar.live"); got != "" {
		t.Fatalf("same host must stay host-only, got %q", got)
	}
}

func TestDomainCNAMEPrefersPublishedEdge(t *testing.T) {
	cfg := Config{EdgeDomain: "salesradar.live", TunnelCNAME: "abc.cfargotunnel.com", CNAMETarget: "cname.salesradar.live"}
	if got := cfg.DomainCNAME(); got != "cname.salesradar.live" {
		t.Fatalf("published edge %s", got)
	}
	cfg.CNAMETarget = ""
	if got := cfg.DomainCNAME(); got != "cname.salesradar.live" {
		t.Fatalf("derived edge %s", got)
	}
	targets := cfg.DomainCNAMETargets()
	if len(targets) < 2 || targets[0] != "cname.salesradar.live" || targets[1] != "abc.cfargotunnel.com" {
		t.Fatalf("targets %v", targets)
	}
	cfg.EdgeDomain = "app.localhost"
	cfg.TunnelCNAME = ""
	if got := cfg.DomainCNAME(); got != "" {
		t.Fatalf("localhost should not invent a public CNAME, got %s", got)
	}
}

func TestCustomDomainOriginLoopback(t *testing.T) {
	cfg := Config{Addr: "0.0.0.0:18080"}
	if got := cfg.CustomDomainOrigin(); got != "http://127.0.0.1:18080" {
		t.Fatalf("origin %s", got)
	}
}

func TestProductionDetection(t *testing.T) {
	for _, env := range []string{"production", "prod"} {
		if !(Config{Env: env}).Production() {
			t.Fatalf("%q should be production", env)
		}
	}
	for _, env := range []string{"", "development", "staging"} {
		if (Config{Env: env}).Production() {
			t.Fatalf("%q should not be production", env)
		}
	}
}

func hasProblem(problems []error, needle string) bool {
	for _, problem := range problems {
		if strings.Contains(problem.Error(), needle) {
			return true
		}
	}
	return false
}
