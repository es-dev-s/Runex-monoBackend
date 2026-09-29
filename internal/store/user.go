package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"
)

type User struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"-"`
	Name         string    `json:"name"`
	AvatarURL    string    `json:"avatarUrl,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
}

func NewSessionID() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf)
}

const userSelect = `SELECT u.id, u.username, u.email, COALESCE(u.password_hash,''), u.name, u.avatar_url, u.created_at`

func (s *Store) CreateUser(ctx context.Context, u User) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO users (id, username, email, password_hash, name)
		VALUES ($1,$2,$3,$4,$5)
	`, u.ID, u.Username, strings.ToLower(strings.TrimSpace(u.Email)), u.PasswordHash, u.Name)
	return err
}

func (s *Store) UserByID(ctx context.Context, id string) (User, error) {
	return scanUser(s.pool.QueryRow(ctx, userSelect+` FROM users u WHERE u.id=$1`, id))
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
		WHERE s.id=$1 AND s.expires_at > now()`, sessionID))
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
	err := row.Scan(&u.ID, &u.Username, &u.Email, &u.PasswordHash, &u.Name, &u.AvatarURL, &u.CreatedAt)
	return u, err
}
