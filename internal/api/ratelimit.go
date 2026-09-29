package api

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

// Global abuse controls layered on top of withCommon. Auth already has a
// dedicated limiter in auth.go; these cover other expensive/public surfaces.
var (
	githubLimit    = newLimiter(40, time.Minute)
	deployLimit    = newLimiter(20, time.Minute)
	domainAPILimit = newLimiter(30, time.Minute)
	// Browsing a database is read-only and served from a warm pool in about
	// two milliseconds, so the ceiling only needs to stop abuse. It used to be
	// 90 a minute keyed by address, which a single person clicking through a
	// few hundred tables could exhaust — and everyone behind one office
	// address shared that budget, so one user's browsing returned 429 to
	// everybody else and the studio rendered empty.
	databaseLimit = newLimiter(600, time.Minute)
	writeLimit    = newLimiter(240, time.Minute)
)

// rateKey buckets a caller. Rate limiting runs before authentication, so the
// user is not resolved yet; the session cookie identifies them just as well and
// costs nothing to read. It is hashed so the limiter never holds raw session
// tokens. Callers without a session fall back to their address.
func rateKey(r *http.Request) string {
	if cookie, err := r.Cookie(sessionCookie); err == nil && cookie.Value != "" {
		sum := sha256.Sum256([]byte(cookie.Value))
		return "s:" + hex.EncodeToString(sum[:16])
	}
	return "ip:" + clientIP(r)
}

func (s *Server) withRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		ip := clientIP(r)
		// Everything below /v1/projects requires a session, so those buckets
		// are per caller. One tenant must never be able to rate limit another.
		caller := rateKey(r)
		path := r.URL.Path
		var ok bool
		switch {
		case strings.HasPrefix(path, "/v1/auth/"):
			// handled inside login/register handlers
			ok = true
		case strings.HasPrefix(path, "/v1/github"):
			ok = githubLimit.allow(ip)
		case strings.Contains(path, "/deploy") || (r.Method == http.MethodPost && strings.Contains(path, "/nodes/") && strings.HasSuffix(path, "/cancel")):
			ok = deployLimit.allow(caller)
		case strings.Contains(path, "/domains") || strings.Contains(path, "/platform-url"):
			ok = domainAPILimit.allow(caller)
		case strings.Contains(path, "/database"):
			ok = databaseLimit.allow(caller)
		case r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch || r.Method == http.MethodDelete:
			ok = writeLimit.allow(caller)
		default:
			ok = true
		}
		if !ok {
			w.Header().Set("Retry-After", "30")
			writeErr(w, http.StatusTooManyRequests, "rate_limited", "Too many requests. Slow down and try again.")
			return
		}
		next.ServeHTTP(w, r)
	})
}
