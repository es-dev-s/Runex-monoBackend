package edge

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"deploy_platform/internal/config"
	"deploy_platform/internal/runtime"
)

// Named is the subset of a service used to assign a unique public hostname.
type Named struct {
	ID            string
	Title         string
	PreferredSlug string // optional user-chosen platform subdomain
}

func Slug(nodeID string) string {
	return runtime.DockerSafe(nodeID)
}

// PublicSlug is the hostname label shown to users. Prefer a short name
// derived from the project or service; fall back to the internal node id.
func PublicSlug(names ...string) string {
	var last string
	for _, slug := range SlugAliases(names...) {
		last = slug
		if ReservedSlug(slug) || genericSlug(slug) {
			continue
		}
		return slug
	}
	if last != "" {
		return last
	}
	return "app"
}

// SlugAliases are every hostname we will route for a service: the short
// display name, the full title, and the node id.
func SlugAliases(names ...string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(slug string) {
		slug = slugify(slug)
		if slug == "" || seen[slug] {
			return
		}
		seen[slug] = true
		out = append(out, slug)
	}
	for _, name := range names {
		raw := slugify(name)
		if trimmed := trimNameSuffixes(raw); trimmed != raw {
			add(trimmed)
		}
		add(raw)
	}
	return out
}

func MatchesSlug(slug string, names ...string) bool {
	slug = slugify(slug)
	if slug == "" {
		return false
	}
	for _, alias := range SlugAliases(names...) {
		if alias == slug {
			return true
		}
	}
	return false
}

func trimNameSuffixes(slug string) string {
	for _, suffix := range []string{"-boilerplate", "-template", "-example", "-starter", "-demo"} {
		slug = strings.TrimSuffix(slug, suffix)
	}
	return strings.Trim(slug, "-")
}

func slugify(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func genericSlug(slug string) bool {
	switch slug {
	case "service", "web", "app", "untitled", "new", "workspace":
		return true
	default:
		return false
	}
}

// ShortID is a stable suffix used when two services would otherwise share a
// hostname. Prefer the last six characters of the docker-safe node id.
func ShortID(nodeID string) string {
	s := strings.TrimPrefix(Slug(nodeID), "svc-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "svc"
	}
	if len(s) > 6 {
		s = s[len(s)-6:]
	}
	return s
}

// HostSlugs assigns a unique public hostname label to each service.
// Automatic names are <title>-<stable id>, so two deploys of the same repo
// never share a subdomain. An explicit platform subdomain is kept as entered.
func HostSlugs(nodes []Named) map[string]string {
	claimed := map[string]bool{}
	out := map[string]string{}
	// First pass: reserve explicit platform subdomains so auto titles cannot steal them.
	for _, node := range nodes {
		slug := NormalizePlatformSlug(node.PreferredSlug)
		if slug == "" || ReservedSlug(slug) {
			continue
		}
		if claimed[slug] {
			continue
		}
		if strings.TrimSpace(node.ID) == "" {
			continue
		}
		claimed[slug] = true
		out[node.ID] = slug
	}
	for _, node := range nodes {
		if strings.TrimSpace(node.ID) == "" {
			continue
		}
		if _, exists := out[node.ID]; exists {
			continue
		}
		slug := PublicSlug(node.Title+"-"+ShortID(node.ID), node.ID)
		if claimed[slug] || ReservedSlug(slug) {
			slug = Slug(node.ID)
		}
		claimed[slug] = true
		out[node.ID] = slug
	}
	return out
}

// NormalizePlatformSlug lowercases and slugifies a user-chosen platform subdomain.
// Empty means "clear / use auto". Invalid characters are stripped; result may be empty.
func NormalizePlatformSlug(raw string) string {
	return slugify(raw)
}

// ValidatePlatformSlug returns a normalized subdomain or an error for reserved/invalid values.
func ValidatePlatformSlug(raw string) (string, error) {
	slug := NormalizePlatformSlug(raw)
	if slug == "" {
		return "", fmt.Errorf("subdomain is required")
	}
	if len(slug) < 2 {
		return "", fmt.Errorf("subdomain must be at least 2 characters")
	}
	if len(slug) > 48 {
		return "", fmt.Errorf("subdomain must be at most 48 characters")
	}
	if ReservedSlug(slug) || genericSlug(slug) {
		return "", fmt.Errorf("that subdomain is reserved by Runex")
	}
	if slug[0] == '-' || slug[len(slug)-1] == '-' {
		return "", fmt.Errorf("subdomain cannot start or end with a hyphen")
	}
	return slug, nil
}

// HostSlug is the canonical hostname label for one service among its siblings.
func HostSlug(id, title string, nodes []Named) string {
	if strings.TrimSpace(id) == "" {
		return PublicSlug(title)
	}
	found := false
	for _, node := range nodes {
		if node.ID == id {
			found = true
			break
		}
	}
	if !found {
		nodes = append(append([]Named(nil), nodes...), Named{ID: id, Title: title})
	}
	if slug := HostSlugs(nodes)[id]; slug != "" {
		return slug
	}
	return PublicSlug(title, id)
}

func ParseHost(host, domain string) (slug string, ok bool) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return "", false
	}
	host = strings.ToLower(strings.TrimSpace(host))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	suffix := "." + domain
	if !strings.HasSuffix(host, suffix) {
		return "", false
	}
	slug = strings.TrimSuffix(host, suffix)
	if slug == "" || strings.Contains(slug, ".") {
		return "", false
	}
	return slug, true
}

func ParseAnyHost(host string, domains ...string) (slug string, ok bool) {
	for _, domain := range domains {
		if slug, ok = ParseHost(host, domain); ok {
			return slug, true
		}
	}
	return "", false
}

// ReservedSlug is a platform hostname (dashboard, API) that must never be
// treated as a deployed app, even when it sits on EDGE_DOMAIN.
func ReservedSlug(slug string) bool {
	switch strings.ToLower(strings.TrimSpace(slug)) {
	case "www", "api", "app", "apps", "mail", "ftp", "harbor", "runex", "dashboard",
		"deploy", "proxy", "health", "healthz", "static", "assets", "v1",
		"designflow", "go", "production", "quality", "relay", "time",
		"cname", "custom", "edge":
		return true
	default:
		return false
	}
}

func AppURL(cfg config.Config, nodeID string, port int, names ...string) string {
	slug := PublicSlug(append(names, nodeID)...)
	if prefix := strings.TrimSpace(cfg.AppPathPrefix); prefix != "" {
		base := publicOrigin(cfg.PublicBase, cfg.WebURL)
		if base == "" {
			base = "http://127.0.0.1"
		}
		return strings.TrimRight(base, "/") + "/" + strings.Trim(prefix, "/") + "/" + slug
	}
	if strings.TrimSpace(cfg.EdgeDomain) == "" {
		return fmt.Sprintf("%s:%d", strings.TrimRight(cfg.PublicBase, "/"), port)
	}
	return Origin(cfg, slug)
}

// ParseAppPath reads /apps/<slug>/... so a named app is reachable on the
// dashboard host without a wildcard DNS record.
func ParseAppPath(path, prefix string) (slug, rest string, ok bool) {
	prefix = "/" + strings.Trim(strings.TrimSpace(prefix), "/")
	if prefix == "/" {
		return "", "", false
	}
	p := path
	if q := strings.IndexByte(p, '?'); q >= 0 {
		p = p[:q]
	}
	if p != prefix && !strings.HasPrefix(p, prefix+"/") {
		return "", "", false
	}
	tail := strings.TrimPrefix(p, prefix)
	tail = strings.TrimPrefix(tail, "/")
	if tail == "" {
		return "", "", false
	}
	slug, remainder, _ := strings.Cut(tail, "/")
	slug = slugify(slug)
	if slug == "" {
		return "", "", false
	}
	if remainder != "" {
		return slug, "/" + remainder, true
	}
	return slug, "/", true
}

func Origin(cfg config.Config, slug string) string {
	base := strings.TrimRight(strings.TrimSpace(cfg.PublicBase), "/")
	parsed, err := url.Parse(base)
	scheme := "http"
	if err == nil && strings.EqualFold(parsed.Scheme, "https") {
		scheme = "https"
	}
	if publicTLSHost(cfg.EdgeDomain) || publicTLSHost(hostnameOf(base)) {
		scheme = "https"
	}
	host := slug + "." + strings.TrimSpace(cfg.EdgeDomain)
	port := ""
	if err == nil {
		port = parsed.Port()
	}
	if port == "" && scheme != "https" {
		hostName := ""
		if err == nil {
			hostName = parsed.Hostname()
		}
		if hostName == "" || net.ParseIP(hostName) != nil {
			port = listenPort(cfg.Addr)
		}
	}
	if port != "" && port != "80" && port != "443" {
		host = net.JoinHostPort(host, port)
	}
	return scheme + "://" + host
}

func publicOrigin(raws ...string) string {
	for _, raw := range raws {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host == "" {
			if !strings.Contains(raw, "://") {
				parsed, err = url.Parse("http://" + raw)
			}
			if err != nil || parsed.Host == "" {
				continue
			}
		}
		scheme := parsed.Scheme
		if scheme == "" {
			scheme = "http"
		}
		if publicTLSHost(parsed.Hostname()) {
			scheme = "https"
		}
		return scheme + "://" + parsed.Host
	}
	return ""
}

func publicTLSHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return false
	}
	if net.ParseIP(host) != nil {
		return false
	}
	return strings.Contains(host, ".")
}

func hostnameOf(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Hostname() == "" {
		return ""
	}
	return parsed.Hostname()
}

func listenPort(addr string) string {
	_, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err == nil {
		return port
	}
	return "8080"
}

func Loopback(remote string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(remote))
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func ForwardedProto(r *http.Request) string {
	if r == nil {
		return "http"
	}
	if Loopback(r.RemoteAddr) {
		proto := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")))
		if i := strings.IndexByte(proto, ','); i >= 0 {
			proto = strings.TrimSpace(proto[:i])
		}
		if proto == "https" || proto == "http" {
			return proto
		}
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func AllowDashboardFrame(header http.Header, webURL string) {
	if header == nil {
		return
	}
	header.Del("X-Frame-Options")
	policies := header.Values("Content-Security-Policy")
	header.Del("Content-Security-Policy")
	for _, policy := range policies {
		if next := stripCSPDirective(policy, "frame-ancestors"); next != "" {
			header.Add("Content-Security-Policy", next)
		}
	}
	header.Add("Content-Security-Policy", FrameAncestors(webURL))
}

func FrameAncestors(webURL string) string {
	origin := strings.TrimRight(strings.TrimSpace(webURL), "/")
	if origin == "" {
		return "frame-ancestors 'self'"
	}
	return "frame-ancestors 'self' " + origin
}

func stripCSPDirective(policy, name string) string {
	var kept []string
	for _, part := range strings.Split(policy, ";") {
		dir := strings.TrimSpace(part)
		if dir == "" {
			continue
		}
		key := dir
		if i := strings.IndexAny(dir, " \t"); i >= 0 {
			key = dir[:i]
		}
		if strings.EqualFold(key, name) {
			continue
		}
		kept = append(kept, dir)
	}
	return strings.Join(kept, "; ")
}
