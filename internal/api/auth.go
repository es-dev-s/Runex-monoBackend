package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/mail"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"deploy_platform/internal/ident"
	"deploy_platform/internal/store"

	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookie = "platform_session"
	sessionTTL    = 30 * 24 * time.Hour
	bcryptCost    = 12
)

type ctxKey int

const userCtxKey ctxKey = 1

var (
	usernameRE    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{2,31}$`)
	dummyHash     []byte
	authLimit     = newLimiter(8, 10*time.Minute)
	registerLimit = newLimiter(3, 10*time.Minute)
)

func init() {
	dummyHash, _ = bcrypt.GenerateFromPassword([]byte("not-a-real-password"), bcryptCost)
}

func userFrom(r *http.Request) store.User {
	u, _ := r.Context().Value(userCtxKey).(store.User)
	return u
}

func withUser(r *http.Request, u store.User) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), userCtxKey, u))
}

func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authPublic(r) {
			next.ServeHTTP(w, r)
			return
		}
		user, err := s.sessionUser(r)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "Sign in to continue")
			return
		}
		next.ServeHTTP(w, withUser(r, user))
	})
}

func authPublic(r *http.Request) bool {
	if r.Method == http.MethodOptions {
		return true
	}
	path := r.URL.Path
	switch {
	case (r.Method == http.MethodGet || r.Method == http.MethodHead) && (path == "/healthz" || path == "/readyz"):
		return true
	case r.Method == http.MethodPost && path == "/v1/github/webhook":
		return true
	case r.Method == http.MethodPost && (path == "/v1/auth/register" || path == "/v1/auth/login"):
		return true
	case r.Method == http.MethodGet && (path == "/v1/auth/google" || path == "/v1/auth/google/callback"):
		return true
	case r.Method == http.MethodGet && path == "/v1/github/setup":
		return true
	default:
		return false
	}
}

type authBody struct {
	Username string `json:"username"`
	Email    string `json:"email"`
	Login    string `json:"login"`
	Password string `json:"password"`
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.AllowPublicRegister {
		writeErr(w, http.StatusForbidden, "registration_disabled", "Public registration is disabled. Ask an admin for access.")
		return
	}
	ip := clientIP(r)
	if !registerLimit.allow(ip) || !authLimit.allow(ip) {
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "Too many attempts. Try again in a few minutes.")
		return
	}
	var body authBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "JSON body is required")
		return
	}
	username := strings.TrimSpace(body.Username)
	email := strings.ToLower(strings.TrimSpace(body.Email))
	if err := validateUsername(username); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_username", err.Error())
		return
	}
	if err := validateEmail(email); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_email", err.Error())
		return
	}
	if err := validatePassword(body.Password); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_password", err.Error())
		return
	}
	taken, err := s.store.UsernameTaken(r.Context(), username)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not create account")
		return
	}
	if taken {
		writeErr(w, http.StatusConflict, "username_taken", "That username is already in use")
		return
	}
	taken, err = s.store.EmailTaken(r.Context(), email)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not create account")
		return
	}
	if taken {
		writeErr(w, http.StatusConflict, "email_taken", "That email is already in use")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcryptCost)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "hash", "Could not create account")
		return
	}
	user := store.User{
		ID:           ident.New("usr_"),
		Username:     username,
		Email:        email,
		PasswordHash: string(hash),
		Name:         username,
	}
	if err := s.store.CreateUser(r.Context(), user); err != nil {
		writeErr(w, http.StatusConflict, "account_exists", "Could not create that account")
		return
	}
	if created, err := s.store.UserByID(r.Context(), user.ID); err == nil {
		user = created
	}
	if err := s.issueSession(w, r, user.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "session", "Account created, but sign-in failed. Sign in manually.")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"user": publicUser(user)})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !authLimit.allow(clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "Too many attempts. Try again in a few minutes.")
		return
	}
	var body authBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "JSON body is required")
		return
	}
	login := strings.TrimSpace(body.Login)
	if login == "" {
		login = strings.TrimSpace(body.Email)
	}
	if login == "" {
		login = strings.TrimSpace(body.Username)
	}
	user, err := s.store.UserByLogin(r.Context(), login)
	hash := dummyHash
	if err == nil && user.PasswordHash != "" {
		hash = []byte(user.PasswordHash)
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(body.Password)) != nil || err != nil || user.PasswordHash == "" {
		writeErr(w, http.StatusUnauthorized, "invalid_credentials", "Incorrect username or password")
		return
	}
	if user.RevokedAt != nil {
		writeErr(w, http.StatusForbidden, "access_revoked", "This account no longer has access to Runex.")
		return
	}
	s.store.PruneExpiredSessions(r.Context())
	if err := s.issueSession(w, r, user.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "session", "Could not start a session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": publicUser(user)})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		s.forgetSession(cookie.Value)
		_ = s.store.DeleteSession(r.Context(), cookie.Value)
	}
	s.clearSessionCookie(w, r)
	s.wakeAdmins()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"user": publicUser(userFrom(r))})
}

// sessionCacheTTL bounds how long a resolved session is reused. Every API call
// has to identify the caller, and a managed database is often a cross-region
// hop, so looking the session up each time added that latency to every request.
// Sign-out deletes the entry, so a revoked session never survives this window.
const sessionCacheTTL = 20 * time.Second

type sessionEntry struct {
	user    store.User
	expires time.Time
}

func (s *Server) sessionUser(r *http.Request) (store.User, error) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || len(cookie.Value) < 32 {
		return store.User{}, errors.New("no session")
	}
	if user, ok := s.cachedSession(cookie.Value); ok {
		return user, nil
	}
	user, err := s.store.UserBySession(r.Context(), cookie.Value)
	if err != nil {
		return store.User{}, err
	}
	s.rememberSession(cookie.Value, user)
	return user, nil
}

func (s *Server) cachedSession(id string) (store.User, bool) {
	s.sessionMu.RLock()
	entry, ok := s.sessions[id]
	s.sessionMu.RUnlock()
	if !ok || time.Now().After(entry.expires) {
		return store.User{}, false
	}
	return entry.user, true
}

func (s *Server) rememberSession(id string, user store.User) {
	s.sessionMu.Lock()
	if s.sessions == nil {
		s.sessions = map[string]sessionEntry{}
	}
	// Bound the map so a long-lived process cannot grow it without limit.
	if len(s.sessions) > 4096 {
		now := time.Now()
		for key, entry := range s.sessions {
			if now.After(entry.expires) {
				delete(s.sessions, key)
			}
		}
		if len(s.sessions) > 4096 {
			s.sessions = map[string]sessionEntry{}
		}
	}
	s.sessions[id] = sessionEntry{user: user, expires: time.Now().Add(sessionCacheTTL)}
	s.sessionMu.Unlock()
}

func (s *Server) forgetSession(id string) {
	s.sessionMu.Lock()
	delete(s.sessions, id)
	s.sessionMu.Unlock()
}

func (s *Server) forgetUserSessions(userID string) {
	s.sessionMu.Lock()
	for id, entry := range s.sessions {
		if entry.user.ID == userID {
			delete(s.sessions, id)
		}
	}
	s.sessionMu.Unlock()
}

func (s *Server) issueSession(w http.ResponseWriter, r *http.Request, userID string) error {
	if cookie, err := r.Cookie(sessionCookie); err == nil && cookie.Value != "" {
		s.forgetSession(cookie.Value)
		_ = s.store.DeleteSession(r.Context(), cookie.Value)
	}
	id := store.NewSessionID()
	if err := s.store.CreateSession(r.Context(), id, userID, sessionTTL); err != nil {
		return err
	}
	http.SetCookie(w, s.sessionCookieFor(r, id, sessionTTL))
	s.wakeAdmins()
	return nil
}

func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	c := s.sessionCookieFor(r, "", -1)
	c.MaxAge = -1
	c.Expires = time.Unix(0, 0)
	http.SetCookie(w, c)
}

func publicUser(u store.User) map[string]any {
	name := strings.TrimSpace(u.Name)
	if name == "" {
		name = u.Username
	}
	role := u.Role
	if role == "" {
		role = store.RoleUser
	}
	return map[string]any{
		"id":          u.ID,
		"username":    u.Username,
		"email":       u.Email,
		"name":        name,
		"role":        role,
		"avatarUrl":   u.AvatarURL,
		"theme":       u.Theme,
		"hasPassword": u.PasswordHash != "",
		"createdAt":   u.CreatedAt,
		"plan":        planFor(u, time.Now()),
		"onboarded":   u.OnboardedAt != nil,
	}
}

// completeOnboarding records that the user dismissed the first-run welcome, on
// the account so other browsers never show it again.
func (s *Server) completeOnboarding(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	at, err := s.store.MarkOnboarded(r.Context(), user.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not save your progress")
		return
	}
	user.OnboardedAt = &at
	s.forgetUserSessions(user.ID)
	writeJSON(w, http.StatusOK, map[string]any{"user": publicUser(user)})
}

// updatePreferences saves per-account dashboard settings. Only the theme is
// stored today; an empty value means the user has not chosen yet.
func (s *Server) updatePreferences(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	var body struct {
		Theme *string `json:"theme"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&body); err != nil || body.Theme == nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "A theme is required")
		return
	}
	theme := strings.TrimSpace(*body.Theme)
	if theme != "system" && theme != "light" && theme != "dark" {
		writeErr(w, http.StatusBadRequest, "invalid_theme", "Theme must be system, light, or dark")
		return
	}
	if err := s.store.SetUserTheme(r.Context(), user.ID, theme); err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not save your appearance")
		return
	}
	s.forgetUserSessions(user.ID)
	user.Theme = theme
	writeJSON(w, http.StatusOK, map[string]any{"user": publicUser(user)})
}

func validateUsername(raw string) error {
	if !usernameRE.MatchString(raw) {
		return errors.New("Username must be 3–32 characters: letters, numbers, dot, underscore, or hyphen")
	}
	return nil
}

func validateEmail(raw string) error {
	addr, err := mail.ParseAddress(raw)
	if err != nil || addr.Address != raw || !strings.Contains(raw, ".") {
		return errors.New("Enter a valid email address")
	}
	if len(raw) > 254 {
		return errors.New("Email is too long")
	}
	return nil
}

func validatePassword(raw string) error {
	if len(raw) < 8 || len(raw) > 72 {
		return errors.New("Password must be 8–72 characters")
	}
	var letter, number bool
	for _, r := range raw {
		switch {
		case unicode.IsLetter(r):
			letter = true
		case unicode.IsNumber(r):
			number = true
		}
	}
	if !letter || !number {
		return errors.New("Password must include a letter and a number")
	}
	return nil
}

func clientIP(r *http.Request) string {
	// Cloudflare sets CF-Connecting-IP from the real client socket and
	// overwrites any spoofed value. Trust that first.
	if ip := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); ip != "" {
		if parsed := net.ParseIP(ip); parsed != nil {
			return parsed.String()
		}
	}
	// Do NOT trust X-Forwarded-For or X-Real-IP here. Attackers can set
	// those on requests that reach the origin, which previously bypassed
	// login/register rate limits after a 429.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type limiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	max    int
	window time.Duration
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{hits: map[string][]time.Time{}, max: max, window: window}
}

func (l *limiter) allow(key string) bool {
	if key == "" {
		key = "unknown"
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := now.Add(-l.window)
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.max {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	if n := len(l.hits); n > 10_000 {
		for k, ts := range l.hits {
			if len(ts) == 0 || ts[len(ts)-1].Before(cut) {
				delete(l.hits, k)
			}
		}
	}
	return true
}
