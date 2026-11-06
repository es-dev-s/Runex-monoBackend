package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"deploy_platform/internal/ident"
	"deploy_platform/internal/store"
	"github.com/jackc/pgx/v5"
)

const googleStateCookie = "google_oauth_state"
const googleCallbackPath = "/v1/auth/google/callback"

// Only these browser origins may receive an OAuth code. Never use WebURL here:
// a local Next rewrite may run with production environment configuration.
func googleWebOrigin(r *http.Request) (string, error) {
	host := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Host"), ",")[0])
	if host == "" {
		host = r.Host
	}
	parsed, err := url.Parse("//" + host)
	if err != nil || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("invalid OAuth host")
	}
	switch strings.ToLower(parsed.Hostname()) {
	case "app.runex.cloud", "aivex.art", "www.aivex.art":
		if parsed.Port() != "" && parsed.Port() != "443" {
			return "", errors.New("invalid OAuth port")
		}
		return "https://" + strings.ToLower(parsed.Hostname()), nil
	case "localhost", "127.0.0.1":
		return "http://localhost:3000", nil
	default:
		return "", errors.New("unsupported OAuth host")
	}
}

func googleStateCookieFor(origin, value string, maxAge int) *http.Cookie {
	c := &http.Cookie{Name: googleStateCookie, Value: value, Path: "/v1/auth/google", HttpOnly: true,
		Secure: strings.HasPrefix(origin, "https://"), SameSite: http.SameSiteLaxMode, MaxAge: maxAge,
		Expires: time.Now().Add(time.Duration(maxAge) * time.Second)}
	if maxAge < 0 {
		c.Expires = time.Unix(0, 0)
	}
	return c
}

func (s *Server) googleConfigured() bool {
	return strings.TrimSpace(s.cfg.GoogleClientID) != "" && strings.TrimSpace(s.cfg.GoogleClientSecret) != ""
}

func (s *Server) googleLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.googleConfigured() {
		writeErr(w, http.StatusServiceUnavailable, "google_unconfigured", "Google sign-in is not configured")
		return
	}
	origin, err := googleWebOrigin(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_origin", "Google sign-in is not available on this host")
		return
	}
	// Canonicalize 127.0.0.1 before setting the host-only state cookie; Google
	// must return to localhost:3000 and cookies are not shared across those hosts.
	host := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Host"), ",")[0])
	if host == "" {
		host = r.Host
	}
	if origin == "http://localhost:3000" && !strings.EqualFold(host, "localhost:3000") {
		http.Redirect(w, r, origin+"/v1/auth/google", http.StatusFound)
		return
	}
	stateBytes := make([]byte, 32)
	if _, err := rand.Read(stateBytes); err != nil {
		writeErr(w, 500, "oauth_state", "Could not start Google sign-in")
		return
	}
	state := base64.RawURLEncoding.EncodeToString(stateBytes)
	http.SetCookie(w, googleStateCookieFor(origin, state, 600))
	q := url.Values{"client_id": {s.cfg.GoogleClientID}, "redirect_uri": {origin + googleCallbackPath},
		"response_type": {"code"}, "scope": {"openid email profile"}, "state": {state}}
	http.Redirect(w, r, "https://accounts.google.com/o/oauth2/v2/auth?"+q.Encode(), http.StatusFound)
}

func (s *Server) googleCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.googleConfigured() {
		writeErr(w, 503, "google_unconfigured", "Google sign-in is not configured")
		return
	}
	origin, err := googleWebOrigin(r)
	if err != nil {
		writeErr(w, 400, "invalid_origin", "Invalid Google sign-in origin")
		return
	}
	cookie, cookieErr := r.Cookie(googleStateCookie)
	http.SetCookie(w, googleStateCookieFor(origin, "", -1))
	state := r.URL.Query().Get("state")
	if cookieErr != nil || len(state) != 43 || len(cookie.Value) != 43 || subtle.ConstantTimeCompare([]byte(state), []byte(cookie.Value)) != 1 {
		googleFail(w, r, origin, "invalid_state")
		return
	}
	if r.URL.Query().Get("error") != "" || r.URL.Query().Get("code") == "" {
		googleFail(w, r, origin, "google_denied")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	profile, err := s.googleProfile(ctx, r.URL.Query().Get("code"), origin+googleCallbackPath)
	if err != nil {
		googleFail(w, r, origin, "google_exchange")
		return
	}
	email := strings.ToLower(strings.TrimSpace(profile.Email))
	if !profile.EmailVerified || profile.Sub == "" || validateEmail(email) != nil {
		googleFail(w, r, origin, "unverified_email")
		return
	}
	picture := googlePicture(profile.Picture)
	user, err := s.store.UserByEmail(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		user, err = s.createGoogleUser(ctx, email, profile.Name, picture)
	} else if err == nil {
		s.refreshGoogleUser(ctx, &user, profile.Name, picture)
	}
	if err != nil {
		googleFail(w, r, origin, "db")
		return
	}
	if user.RevokedAt != nil {
		googleFail(w, r, origin, "access_revoked")
		return
	}
	if err := s.issueSession(w, r, user.ID); err != nil {
		googleFail(w, r, origin, "session")
		return
	}
	http.Redirect(w, r, origin+"/", http.StatusFound)
}

// googleFail sends the browser back to the sign-in page with a short reason
// code instead of leaving it on a raw JSON error from the callback.
func googleFail(w http.ResponseWriter, r *http.Request, origin, code string) {
	http.Redirect(w, r, origin+"/sign-in?oauth_error="+url.QueryEscape(code), http.StatusFound)
}

// createGoogleUser registers a first-time Google account with a readable
// handle. A concurrent callback for the same email wins the insert; we then
// resolve by email only. A handle collision simply moves to the next candidate.
func (s *Server) createGoogleUser(ctx context.Context, email, name, picture string) (store.User, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = strings.SplitN(email, "@", 2)[0]
	}
	var lastErr error
	for _, candidate := range s.googleUsernames(ctx, name, email) {
		user := store.User{ID: ident.New("usr_"), Username: candidate, Email: email, Name: name, AvatarURL: picture}
		if err := s.store.CreateUser(ctx, user); err != nil {
			lastErr = err
			if existing, findErr := s.store.UserByEmail(ctx, email); findErr == nil {
				return existing, nil
			}
			continue
		}
		return s.store.UserByEmail(ctx, email)
	}
	if lastErr == nil {
		lastErr = errors.New("no username available")
	}
	return store.User{}, lastErr
}

// refreshGoogleUser keeps the Google photo current and replaces an opaque
// generated handle (google_<hex>) with a readable one. Both are best effort:
// a failure here must never block sign-in.
func (s *Server) refreshGoogleUser(ctx context.Context, user *store.User, name, picture string) {
	if user.RevokedAt != nil {
		return
	}
	changed := false
	if (picture != "" && picture != user.AvatarURL) || (strings.TrimSpace(user.Name) == "" && strings.TrimSpace(name) != "") {
		if s.store.RefreshOAuthProfile(ctx, user.ID, name, picture) == nil {
			changed = true
		}
	}
	if generatedUsername(user.Username) {
		display := strings.TrimSpace(user.Name)
		if display == "" {
			display = name
		}
		for _, candidate := range s.googleUsernames(ctx, display, user.Email) {
			ok, err := s.store.ReplaceGeneratedUsername(ctx, user.ID, user.Username, candidate)
			if err == nil {
				changed = changed || ok
				break
			}
		}
	}
	if !changed {
		return
	}
	s.forgetUserSessions(user.ID)
	if fresh, err := s.store.UserByID(ctx, user.ID); err == nil {
		*user = fresh
	}
}

// generatedUsername reports handles minted by the first Google sign-in build.
func generatedUsername(username string) bool {
	rest, ok := strings.CutPrefix(username, "google_")
	if !ok || len(rest) < 12 {
		return false
	}
	for _, r := range rest {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// googleUsernames returns free, valid handles to try in order: the name
// (pawan.ojha), the email local part (pawanojha414), then suffixed variants.
func (s *Server) googleUsernames(ctx context.Context, name, email string) []string {
	var bases []string
	seen := map[string]bool{}
	for _, raw := range []string{strings.ReplaceAll(strings.TrimSpace(name), " ", "."), strings.SplitN(email, "@", 2)[0]} {
		if base := slugUsername(raw); base != "" && !seen[base] {
			seen[base] = true
			bases = append(bases, base)
		}
	}
	if len(bases) == 0 {
		bases = []string{"user"}
	}
	var out []string
	add := func(candidate string) {
		if len(out) >= 4 || validateUsername(candidate) != nil || generatedUsername(candidate) {
			return
		}
		if taken, err := s.store.UsernameTaken(ctx, candidate); err == nil && !taken {
			out = append(out, candidate)
		}
	}
	for _, base := range bases {
		add(base)
	}
	for i := 0; i < 6 && len(out) < 4; i++ {
		suffix := randomDigits(4)
		base := bases[i%len(bases)]
		if len(base) > 27 {
			base = strings.TrimRight(base[:27], "._-")
		}
		add(base + "-" + suffix)
	}
	return out
}

// slugUsername folds free text into the username alphabet. Characters outside
// [a-z0-9._-] become dots; runs collapse and the edges are trimmed.
func slugUsername(raw string) string {
	var b strings.Builder
	lastSep := true
	for _, r := range strings.ToLower(raw) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			lastSep = false
		case r == '.' || r == '_' || r == '-' || r == ' ' || r == '+':
			if !lastSep {
				if r == ' ' || r == '+' {
					r = '.'
				}
				b.WriteRune(r)
				lastSep = true
			}
		default:
			if !lastSep {
				b.WriteRune('.')
				lastSep = true
			}
		}
	}
	out := strings.Trim(b.String(), "._-")
	if len(out) > 32 {
		out = strings.TrimRight(out[:32], "._-")
	}
	if len(out) < 3 {
		return ""
	}
	return out
}

func randomDigits(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return strings.Repeat("0", n)
	}
	for i := range buf {
		buf[i] = '0' + buf[i]%10
	}
	return string(buf)
}

// googlePicture accepts only an https Google-hosted photo URL so the profile
// image can never point the dashboard at an arbitrary origin.
func googlePicture(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2048 {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "googleusercontent.com" && !strings.HasSuffix(host, ".googleusercontent.com") {
		return ""
	}
	return parsed.String()
}

type googleUserInfo struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
}

// Tokens are used only for this request and never stored or logged. Standard
// OAuth form exchange needs no new dependency or access beyond these scopes.
func (s *Server) googleProfile(ctx context.Context, code, redirectURI string) (googleUserInfo, error) {
	var profile googleUserInfo
	form := url.Values{"code": {code}, "client_id": {s.cfg.GoogleClientID}, "client_secret": {s.cfg.GoogleClientSecret},
		"redirect_uri": {redirectURI}, "grant_type": {"authorization_code"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://oauth2.googleapis.com/token", strings.NewReader(form.Encode()))
	if err != nil {
		return profile, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return profile, errors.New("Google token request failed")
	}
	defer resp.Body.Close()
	var token struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&token) != nil || token.AccessToken == "" || !strings.EqualFold(token.TokenType, "Bearer") {
		return profile, errors.New("Google token exchange failed")
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, "https://openidconnect.googleapis.com/v1/userinfo", nil)
	if err != nil {
		return profile, err
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	info, err := client.Do(req)
	if err != nil {
		return profile, errors.New("Google userinfo request failed")
	}
	defer info.Body.Close()
	if info.StatusCode != http.StatusOK {
		return profile, errors.New("Google userinfo failed")
	}
	err = json.NewDecoder(io.LimitReader(info.Body, 1<<20)).Decode(&profile)
	return profile, err
}
