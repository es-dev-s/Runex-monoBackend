// Package customdomain validates customer hostnames, builds the DNS records
// they must create, and checks those records before Runex starts routing.
//
// A hostname is never routed until the TXT ownership token is present. The
// CNAME/ALIAS (or flattened A/AAAA match) is required before status=active so
// a verified-but-unpointed domain cannot steal traffic from another site.
package customdomain

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
	"unicode"

	"deploy_platform/internal/config"
	"deploy_platform/internal/edge"
)

const (
	StatusPending    = "pending"
	StatusVerifying  = "verifying"
	StatusActive     = "active"
	StatusError      = "error"
	ChallengePrefix  = "_harbor-challenge"
	TokenPrefix      = "harbor-verify="
	MaxPerUser       = 20
	MaxPerNode       = 8
	MaxHostnameBytes = 253
)

var (
	ErrInvalid      = errors.New("invalid hostname")
	ErrPlatform     = errors.New("platform hostname")
	ErrTaken        = errors.New("domain taken")
	ErrLimit        = errors.New("domain limit")
	ErrNotService   = errors.New("not a service")
	ErrTXTMissing   = errors.New("txt missing")
	ErrCNAMEMissing = errors.New("cname missing")
)

type Record struct {
	Type     string `json:"type"`
	Host     string `json:"host"`
	Name     string `json:"name"`
	Value    string `json:"value"`
	Purpose  string `json:"purpose"`
	Required bool   `json:"required"`
	Hint     string `json:"hint,omitempty"`
}

type Check struct {
	TXT    bool
	Route  bool
	Active bool
	Error  string
}

type Resolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
	LookupCNAME(ctx context.Context, name string) (string, error)
	LookupHost(ctx context.Context, name string) ([]string, error)
}

type netResolver struct{}

func DefaultResolver() Resolver { return netResolver{} }

func (netResolver) LookupTXT(ctx context.Context, name string) ([]string, error) {
	return net.DefaultResolver.LookupTXT(ctx, name)
}

func (netResolver) LookupCNAME(ctx context.Context, name string) (string, error) {
	return net.DefaultResolver.LookupCNAME(ctx, name)
}

func (netResolver) LookupHost(ctx context.Context, name string) ([]string, error) {
	return net.DefaultResolver.LookupHost(ctx, name)
}

// Normalize accepts a pasted URL or hostname and returns a lowercase FQDN.
func Normalize(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	value = strings.TrimSuffix(value, ".")
	value = strings.TrimPrefix(value, "*.")
	if value == "" {
		return "", ErrInvalid
	}
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", ErrInvalid
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	host = strings.TrimSuffix(host, ".")
	if host == "" || net.ParseIP(host) != nil {
		return "", ErrInvalid
	}
	if len(host) > MaxHostnameBytes || !validFQDN(host) {
		return "", ErrInvalid
	}
	if privateSuffix(host) {
		return "", ErrInvalid
	}
	return host, nil
}

func validFQDN(host string) bool {
	if !strings.Contains(host, ".") {
		return false
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 || len(labels) > 10 {
		return false
	}
	for _, label := range labels {
		if len(label) < 1 || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if r > unicode.MaxASCII {
				return false
			}
			ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
			if !ok {
				return false
			}
		}
	}
	return true
}

func privateSuffix(host string) bool {
	for _, suffix := range []string{".localhost", ".local", ".internal", ".invalid", ".test", ".example", ".lan", ".home", ".corp"} {
		if host == strings.TrimPrefix(suffix, ".") || strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return host == "localhost"
}

// PlatformHost is true for Runex's own dashboard, API, edge slugs, and
// generated *.EdgeDomain names. Those must never be claimed as a custom domain.
func PlatformHost(cfg config.Config, host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return true
	}
	for _, raw := range []string{cfg.EdgeDomain, cfg.LanDomain, hostnameOf(cfg.WebURL), hostnameOf(cfg.PublicURL), hostnameOf(cfg.PublicBase), cfg.CNAMETarget, cfg.TunnelCNAME, cfg.DomainCNAME()} {
		own := strings.ToLower(strings.TrimSpace(raw))
		own = strings.TrimSuffix(own, ".")
		if own == "" {
			continue
		}
		if host == own || strings.HasSuffix(host, "."+own) {
			return true
		}
	}
	if slug, ok := edge.ParseAnyHost(host, cfg.EdgeDomain, cfg.LanDomain); ok {
		return slug != ""
	}
	return false
}

func hostnameOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}

func TokenFromSecret(secret string) string {
	secret = strings.ToLower(strings.TrimSpace(secret))
	if secret == "" {
		return ""
	}
	if strings.HasPrefix(secret, TokenPrefix) {
		return secret
	}
	return TokenPrefix + secret
}

func ChallengeName(hostname string) string {
	return ChallengePrefix + "." + hostname
}

func SplitZone(hostname string) (label, zone string, apex bool) {
	parts := strings.Split(hostname, ".")
	if len(parts) <= 2 {
		return "@", hostname, true
	}
	return strings.Join(parts[:len(parts)-2], "."), strings.Join(parts[len(parts)-2:], "."), false
}

// Records are the unique DNS instructions for one hostname. TXT proves
// ownership; CNAME/ALIAS points the name at Runex's edge.
func Records(hostname, token, cnameTarget string) []Record {
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	token = strings.TrimSpace(token)
	cnameTarget = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(cnameTarget), "."))
	label, _, apex := SplitZone(hostname)
	txtHost := ChallengePrefix
	if !apex && label != "@" {
		txtHost = ChallengePrefix + "." + label
	}
	out := []Record{{
		Type:     "TXT",
		Host:     txtHost,
		Name:     ChallengeName(hostname),
		Value:    token,
		Purpose:  "verify",
		Required: true,
		Hint:     "Proves you control this domain. Runex will not route until this matches.",
	}}
	if cnameTarget == "" {
		return out
	}
	if apex {
		out = append(out, Record{
			Type:     "ALIAS",
			Host:     "@",
			Name:     hostname,
			Value:    cnameTarget,
			Purpose:  "route",
			Required: true,
			Hint:     "Apex names cannot use a CNAME. Use ALIAS, ANAME, or Cloudflare CNAME flattening (Proxied) to this target. Runex authorizes the name so it works from any DNS provider.",
		})
		return out
	}
	out = append(out, Record{
		Type:     "CNAME",
		Host:     label,
		Name:     hostname,
		Value:    cnameTarget,
		Purpose:  "route",
		Required: true,
		Hint:     "In Cloudflare, use a Proxied (orange cloud) CNAME to this target. Runex authorizes it so Error 1014 does not appear.",
	})
	return out
}

func PublicURL(hostname string) string {
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	if hostname == "" {
		return ""
	}
	return "https://" + hostname
}

func TXTMatch(values []string, token string) bool {
	want := strings.ToLower(strings.TrimSpace(token))
	if want == "" {
		return false
	}
	var joined []string
	for _, raw := range values {
		got := strings.ToLower(strings.TrimSpace(raw))
		got = strings.Trim(got, `"'`)
		if got == "" {
			continue
		}
		if got == want {
			return true
		}
		joined = append(joined, got)
	}
	return strings.Join(joined, "") == want
}

func sameHost(a, b string) bool {
	a = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(a), "."))
	b = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(b), "."))
	return a != "" && a == b
}

func Verify(ctx context.Context, resolver Resolver, hostname, token string, cnameTargets ...string) Check {
	if resolver == nil {
		resolver = DefaultResolver()
	}
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 8*time.Second {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
	}
	out := Check{}
	txt, err := resolver.LookupTXT(ctx, ChallengeName(hostname))
	if err == nil && TXTMatch(txt, token) {
		out.TXT = true
	}
	if !out.TXT {
		if extra, extraErr := resolver.LookupTXT(ctx, hostname); extraErr == nil && TXTMatch(extra, token) {
			out.TXT = true
		}
	}
	targets := cleanTargets(cnameTargets)
	if len(targets) == 0 {
		out.Route = out.TXT
	} else {
		out.Route = pointsAt(ctx, resolver, hostname, targets)
	}
	switch {
	case out.TXT && out.Route:
		out.Active = true
	case !out.TXT && !out.Route:
		out.Error = "Add the TXT and routing records Runex generated, then verify again."
	case !out.TXT:
		out.Error = "Add the TXT record so Runex can prove you own this domain."
	default:
		shown := targets[0]
		out.Error = fmt.Sprintf("TXT is present. Point this hostname at %s with a Proxied CNAME.", shown)
	}
	return out
}

func cleanTargets(raw []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, item := range raw {
		host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(item), "."))
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		out = append(out, host)
	}
	return out
}

func pointsAt(ctx context.Context, resolver Resolver, hostname string, targets []string) bool {
	if cname, err := resolver.LookupCNAME(ctx, hostname); err == nil {
		cname = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(cname), "."))
		if cname != "" && !sameHost(cname, hostname) {
			for _, target := range targets {
				if sameHost(cname, target) {
					return true
				}
			}
		}
	}
	got, err := resolver.LookupHost(ctx, hostname)
	if err != nil || len(got) == 0 {
		return false
	}
	if CloudflareProxied(got) {
		return true
	}
	need := map[string]struct{}{}
	for _, target := range targets {
		want, lookErr := resolver.LookupHost(ctx, target)
		if lookErr != nil {
			continue
		}
		for _, ip := range want {
			parsed := net.ParseIP(ip)
			if parsed == nil {
				continue
			}
			need[parsed.String()] = struct{}{}
		}
	}
	for _, ip := range got {
		parsed := net.ParseIP(ip)
		if parsed == nil {
			continue
		}
		if _, ok := need[parsed.String()]; ok {
			return true
		}
	}
	return false
}
