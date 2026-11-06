package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"deploy_platform/internal/config"
)

func TestGoogleWebOrigin(t *testing.T) {
	for _, tc := range []struct{ host, forwarded, want string }{
		{"app.runex.cloud", "", "https://app.runex.cloud"},
		{"127.0.0.1:8080", "app.runex.cloud", "https://app.runex.cloud"},
		{"app.runex.cloud.evil.example", "", ""},
		{"aivex.art", "", "https://aivex.art"},
		{"www.aivex.art", "", "https://www.aivex.art"},
		{"127.0.0.1:8080", "aivex.art", "https://aivex.art"},
		{"127.0.0.1:8080", "www.aivex.art, proxy.local", "https://www.aivex.art"},
		{"localhost:3000", "", "http://localhost:3000"},
		{"127.0.0.1:8080", "", "http://localhost:3000"},
		{"evil.example", "", ""},
		{"aivex.art.evil.example", "", ""},
		{"aivex.art:8080", "", ""},
		{"aivex.art", "evil.example", ""},
	} {
		t.Run(tc.host+tc.forwarded, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://"+tc.host+"/v1/auth/google", nil)
			r.Header.Set("X-Forwarded-Host", tc.forwarded)
			got, err := googleWebOrigin(r)
			if got != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("origin=%q err=%v want=%q", got, err, tc.want)
			}
		})
	}
}

func TestGoogleAuthPublic(t *testing.T) {
	for _, path := range []string{"/v1/auth/google", "/v1/auth/google/callback"} {
		if !authPublic(httptest.NewRequest("GET", path, nil)) {
			t.Fatal("OAuth GET must be public")
		}
		if authPublic(httptest.NewRequest("POST", path, nil)) {
			t.Fatal("OAuth POST must not be public")
		}
	}
}

func TestGoogleLoginUnconfigured(t *testing.T) {
	for _, cfg := range []config.Config{{}, {GoogleClientID: "test"}, {GoogleClientSecret: "test"}} {
		s := &Server{cfg: cfg}
		for _, handler := range []http.HandlerFunc{s.googleLogin, s.googleCallback} {
			w := httptest.NewRecorder()
			handler(w, httptest.NewRequest("GET", "http://localhost:3000/v1/auth/google", nil))
			if w.Code != 503 {
				t.Fatalf("got %d want 503", w.Code)
			}
		}
	}
}

func TestGoogleLoginRedirect(t *testing.T) {
	s := &Server{cfg: config.Config{GoogleClientID: "test-id", GoogleClientSecret: "test-secret", WebURL: "https://wrong.example"}}
	for _, host := range []string{"localhost:3000", "aivex.art", "www.aivex.art"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "http://"+host+"/v1/auth/google", nil)
		s.googleLogin(w, r)
		if w.Code != 302 {
			t.Fatalf("status %d", w.Code)
		}
		dest, _ := url.Parse(w.Header().Get("Location"))
		q := dest.Query()
		origin, _ := googleWebOrigin(r)
		if dest.Host != "accounts.google.com" || q.Get("redirect_uri") != origin+googleCallbackPath || q.Get("scope") != "openid email profile" || q.Get("client_id") != "test-id" || q.Get("response_type") != "code" {
			t.Fatal("incorrect authorization URL")
		}
		cookies := w.Result().Cookies()
		if len(cookies) != 1 {
			t.Fatal("missing state cookie")
		}
		c := cookies[0]
		if c.Name != googleStateCookie || c.Value != q.Get("state") || len(c.Value) != 43 || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Domain != "" || c.MaxAge != 600 || c.Secure != (host != "localhost:3000") {
			t.Fatal("incorrect state cookie")
		}
		if strings.Contains(w.Header().Get("Location"), "test-secret") {
			t.Fatal("client secret leaked")
		}
	}
	w := httptest.NewRecorder()
	s.googleLogin(w, httptest.NewRequest("GET", "http://127.0.0.1:3000/v1/auth/google", nil))
	if w.Header().Get("Location") != "http://localhost:3000/v1/auth/google" || len(w.Result().Cookies()) != 0 {
		t.Fatal("must canonicalize before setting state cookie")
	}
}

func TestGoogleCallbackInvalidState(t *testing.T) {
	s := &Server{cfg: config.Config{GoogleClientID: "test", GoogleClientSecret: "test"}}
	for _, state := range []string{"", "short", strings.Repeat("b", 43)} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "http://localhost:3000/v1/auth/google/callback?code=example&state="+state, nil)
		r.AddCookie(&http.Cookie{Name: googleStateCookie, Value: strings.Repeat("a", 43)})
		s.googleCallback(w, r)
		if w.Code != 302 || w.Header().Get("Location") != "http://localhost:3000/sign-in?oauth_error=invalid_state" {
			t.Fatal("invalid state accepted")
		}
		if w.Result().Cookies()[0].MaxAge != -1 {
			t.Fatal("state cookie not consumed")
		}
	}
}

func TestGoogleCallbackDenied(t *testing.T) {
	s := &Server{cfg: config.Config{GoogleClientID: "test", GoogleClientSecret: "test"}}
	state := strings.Repeat("a", 43)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "http://localhost:3000/v1/auth/google/callback?error=access_denied&state="+state, nil)
	r.AddCookie(&http.Cookie{Name: googleStateCookie, Value: state})
	s.googleCallback(w, r)
	if w.Code != 302 || w.Header().Get("Location") != "http://localhost:3000/sign-in?oauth_error=google_denied" {
		t.Fatal("denial mishandled")
	}
}

type googleTestTransport func(*http.Request) (*http.Response, error)

func (f googleTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGoogleProfileExchange(t *testing.T) {
	original := http.DefaultTransport
	defer func() { http.DefaultTransport = original }()
	calls := 0
	http.DefaultTransport = googleTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		body := `{"access_token":"test-access","token_type":"Bearer"}`
		if calls == 1 {
			if r.URL.String() != "https://oauth2.googleapis.com/token" || r.Method != "POST" {
				t.Fatal("incorrect token endpoint")
			}
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.Form.Get("redirect_uri") != "https://aivex.art/v1/auth/google/callback" || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "test-code" {
				t.Fatal("incorrect exchange")
			}
		} else {
			if r.URL.String() != "https://openidconnect.googleapis.com/v1/userinfo" || r.Header.Get("Authorization") != "Bearer test-access" {
				t.Fatal("incorrect userinfo request")
			}
			body = `{"sub":"test-sub","email":"person@example.com","email_verified":true,"name":"Person","picture":"https://example.com/avatar"}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	s := &Server{cfg: config.Config{GoogleClientID: "test", GoogleClientSecret: "test"}}
	profile, err := s.googleProfile(context.Background(), "test-code", "https://aivex.art/v1/auth/google/callback")
	if err != nil || calls != 2 || !profile.EmailVerified || profile.Sub != "test-sub" {
		t.Fatalf("profile failed: %v", err)
	}
}

func TestGoogleCallbackUnverified(t *testing.T) {
	original := http.DefaultTransport
	defer func() { http.DefaultTransport = original }()
	http.DefaultTransport = googleTestTransport(func(r *http.Request) (*http.Response, error) {
		body := `{"access_token":"test-access","token_type":"Bearer"}`
		if r.Method == "GET" {
			body = `{"sub":"test-sub","email":"person@example.com","email_verified":false}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	s := &Server{cfg: config.Config{GoogleClientID: "test", GoogleClientSecret: "test"}}
	state := strings.Repeat("a", 43)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "http://localhost:3000/v1/auth/google/callback?code=test&state="+state, nil)
	r.AddCookie(&http.Cookie{Name: googleStateCookie, Value: state})
	s.googleCallback(w, r)
	if w.Code != 302 || w.Header().Get("Location") != "http://localhost:3000/sign-in?oauth_error=unverified_email" {
		t.Fatal("unverified email accepted")
	}
}

func TestGoogleSessionCookieUsesBrowserOrigin(t *testing.T) {
	s := &Server{cfg: config.Config{WebURL: "https://aivex.art", PublicURL: "https://api.aivex.art"}}
	for _, host := range []string{"localhost:3000", "aivex.art"} {
		r := httptest.NewRequest("GET", "http://"+host+googleCallbackPath, nil)
		c := s.sessionCookieFor(r, "test", sessionTTL)
		if c.Domain != "" || c.Secure != (host == "aivex.art") || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
			t.Fatal("session cookie would be dropped by browser")
		}
	}
}

func TestSlugUsername(t *testing.T) {
	for in, want := range map[string]string{
		"Pawan.Ojha":     "pawan.ojha",
		"pawanojha414":   "pawanojha414",
		"  José  María ": "jos.mar.a",
		"a":              "",
		"__x__":          "",
		"first+tag":      "first.tag",
		"日本":             "",
		"very.long.name.that.goes.on.and.on.forever": "very.long.name.that.goes.on.and",
	} {
		if got := slugUsername(in); got != want {
			t.Fatalf("slugUsername(%q)=%q want %q", in, got, want)
		}
	}
}

func TestGeneratedUsername(t *testing.T) {
	if !generatedUsername("google_d001f2ada38c48d68275") {
		t.Fatal("expected generated")
	}
	for _, name := range []string{"google_fan", "pawan.ojha", "google_d001f2ada38c48d6827Z"} {
		if generatedUsername(name) {
			t.Fatalf("%q is not generated", name)
		}
	}
}

func TestGooglePicture(t *testing.T) {
	if googlePicture("https://lh3.googleusercontent.com/a/abc=s96-c") == "" {
		t.Fatal("google photo rejected")
	}
	for _, bad := range []string{"http://lh3.googleusercontent.com/a", "https://evil.example/a.png", "https://googleusercontent.com.evil.example/x", "javascript:alert(1)"} {
		if googlePicture(bad) != "" {
			t.Fatalf("%q accepted", bad)
		}
	}
}
