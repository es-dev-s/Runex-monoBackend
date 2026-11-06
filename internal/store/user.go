package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

var errNotFound = errors.New("not found")

const (
	RoleUser  = "user"
	RoleAdmin = "admin"
)

type User struct {
	ID           string     `json:"id"`
	Username     string     `json:"username"`
	Email        string     `json:"email"`
	PasswordHash string     `json:"-"`
	Name         string     `json:"name"`
	AvatarURL    string     `json:"avatarUrl,omitempty"`
	Theme        string     `json:"theme,omitempty"`
	Role         string     `json:"role"`
	RevokedAt    *time.Time `json:"revokedAt,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
	// TrialStartedAt starts the free trial clock. Nil only on a struct built in memory before insert.
	TrialStartedAt *time.Time `json:"-"`
	// OnboardedAt is set once the user dismisses the first-run welcome.
	OnboardedAt *time.Time `json:"-"`
	// BillingStatus is "trial" or "active". Support sets "active" for paying accounts.
	BillingStatus string `json:"-"`
}

// Billing statuses stored in users.billing_status.
const (
	BillingTrial  = "trial"
	BillingActive = "active"
)

// Account is a user as an administrator sees them. It never includes a password.
type Account struct {
	ID        string     `json:"id"`
	Username  string     `json:"username"`
	Email     string     `json:"email"`
	Name      string     `json:"name"`
	Role      string     `json:"role"`
	CreatedAt time.Time  `json:"createdAt"`
	RevokedAt *time.Time `json:"revokedAt,omitempty"`
	SignedIn  bool       `json:"signedIn"`
}

func NewSessionID() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf)
}

const userSelect = `SELECT u.id, u.username, u.email, COALESCE(u.password_hash,''), u.name, u.avatar_url, COALESCE(u.theme,''), COALESCE(u.role,'user'), u.revoked_at, u.created_at, u.trial_started_at, u.onboarded_at, COALESCE(u.billing_status,'trial')`

func (s *Store) CreateUser(ctx context.Context, u User) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO users (id, username, email, password_hash, name, avatar_url)
		VALUES ($1,$2,$3,$4,$5,$6)
	`, u.ID, u.Username, strings.ToLower(strings.TrimSpace(u.Email)), u.PasswordHash, u.Name, u.AvatarURL)
	return err
}

func (s *Store) UserByID(ctx context.Context, id string) (User, error) {
	return scanUser(s.pool.QueryRow(ctx, userSelect+` FROM users u WHERE u.id=$1`, id))
}

// UserByEmail deliberately does not match usernames when resolving an OAuth identity.
func (s *Store) UserByEmail(ctx context.Context, email string) (User, error) {
	return scanUser(s.pool.QueryRow(ctx, userSelect+` FROM users u WHERE lower(u.email)=$1`, strings.ToLower(strings.TrimSpace(email))))
}

// MarkOnboarded records that the user finished the first-run welcome. It is
// idempotent: the first timestamp wins, so double clicks and retries are safe.
func (s *Store) MarkOnboarded(ctx context.Context, id string) (time.Time, error) {
	var at time.Time
	err := s.pool.QueryRow(ctx, `UPDATE users SET onboarded_at = COALESCE(onboarded_at, now()), updated_at = now() WHERE id=$1 RETURNING onboarded_at`, id).Scan(&at)
	return at, err
}

// ProjectOwner returns the user that owns a project.
func (s *Store) ProjectOwner(ctx context.Context, projectID string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(user_id,'') FROM projects WHERE id=$1`, projectID).Scan(&id)
	return id, err
}

// ListTrialEndedUsers returns accounts whose free trial is over and that support
// has not marked active. Their compute must stay stopped.
func (s *Store) ListTrialEndedUsers(ctx context.Context, trialDays int) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id FROM users
		WHERE COALESCE(billing_status,'trial') <> 'active'
		  AND trial_started_at + make_interval(days => $1) <= now()`, trialDays)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SetUserTheme stores the dashboard appearance the user picked ("system", "light", or "dark").
func (s *Store) SetUserTheme(ctx context.Context, id, theme string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE users SET theme=$2, updated_at=now() WHERE id=$1`, id, theme)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errNotFound
	}
	return nil
}

// RefreshOAuthProfile keeps the photo current and fills a missing display name.
// A name the user already has is never overwritten.
func (s *Store) RefreshOAuthProfile(ctx context.Context, id, name, avatarURL string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE users
		SET avatar_url = CASE WHEN $3 <> '' THEN $3 ELSE avatar_url END,
		    name = CASE WHEN btrim(name) = '' AND $2 <> '' THEN $2 ELSE name END,
		    updated_at = now()
		WHERE id=$1 AND (($3 <> '' AND avatar_url IS DISTINCT FROM $3) OR (btrim(name) = '' AND $2 <> ''))
	`, id, strings.TrimSpace(name), strings.TrimSpace(avatarURL))
	return err
}

// ReplaceGeneratedUsername swaps an opaque generated handle (for example
// google_<hex>) for a readable one. It only touches rows that still carry the
// expected old handle, so a concurrent rename can never be overwritten.
func (s *Store) ReplaceGeneratedUsername(ctx context.Context, id, from, to string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE users SET username=$3, updated_at=now() WHERE id=$1 AND username=$2`, id, from, to)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Store) UserByLogin(ctx context.Context, login string) (User, error) {
	login = strings.ToLower(strings.TrimSpace(login))
	return scanUser(s.pool.QueryRow(ctx, userSelect+` FROM users u WHERE lower(u.username)=$1 OR lower(u.email)=$1`, login))
}

func (s *Store) UsernameTaken(ctx context.Context, username string) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(1)::int FROM users WHERE lower(username)=lower($1)`, username).Scan(&n)
	return n > 0, err
}

func (s *Store) EmailTaken(ctx context.Context, email string) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(1)::int FROM users WHERE lower(email)=lower($1)`, email).Scan(&n)
	return n > 0, err
}

func (s *Store) CreateSession(ctx context.Context, id, userID string, ttl time.Duration) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO sessions (id, user_id, expires_at) VALUES ($1,$2,$3)`, id, userID, time.Now().Add(ttl))
	return err
}

func (s *Store) UserBySession(ctx context.Context, sessionID string) (User, error) {
	return scanUser(s.pool.QueryRow(ctx, userSelect+`
		FROM users u
		JOIN sessions s ON s.user_id = u.id
		WHERE s.id=$1 AND s.expires_at > now() AND u.revoked_at IS NULL`, sessionID))
}

func (s *Store) ListAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.username, u.email, u.name, COALESCE(u.role,'user'), u.created_at, u.revoked_at,
		       EXISTS (
		         SELECT 1 FROM sessions s
		         WHERE s.user_id = u.id AND s.expires_at > now()
		       )
		FROM users u
		ORDER BY (u.revoked_at IS NOT NULL), lower(u.username)
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Account{}
	for rows.Next() {
		var item Account
		if err := rows.Scan(&item.ID, &item.Username, &item.Email, &item.Name, &item.Role, &item.CreatedAt, &item.RevokedAt, &item.SignedIn); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) GrantAdmin(ctx context.Context, id, passwordHash string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE users
		SET password_hash=$2, role=$3, revoked_at=NULL, updated_at=now()
		WHERE id=$1
	`, id, passwordHash, RoleAdmin)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errNotFound
	}
	return nil
}

func (s *Store) SetRevoked(ctx context.Context, id string, revoked bool) (Account, error) {
	var item Account
	err := s.pool.QueryRow(ctx, `
		UPDATE users
		SET revoked_at = CASE WHEN $2 THEN now() ELSE NULL END,
		    updated_at = now()
		WHERE id=$1
		RETURNING id, username, email, name, COALESCE(role,'user'), created_at, revoked_at, false
	`, id, revoked).Scan(&item.ID, &item.Username, &item.Email, &item.Name, &item.Role, &item.CreatedAt, &item.RevokedAt, &item.SignedIn)
	return item, err
}

func (s *Store) DeleteUserSessions(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE user_id=$1`, userID)
	return err
}

func (s *Store) CountActiveAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(1)::int FROM users
		WHERE role=$1 AND revoked_at IS NULL
	`, RoleAdmin).Scan(&n)
	return n, err
}

func (s *Store) DeleteSession(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE id=$1`, id)
	return err
}

func (s *Store) PruneExpiredSessions(ctx context.Context) {
	_, _ = s.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= now()`)
}

func (s *Store) ClaimOrphans(ctx context.Context, userID string) error {
	// Intentionally a no-op. Claiming every ownerless project on signup
	// handed another user's leftover workspaces to whoever registered next.
	_ = userID
	return nil
}

type ConnectIntent struct {
	ID         string
	UserID     string
	ReturnPath string
}

func (s *Store) CreateConnectIntent(ctx context.Context, userID, returnPath string, ttl time.Duration) (string, error) {
	id := NewSessionID()
	_, _ = s.pool.Exec(ctx, `DELETE FROM github_connect_intents WHERE expires_at <= now() OR user_id=$1`, userID)
	_, err := s.pool.Exec(ctx, `
		INSERT INTO github_connect_intents (id, user_id, return_path, expires_at)
		VALUES ($1,$2,$3,$4)
	`, id, userID, returnPath, time.Now().Add(ttl))
	return id, err
}

func (s *Store) GetConnectIntent(ctx context.Context, id string) (ConnectIntent, error) {
	var item ConnectIntent
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, return_path
		FROM github_connect_intents
		WHERE id=$1 AND expires_at > now()
	`, id).Scan(&item.ID, &item.UserID, &item.ReturnPath)
	return item, err
}

func (s *Store) LatestConnectIntent(ctx context.Context, userID string) (ConnectIntent, error) {
	var item ConnectIntent
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, return_path
		FROM github_connect_intents
		WHERE user_id=$1 AND expires_at > now()
		ORDER BY created_at DESC
		LIMIT 1
	`, userID).Scan(&item.ID, &item.UserID, &item.ReturnPath)
	return item, err
}

func (s *Store) DeleteConnectIntent(ctx context.Context, id string) {
	if id == "" {
		return
	}
	_, _ = s.pool.Exec(ctx, `DELETE FROM github_connect_intents WHERE id=$1`, id)
}

func (s *Store) LinkUserInstallation(ctx context.Context, userID string, installationID int64) error {
	// The same GitHub App install may belong to many Runex logins.
	// Isolation is the join row: a user only sees installs they connected.
	_, err := s.pool.Exec(ctx, `
		INSERT INTO github_user_installations (user_id, installation_id)
		VALUES ($1,$2)
		ON CONFLICT DO NOTHING
	`, userID, installationID)
	return err
}

func (s *Store) InstallationOwned(ctx context.Context, userID string, id int64) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(1)::int FROM github_user_installations
		WHERE user_id=$1 AND installation_id=$2
	`, userID, id).Scan(&n)
	return n > 0, err
}

func scanUser(row interface{ Scan(dest ...any) error }) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.Email, &u.PasswordHash, &u.Name, &u.AvatarURL, &u.Theme, &u.Role, &u.RevokedAt, &u.CreatedAt, &u.TrialStartedAt, &u.OnboardedAt, &u.BillingStatus)
	return u, err
}
