package api

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"deploy_platform/internal/config"
)

const dashCookie = "github_dash"

func (s *Server) sessionCookie(value string, ttl time.Duration) *http.Cookie {
	return s.newCookie(sessionCookie, value, ttl)
}

func (s *Server) sessionCookieFor(r *http.Request, value string, ttl time.Duration) *http.Cookie {
	c := s.newCookieFor(r, sessionCookie, value, ttl)
	s.relaxCookieForOrigin(r, c)
	// Google returns through the same-origin Next rewrite. Do not inherit a
	// production cookie domain or Secure setting when signing in on localhost.
	if r != nil && r.URL.Path == "/v1/auth/google/callback" {
		if origin, err := googleWebOrigin(r); err == nil {
			c.Domain = ""
			c.Secure = strings.HasPrefix(origin, "https://")
			c.SameSite = http.SameSiteLaxMode
		}
	}
	return c
}

func (s *Server) connectCookie(value string, ttl time.Duration) *http.Cookie {
	return s.newCookie(connectCookie, value, ttl)
}

func (s *Server) connectCookieFor(r *http.Request, value string, ttl time.Duration) *http.Cookie {
	c := s.newCookieFor(r, connectCookie, value, ttl)
	s.relaxCookieForOrigin(r, c)
	return c
}

func (s *Server) dashboardCookie(origin string, ttl time.Duration) *http.Cookie {
	c := s.newCookie(dashCookie, url.QueryEscape(strings.TrimRight(origin, "/")), ttl)
	if strings.HasPrefix(origin, "https://") {
		c.SameSite = http.SameSiteNoneMode
		c.Secure = true
	}
	return c
}

func (s *Server) newCookie(name, value string, ttl time.Duration) *http.Cookie {
	return s.newCookieFor(nil, name, value, ttl)
}

func (s *Server) newCookieFor(r *http.Request, name, value string, ttl time.Duration) *http.Cookie {
	secure := s.cookieSecure(r)
	sameSite := http.SameSiteLaxMode
	// SameSite=None requires Secure. Do not force Secure just because
	// PLATFORM_PUBLIC_URL is the GitHub proxy on Railway — the LAN dashboard
	// talks same-origin over HTTP and the browser would drop that cookie.
	if config.CrossSiteCookies(s.cfg.WebURL, s.cfg.PublicURL) && secure {
		sameSite = http.SameSiteNoneMode
	}
	cookie := &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   int(ttl.Seconds()),
		Expires:  time.Now().Add(ttl),
		HttpOnly: true,
		Secure:   secure,
		SameSite: sameSite,
	}
	if domain := config.SharedCookieDomain(s.cfg.WebURL, s.cfg.PublicURL); domain != "" {
		cookie.Domain = domain
	}
	return cookie
}

func (s *Server) relaxCookieForOrigin(r *http.Request, c *http.Cookie) {
	if r == nil || c == nil {
		return
	}
	origin := strings.TrimRight(strings.TrimSpace(r.Header.Get("Origin")), "/")
	if origin == "" || !s.originAllowed(origin) || !s.cookieCrossSiteTo(origin) {
		return
	}
	if !s.cookieSecure(r) {
		return
	}
	c.SameSite = http.SameSiteNoneMode
	c.Secure = true
}

func (s *Server) cookieSecure(r *http.Request) bool {
	if r != nil {
		if r.TLS != nil {
			return true
		}
		switch forwardedProto(r) {
		case "https":
			return true
		case "http":
			return false
		}
		// Next's dev rewrite sets X-Forwarded-Host and not X-Forwarded-Proto.
		// A phone on http://192.168.x.x:3000 would otherwise inherit Secure from
		// PLATFORM_WEB_URL and the browser would drop the session cookie.
		if browserFacingHTTP(r) {
			return false
		}
	}
	return strings.HasPrefix(s.cfg.WebURL, "https://") || strings.HasPrefix(s.cfg.PublicURL, "https://")
}

func forwardedProto(r *http.Request) string {
	proto := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))
	if i := strings.IndexByte(proto, ','); i >= 0 {
		proto = proto[:i]
	}
	return strings.ToLower(strings.TrimSpace(proto))
}

func browserFacingHTTP(r *http.Request) bool {
	parsed, err := url.Parse(strings.TrimSpace(r.Header.Get("Origin")))
	if err != nil || !strings.EqualFold(parsed.Scheme, "http") || parsed.Hostname() == "" {
		return false
	}
	fwd := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if i := strings.IndexByte(fwd, ','); i >= 0 {
		fwd = fwd[:i]
	}
	fwd = strings.TrimSpace(fwd)
	if fwd == "" || (!sameHost(parsed.Host, fwd) && !sameHost(parsed.Hostname(), stripPort(fwd))) {
		return false
	}
	return localDevHost(parsed.Hostname())
}

func sameHost(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func stripPort(hostport string) string {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport
	}
	return host
}

func localDevHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

func (s *Server) cookieCrossSiteTo(origin string) bool {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Hostname() == "" {
		return false
	}
	origReg := config.RegistrableDomain(parsed.Hostname())
	apiReg := config.RegistrableDomain(hostnameFromURL(s.cfg.PublicURL))
	webReg := config.RegistrableDomain(hostnameFromURL(s.cfg.WebURL))
	if origReg != "" && (origReg == apiReg || origReg == webReg) {
		return false
	}
	return true
}

func hostnameFromURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

func (s *Server) dashboardFromRequest(r *http.Request) string {
	origin := strings.TrimRight(strings.TrimSpace(r.Header.Get("Origin")), "/")
	if s.originAllowed(origin) {
		return origin
	}
	return ""
}

func (s *Server) dashboardFromCookie(r *http.Request) string {
	cookie, err := r.Cookie(dashCookie)
	if err != nil || cookie.Value == "" {
		return ""
	}
	raw, err := url.QueryUnescape(cookie.Value)
	if err != nil {
		return ""
	}
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if s.originAllowed(raw) {
		return raw
	}
	return ""
}
