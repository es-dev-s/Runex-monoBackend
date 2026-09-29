package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const IsolatedDatabaseURL = "postgres://deploy:deploy@127.0.0.1:55432/deploy?sslmode=disable"
const IsolatedRedisURL = "redis://127.0.0.1:56379/0"
const HostRedisURL = "redis://127.0.0.1:6379/0"

type Config struct {
	Env        string
	Addr       string
	PublicBase string
	EdgeDomain string
	LanDomain  string
	// AppPathPrefix, when set (for example "/apps"), publishes clean URLs on
	// the dashboard host instead of slug.EdgeDomain. Use it when wildcard DNS
	// for the edge domain is not yet in place.
	AppPathPrefix          string
	WebURL                 string
	PublicURL              string
	DatabaseURL            string
	RedisURL               string
	WorkDir                string
	DetectorURL            string
	GitHubToken            string
	GitHubAppID            string
	GitHubAppSlug          string
	GitHubAppClientID      string
	GitHubAppClientSecret  string
	GitHubAppPrivateKey    string
	GitHubAppWebhookSecret string
	CORSOrigins            []string
	MaxUploadBytes         int64
	MaxUncompressed        int64
	MaxZipFiles            int
	CloneTimeout           time.Duration
	BuildTimeout           time.Duration
	StartTimeout           time.Duration
	PortMin                int
	PortMax                int
	MemoryLimit            string
	CPULimit               string
	MaxMemoryMB            int
	MaxDiskMB              int
	// AllowHTTP permits http:// dashboard/API origins in production. Use on
	// a private LAN where TLS cannot be issued (reserved/private A records).
	AllowHTTP bool
	// AllowPublicRegister enables POST /v1/auth/register. Set false to invite-only.
	AllowPublicRegister bool
	// VaultKey is an optional 32-byte hex/base64 root key that wraps each
	// user's vault DEK. When empty, the API stores a generated key in WorkDir.
	VaultKey string
	// CNAMETarget is the hostname customers should CNAME to. For production
	// this is cname.<EdgeDomain>, the Cloudflare for SaaS target.
	CNAMETarget string
	// TunnelCNAME is the Cloudflare tunnel hostname
	// (<id>.cfargotunnel.com). Same-account DNS only; customers on other
	// Cloudflare accounts must CNAME to CNAMETarget after SaaS authorize.
	TunnelCNAME string
	// DomainOrigin is where tunnel public hostnames should send traffic
	// (the Runex API, not mesh nginx). Empty means loopback of Addr.
	DomainOrigin        string
	CloudflareAPIToken  string
	CloudflareAccountID string
	CloudflareTunnelID  string
	CloudflareZoneID    string
}

func Load() Config {
	loadDotEnv()
	return Config{
		Env:                    strings.ToLower(env("PLATFORM_ENV", "development")),
		Addr:                   listenAddr(env("PLATFORM_ADDR", "0.0.0.0:8080")),
		PublicBase:             env("PLATFORM_PUBLIC_BASE", "http://app.localhost"),
		EdgeDomain:             envOptional("PLATFORM_EDGE_DOMAIN", "app.localhost"),
		LanDomain:              envOptional("PLATFORM_LAN_DOMAIN", ""),
		AppPathPrefix:          envOptional("PLATFORM_APP_PATH_PREFIX", ""),
		WebURL:                 originURL(env("PLATFORM_WEB_URL", "http://localhost:3000")),
		PublicURL:              originURL(env("PLATFORM_PUBLIC_URL", env("PLATFORM_WEB_URL", "http://localhost:3000"))),
		DatabaseURL:            env("PLATFORM_DATABASE_URL", IsolatedDatabaseURL),
		RedisURL:               loadRedisURL(),
		WorkDir:                env("PLATFORM_WORKDIR", "data"),
		DetectorURL:            env("DETECTION_ENGINE_URL", "http://127.0.0.1:8090"),
		GitHubToken:            env("GITHUB_TOKEN", os.Getenv("GITHUB_TOKEN")),
		GitHubAppID:            env("GITHUB_APP_ID", ""),
		GitHubAppSlug:          env("GITHUB_APP_SLUG", ""),
		GitHubAppClientID:      env("GITHUB_APP_CLIENT_ID", ""),
		GitHubAppClientSecret:  env("GITHUB_APP_CLIENT_SECRET", ""),
		GitHubAppPrivateKey:    expandHome(env("GITHUB_APP_PRIVATE_KEY", "")),
		GitHubAppWebhookSecret: env("GITHUB_APP_WEBHOOK_SECRET", ""),
		CORSOrigins:            splitCSV(env("PLATFORM_CORS_ORIGINS", "")),
		MaxUploadBytes:         envInt64("PLATFORM_MAX_UPLOAD_BYTES", 80<<20),
		MaxUncompressed:        envInt64("PLATFORM_MAX_UNCOMPRESSED_BYTES", 400<<20),
		MaxZipFiles:            envInt("PLATFORM_MAX_ZIP_FILES", 20000),
		CloneTimeout:           envDuration("PLATFORM_CLONE_TIMEOUT", 60*time.Second),
		BuildTimeout:           envDuration("PLATFORM_BUILD_TIMEOUT", 8*time.Minute),
		StartTimeout:           envDuration("PLATFORM_START_TIMEOUT", 45*time.Second),
		PortMin:                envInt("PLATFORM_PORT_MIN", 21000),
		PortMax:                envInt("PLATFORM_PORT_MAX", 21999),
		MemoryLimit:            env("PLATFORM_MEMORY_LIMIT", "512m"),
		CPULimit:               env("PLATFORM_CPU_LIMIT", "1.0"),
		MaxMemoryMB:            envInt("PLATFORM_MAX_MEMORY_MB", 4096),
		MaxDiskMB:              envInt("PLATFORM_MAX_DISK_MB", 20480),
		AllowHTTP:              envBool("PLATFORM_ALLOW_HTTP", false),
		AllowPublicRegister:    envBool("PLATFORM_ALLOW_PUBLIC_REGISTER", true),
		VaultKey:               env("PLATFORM_VAULT_KEY", ""),
		CNAMETarget:            envOptional("PLATFORM_CNAME_TARGET", ""),
		TunnelCNAME:            firstNonEmpty(envOptional("PLATFORM_TUNNEL_CNAME", ""), tunnelCNAMEFromID(envOptional("CLOUDFLARE_TUNNEL_ID", ""))),
		DomainOrigin:           envOptional("PLATFORM_DOMAIN_ORIGIN", ""),
		CloudflareAPIToken:     firstNonEmpty(envOptional("CLOUDFLARE_API_TOKEN", ""), envOptional("CF_API_TOKEN", "")),
		CloudflareAccountID:    envOptional("CLOUDFLARE_ACCOUNT_ID", ""),
		CloudflareTunnelID:     envOptional("CLOUDFLARE_TUNNEL_ID", ""),
		CloudflareZoneID:       envOptional("CLOUDFLARE_ZONE_ID", ""),
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func tunnelCNAMEFromID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	return strings.ToLower(id) + ".cfargotunnel.com"
}

func normalizeCNAME(raw string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
}

func isTunnelHostname(host string) bool {
	return strings.HasSuffix(normalizeCNAME(host), ".cfargotunnel.com")
}

// DomainCNAME is the hostname customers should CNAME to. Prefer the
// SaaS target (cname.<EdgeDomain>) so orange-cloud records from any
// Cloudflare account can be authorized as custom hostnames.
func (c Config) DomainCNAME() string {
	if target := normalizeCNAME(c.CNAMETarget); target != "" && !isTunnelHostname(target) {
		return target
	}
	edge := strings.ToLower(strings.TrimSpace(c.EdgeDomain))
	if edge != "" && !strings.Contains(edge, "localhost") {
		return "cname." + edge
	}
	if target := normalizeCNAME(c.CNAMETarget); target != "" {
		return target
	}
	return normalizeCNAME(c.TunnelCNAME)
}

// DomainCNAMETargets are every hostname we accept as a valid routing
// record: the advertised CNAME plus the tunnel hostname for grey-cloud
// setups that already pointed at *.cfargotunnel.com.
func (c Config) DomainCNAMETargets() []string {
	seen := map[string]bool{}
	var out []string
	add := func(raw string) {
		host := normalizeCNAME(raw)
		if host == "" || seen[host] {
			return
		}
		seen[host] = true
		out = append(out, host)
	}
	add(c.DomainCNAME())
	add(c.CNAMETarget)
	add(c.TunnelCNAME)
	return out
}

// CustomDomainOrigin is the loopback URL Cloudflare Tunnel should use for a
// verified custom hostname. Sending these at the Runex API (not nginx :80)
// keeps mesh vhosts isolated from customer Host headers.
func (c Config) CustomDomainOrigin() string {
	if origin := strings.TrimSpace(c.DomainOrigin); origin != "" {
		return strings.TrimRight(origin, "/")
	}
	host, port, err := net.SplitHostPort(strings.TrimSpace(c.Addr))
	if err != nil || port == "" {
		return "http://127.0.0.1:18080"
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// Production reports whether the platform is running for real users. In
// production the process must refuse to start on a misconfiguration rather than
// silently degrade onto a different database or a disabled GitHub App.
func (c Config) Production() bool {
	return c.Env == "production" || c.Env == "prod"
}

// Validate returns every configuration problem that would produce confusing
// runtime behaviour. Callers decide whether these are fatal (production) or
// warnings (development).
//
// The checks here exist because each of these has silently broken a deploy:
// a database URL that falls back to a throwaway instance, a GitHub App key
// path that does not exist so no installation token can be minted, and a
// webhook URL pointing at the dashboard instead of this API.
func (c Config) Validate() []error {
	var problems []error

	if strings.TrimSpace(c.DatabaseURL) == "" {
		problems = append(problems, fmt.Errorf("PLATFORM_DATABASE_URL is empty"))
	}
	if c.PortMin >= c.PortMax {
		problems = append(problems, fmt.Errorf("PLATFORM_PORT_MIN (%d) must be below PLATFORM_PORT_MAX (%d)", c.PortMin, c.PortMax))
	}

	// A configured GitHub App that cannot mint tokens is worse than no app at
	// all: the UI offers "Deploy from GitHub" and every attempt fails.
	if strings.TrimSpace(c.GitHubAppID) != "" {
		if err := c.checkGitHubKey(); err != nil {
			problems = append(problems, err)
		}
		if strings.TrimSpace(c.GitHubAppWebhookSecret) == "" {
			problems = append(problems, fmt.Errorf("GITHUB_APP_WEBHOOK_SECRET is empty, so every push webhook will be rejected"))
		}
	}

	// Same origin is valid in production when Caddy sends /v1 to this API
	// (https://aivex.art). It is wrong when both still point at the Next
	// dev server, because GitHub would POST webhooks at the dashboard.
	if strings.TrimSpace(c.GitHubAppID) != "" && c.PublicURL == c.WebURL {
		host := strings.ToLower(c.PublicURL)
		if strings.Contains(host, "localhost") || strings.Contains(host, "127.0.0.1") || strings.Contains(host, ":3000") {
			problems = append(problems, fmt.Errorf(
				"PLATFORM_PUBLIC_URL equals PLATFORM_WEB_URL (%s); point it at a tunnel or domain that reaches this API so GitHub can deliver %s/v1/github/webhook",
				c.PublicURL, c.PublicURL))
		}
	}

	if c.Production() {
		problems = append(problems, c.validateProduction()...)
	}

	return problems
}

func (c Config) validateProduction() []error {
	var problems []error
	web := strings.ToLower(c.WebURL)
	pub := strings.ToLower(c.PublicURL)
	if strings.Contains(web, "localhost") || strings.Contains(web, "127.0.0.1") || strings.Contains(web, ":3000") {
		problems = append(problems, fmt.Errorf("PLATFORM_WEB_URL is still a local origin (%s); on the VPS it must be the public dashboard https://aivex.art", c.WebURL))
	}
	if strings.HasPrefix(web, "http://") && !c.AllowHTTP {
		problems = append(problems, fmt.Errorf("PLATFORM_WEB_URL must be https in production so session cookies are Secure"))
	}
	if strings.Contains(pub, "ngrok") {
		problems = append(problems, fmt.Errorf("PLATFORM_PUBLIC_URL still points at ngrok; GitHub webhooks must hit https://api.aivex.art/v1/github/webhook"))
	}
	if hostnameOf(c.PublicURL) == "aivex.art" || hostnameOf(c.PublicURL) == "www.aivex.art" {
		if hostnameOf(c.WebURL) == hostnameOf(c.PublicURL) {
			problems = append(problems, fmt.Errorf("PLATFORM_PUBLIC_URL is the dashboard origin (%s); GitHub webhooks would hit Railway. Use https://api.aivex.art", c.PublicURL))
		}
	}
	if strings.Contains(c.DatabaseURL, "railway.internal") {
		problems = append(problems, fmt.Errorf("PLATFORM_DATABASE_URL uses *.railway.internal which does not resolve on the VPS; use the public Railway TCP proxy"))
	}
	if strings.Contains(c.RedisURL, "railway.internal") {
		problems = append(problems, fmt.Errorf("PLATFORM_REDIS_URL uses *.railway.internal which does not resolve on the VPS; use loopback redis://127.0.0.1:56379/0"))
	}
	base := strings.ToLower(c.PublicBase)
	if strings.HasPrefix(base, "http://") && c.EdgeDomain != "" && !strings.Contains(c.EdgeDomain, "localhost") && !c.AllowHTTP {
		problems = append(problems, fmt.Errorf("PLATFORM_PUBLIC_BASE must be https in production so app URLs are https://<id>.%s", c.EdgeDomain))
	}
	return problems
}

// checkGitHubKey accepts either inline PEM contents or a readable file path.
func (c Config) checkGitHubKey() error {
	key := strings.TrimSpace(c.GitHubAppPrivateKey)
	if key == "" {
		return fmt.Errorf("GITHUB_APP_ID is set but GITHUB_APP_PRIVATE_KEY is empty")
	}
	if strings.Contains(key, "PRIVATE KEY") {
		return nil
	}
	info, err := os.Stat(key)
	if err != nil {
		return fmt.Errorf("GITHUB_APP_PRIVATE_KEY points at %s which cannot be read (%v); GitHub clones and auto-deploy will fail", key, err)
	}
	if info.IsDir() {
		return fmt.Errorf("GITHUB_APP_PRIVATE_KEY points at directory %s, expected a .pem file", key)
	}
	return nil
}

func originURL(raw string) string {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "http://localhost:3000"
	}
	if strings.Contains(raw, "://") {
		return raw
	}
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "localhost") || strings.HasPrefix(lower, "127.0.0.1") {
		return "http://" + raw
	}
	return "https://" + raw
}

func expandHome(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "~" || strings.HasPrefix(raw, "~/") {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return raw
		}
		if raw == "~" {
			return home
		}
		return filepath.Join(home, strings.TrimPrefix(raw, "~/"))
	}
	return raw
}

func listenAddr(addr string) string {
	host, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil || port == "" {
		return "0.0.0.0:8080"
	}
	// Empty host means "all interfaces". Loopback stays loopback so Caddy can
	// be the only public listener on the VPS.
	if host == "" {
		host = "0.0.0.0"
	}
	return net.JoinHostPort(host, port)
}

// loadRedisURL honours whatever the operator configured. The default is the
// dedicated control-plane Redis that `make redis` provisions on 56379, which is
// deliberately off 6379 so it can never collide with a project sidecar.
func loadRedisURL() string {
	if value := strings.TrimSpace(os.Getenv("PLATFORM_REDIS_URL")); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv("REDIS_URL")); value != "" {
		return value
	}
	return IsolatedRedisURL
}

func envBool(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	switch strings.ToLower(value) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envOptional(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return strings.TrimSpace(value)
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
			return parsed
		}
	}
	return fallback
}

func envInt64(key string, fallback int64) int64 {
	if value := os.Getenv(key); value != "" {
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil && parsed > 0 {
			return parsed
		}
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil && parsed > 0 {
			return parsed
		}
	}
	return fallback
}

func splitCSV(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, originURL(part))
	}
	return out
}

func hostnameOf(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}

func loopbackHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// RegistrableDomain is the parent used for cookie Domain (aivex.art from api.aivex.art).
func RegistrableDomain(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" || loopbackHost(host) || net.ParseIP(host) != nil {
		return ""
	}
	parts := strings.Split(host, ".")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-2] + "." + parts[len(parts)-1]
}

// SharedCookieDomain is set when the Railway dashboard and VPS API share a
// parent domain, so platform_session can be first-party on both hosts.
func SharedCookieDomain(webURL, publicURL string) string {
	web, pub := hostnameOf(webURL), hostnameOf(publicURL)
	if web == "" || pub == "" || web == pub || loopbackHost(web) || loopbackHost(pub) {
		return ""
	}
	a, b := RegistrableDomain(web), RegistrableDomain(pub)
	if a == "" || a != b {
		return ""
	}
	return a
}

// CrossSiteCookies is true when the dashboard and API are different sites
// (Railway default hostname vs api.aivex.art). Those cookies must be SameSite=None.
func CrossSiteCookies(webURL, publicURL string) bool {
	web, pub := hostnameOf(webURL), hostnameOf(publicURL)
	if web == "" || pub == "" || loopbackHost(web) || loopbackHost(pub) {
		return false
	}
	if SharedCookieDomain(webURL, publicURL) != "" {
		return false
	}
	return web != pub
}
