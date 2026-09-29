package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLimiterWindow(t *testing.T) {
	l := newLimiter(2, time.Minute)
	if !l.allow("x") || !l.allow("x") {
		t.Fatal("first two should pass")
	}
	if l.allow("x") {
		t.Fatal("third should fail")
	}
}

// Two people in one office share a public address. Bucketing authenticated
// reads by address meant one of them browsing a few hundred tables returned 429
// to the other and the studio rendered empty.
func TestDatabaseLimitIsPerSessionNotPerAddress(t *testing.T) {
	s := &Server{}
	h := s.withRateLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	const addr = "203.0.113.42:5555"
	call := func(session string) int {
		req := httptest.NewRequest(http.MethodGet, "/v1/projects/p1/nodes/n1/database/tables", nil)
		req.RemoteAddr = addr
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}

	// Exhaust the first caller's budget.
	for i := 0; i < 600; i++ {
		if code := call("session-one"); code != http.StatusNoContent {
			t.Fatalf("caller one denied early at %d with %d", i, code)
		}
	}
	if code := call("session-one"); code != http.StatusTooManyRequests {
		t.Fatalf("caller one should now be limited, got %d", code)
	}
	// The second caller shares the address but must be unaffected.
	if code := call("session-two"); code != http.StatusNoContent {
		t.Fatalf("caller two was limited by caller one, got %d", code)
	}
}

func TestRateKeyHidesSessionToken(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/projects", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "super-secret-token"})
	key := rateKey(req)
	if strings.Contains(key, "super-secret-token") {
		t.Fatal("the limiter must not hold raw session tokens")
	}
	if !strings.HasPrefix(key, "s:") {
		t.Fatalf("expected a session bucket, got %q", key)
	}

	anon := httptest.NewRequest(http.MethodGet, "/v1/projects", nil)
	anon.RemoteAddr = "198.51.100.9:1111"
	if got := rateKey(anon); got != "ip:198.51.100.9" {
		t.Fatalf("a caller with no session must fall back to address, got %q", got)
	}
}

func TestWithRateLimitGitHub(t *testing.T) {
	s := &Server{}
	// drain github limit
	ip := "198.51.100.7"
	for i := 0; i < 40; i++ {
		if !githubLimit.allow(ip) {
			t.Fatalf("unexpected early deny at %d", i)
		}
	}
	h := s.withRateLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/github/repos", nil)
	req.RemoteAddr = ip + ":1234"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d", rr.Code)
	}
}
