// Package customdomain validates customer hostnames, builds the DNS records
// they must create, and checks those records before Runex starts routing.
//
// A hostname is never routed until the TXT ownership token is present. The
// CNAME (or a flattened A/AAAA match) is required before status=active so
// a verified-but-unpointed domain cannot steal traffic from another site.
package customdomain

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"sync"
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

// Records are the DNS rows a provider form actually has. TXT proves
// ownership. A subdomain routes with CNAME. A root domain uses that same
// CNAME when the provider allows it (Cloudflare does), or A and AAAA from
// AddressRecords when it does not.
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
		Hint:     "Type TXT. Name is the host above. Paste the value exactly.",
	}}
	if cnameTarget == "" {
		return out
	}
	host := label
	hint := "Type CNAME. Name is the host above, target is this value. On Cloudflare, leave the proxy on (orange cloud)."
	if apex {
		host = "@"
		hint = "Type CNAME, name @. Cloudflare allows this on the root and should stay proxied. If your provider rejects a CNAME on @, add the A and AAAA records instead. ALIAS or ANAME, where you see them, use this same target."
	}
	out = append(out, Record{
		Type:     "CNAME",
		Host:     host,
		Name:     hostname,
		Value:    cnameTarget,
		Purpose:  "route",
		Required: true,
		Hint:     hint,
	})
	return out
}

// AddressRecords are the A and AAAA rows for a root domain whose DNS
// provider has no CNAME on @. Subdomains do not need them.
func AddressRecords(hostname string, v4, v6 []string) []Record {
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	_, _, apex := SplitZone(hostname)
	if !apex {
		return nil
	}
	var out []Record
	for _, ip := range uniqueIPs(v4) {
		out = append(out, Record{
			Type:     "A",
			Host:     "@",
			Name:     hostname,
			Value:    ip,
			Purpose:  "route",
			Required: false,
			Hint:     "Add this A record only when a CNAME on @ is not allowed.",
		})
	}
	for _, ip := range uniqueIPs(v6) {
		out = append(out, Record{
			Type:     "AAAA",
			Host:     "@",
			Name:     hostname,
			Value:    ip,
			Purpose:  "route",
			Required: false,
			Hint:     "Add this AAAA record only when a CNAME on @ is not allowed.",
		})
	}
	return out
}

func uniqueIPs(raw []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, item := range raw {
		parsed := net.ParseIP(strings.TrimSpace(item))
		if parsed == nil {
			continue
		}
		key := parsed.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, key)
	}
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
	txt, err := lookupTXT(ctx, resolver, ChallengeName(hostname))
	if err == nil && TXTMatch(txt, token) {
		out.TXT = true
	}
	if !out.TXT {
		if extra, extraErr := lookupTXT(ctx, resolver, hostname); extraErr == nil && TXTMatch(extra, token) {
			out.TXT = true
		}
	}
	targets := cleanTargets(cnameTargets)
	if len(targets) == 0 {
		out.Route = out.TXT
	} else {
		out.Route = pointsAt(ctx, resolver, hostname, targets)
	}
	_, _, apex := SplitZone(hostname)
	shown := ""
	if len(targets) > 0 {
		shown = targets[0]
	}
	switch {
	case out.TXT && out.Route:
		out.Active = true
	case !out.TXT && !out.Route:
		if apex {
			out.Error = "Add the TXT record, then a CNAME to " + shown + ", or A and AAAA records if your provider has no CNAME on @."
		} else {
			out.Error = "Add the TXT record and a CNAME to " + shown + ", then verify again."
		}
	case !out.TXT:
		out.Error = "Routing is in place. Add the TXT record so Runex can confirm you own this domain."
	default:
		if apex {
			out.Error = "TXT is in place. Add a CNAME to " + shown + ", or the A and AAAA records, then verify again."
		} else {
			out.Error = "TXT is in place. Add a CNAME pointing at " + shown + "."
		}
	}
	return out
}

func lookupTXT(ctx context.Context, resolver Resolver, name string) ([]string, error) {
	return retryDNS(ctx, func() ([]string, error) { return resolver.LookupTXT(ctx, name) })
}

func lookupCNAME(ctx context.Context, resolver Resolver, name string) (string, error) {
	return retryDNS(ctx, func() (string, error) { return resolver.LookupCNAME(ctx, name) })
}

func lookupHost(ctx context.Context, resolver Resolver, name string) ([]string, error) {
	return retryDNS(ctx, func() ([]string, error) { return resolver.LookupHost(ctx, name) })
}

func retryDNS[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	var zero T
	values, err := fn()
	if err == nil || !temporaryDNS(err) || ctx.Err() != nil {
		return values, err
	}
	timer := time.NewTimer(300 * time.Millisecond)
	select {
	case <-ctx.Done():
		timer.Stop()
		return zero, ctx.Err()
	case <-timer.C:
	}
	return fn()
}

var edgeAddrCache struct {
	sync.Mutex
	target string
	at     time.Time
	v4     []string
	v6     []string
}

// LookupEdgeAddresses resolves the platform CNAME target into the A and AAAA
// addresses a root-domain DNS form can store. Results are cached briefly.
func LookupEdgeAddresses(ctx context.Context, target string) (v4, v6 []string) {
	target = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(target), "."))
	if target == "" {
		return nil, nil
	}
	edgeAddrCache.Lock()
	if edgeAddrCache.target == target && time.Since(edgeAddrCache.at) < 10*time.Minute {
		v4 = append([]string(nil), edgeAddrCache.v4...)
		v6 = append([]string(nil), edgeAddrCache.v6...)
		edgeAddrCache.Unlock()
		return v4, v6
	}
	edgeAddrCache.Unlock()

	lookupCtx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(lookupCtx, target)
	if err != nil || len(ips) == 0 {
		return nil, nil
	}
	for _, ip := range ips {
		if ip.IP == nil {
			continue
		}
		if v4addr := ip.IP.To4(); v4addr != nil {
			v4 = append(v4, v4addr.String())
			continue
		}
		v6 = append(v6, ip.IP.String())
	}
	v4, v6 = uniqueIPs(v4), uniqueIPs(v6)
	edgeAddrCache.Lock()
	edgeAddrCache.target = target
	edgeAddrCache.at = time.Now()
	edgeAddrCache.v4 = append([]string(nil), v4...)
	edgeAddrCache.v6 = append([]string(nil), v6...)
	edgeAddrCache.Unlock()
	return v4, v6
}

func temporaryDNS(err error) bool {
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) {
		return false
	}
	return dnsErr.Temporary() || dnsErr.Timeout()
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
	if cname, err := lookupCNAME(ctx, resolver, hostname); err == nil {
		cname = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(cname), "."))
		if cname != "" && !sameHost(cname, hostname) {
			for _, target := range targets {
				if sameHost(cname, target) {
					return true
				}
			}
		}
	}
	got, err := lookupHost(ctx, resolver, hostname)
	if err != nil || len(got) == 0 {
		return false
	}
	if CloudflareProxied(got) {
		return true
	}
	need := map[string]struct{}{}
	for _, target := range targets {
		want, lookErr := lookupHost(ctx, resolver, target)
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
