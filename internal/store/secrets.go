package store

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"time"
)

const (
	SecretKDF           = "pbkdf2-sha256"
	SecretSessionKDF    = "session-aes-gcm"
	SecretUnlockPass    = "passphrase"
	SecretUnlockSession = "session"
	SecretMinKDFIters   = 210_000
	SecretMaxKDFIters   = 1_200_000
	SecretMaxGroups     = 200
	SecretMaxItems      = 2_000
	SecretMaxBulkItems  = 200
)

var (
	ErrSecretVaultExists   = errors.New("vault already exists")
	ErrSecretVaultMissing  = errors.New("vault not found")
	ErrSecretLimit         = errors.New("secret store is full")
	ErrSecretInvalidBlob   = errors.New("invalid ciphertext")
	ErrSecretGroupMissing  = errors.New("group not found")
	ErrSecretItemMissing   = errors.New("secret not found")
	ErrSecretGroupMismatch = errors.New("group does not belong to this vault")
)

type SecretVault struct {
	UserID     string    `json:"-"`
	UnlockMode string    `json:"unlock"`
	KDF        string    `json:"kdf"`
	KDFIters   int       `json:"kdfIters"`
	Salt       string    `json:"salt"`
	WrapIV     string    `json:"wrapIv"`
	WrappedDEK string    `json:"wrappedDek"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

func (v SecretVault) SessionUnlock() bool {
	return strings.EqualFold(strings.TrimSpace(v.UnlockMode), SecretUnlockSession) || v.KDF == SecretSessionKDF
}

type SecretGroup struct {
	ID        string    `json:"id"`
	NameIV    string    `json:"nameIv"`
	NameCT    string    `json:"nameCt"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	UserID    string    `json:"-"`
}

type SecretItem struct {
	ID        string    `json:"id"`
	GroupID   string    `json:"groupId,omitempty"`
	BodyIV    string    `json:"bodyIv"`
	BodyCT    string    `json:"bodyCt"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	UserID    string    `json:"-"`
}

func ValidSecretB64(raw string, decodedMin, decodedMax int) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	buf, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return false
	}
	return len(buf) >= decodedMin && len(buf) <= decodedMax
}

func NormalizeSecretVault(v SecretVault) (SecretVault, error) {
	v.KDF = strings.ToLower(strings.TrimSpace(v.KDF))
	v.UnlockMode = strings.ToLower(strings.TrimSpace(v.UnlockMode))
	if v.KDF == "" {
		if v.UnlockMode == SecretUnlockSession {
			v.KDF = SecretSessionKDF
		} else {
			v.KDF = SecretKDF
		}
	}
	if v.UnlockMode == "" {
		if v.KDF == SecretSessionKDF {
			v.UnlockMode = SecretUnlockSession
		} else {
			v.UnlockMode = SecretUnlockPass
		}
	}
	if v.UnlockMode != SecretUnlockPass && v.UnlockMode != SecretUnlockSession {
		return v, ErrSecretInvalidBlob
	}
	if v.UnlockMode == SecretUnlockSession {
		if v.KDF != SecretSessionKDF {
			return v, ErrSecretInvalidBlob
		}
		if v.KDFIters < 1 {
			v.KDFIters = 1
		}
	} else {
		if v.KDF != SecretKDF {
			return v, ErrSecretInvalidBlob
		}
		if v.KDFIters < SecretMinKDFIters || v.KDFIters > SecretMaxKDFIters {
			return v, ErrSecretInvalidBlob
		}
	}
	if !ValidSecretB64(v.Salt, 16, 64) || !ValidSecretB64(v.WrapIV, 12, 12) || !ValidSecretB64(v.WrappedDEK, 32, 128) {
		return v, ErrSecretInvalidBlob
	}
	v.Salt = strings.TrimSpace(v.Salt)
	v.WrapIV = strings.TrimSpace(v.WrapIV)
	v.WrappedDEK = strings.TrimSpace(v.WrappedDEK)
	return v, nil
}

func NormalizeSecretCipher(iv, ct string, ctMax int) (string, string, error) {
	iv = strings.TrimSpace(iv)
	ct = strings.TrimSpace(ct)
	if !ValidSecretB64(iv, 12, 12) || !ValidSecretB64(ct, 16, ctMax) {
		return "", "", ErrSecretInvalidBlob
	}
	return iv, ct, nil
}

func (s *Store) GetSecretVault(ctx context.Context, userID string) (SecretVault, error) {
	var v SecretVault
	err := s.pool.QueryRow(ctx, `
		SELECT user_id, COALESCE(unlock_mode, 'passphrase'), kdf, kdf_iters, salt, wrap_iv, wrapped_dek, created_at, updated_at
		FROM secret_vaults WHERE user_id = $1
	`, userID).Scan(&v.UserID, &v.UnlockMode, &v.KDF, &v.KDFIters, &v.Salt, &v.WrapIV, &v.WrappedDEK, &v.CreatedAt, &v.UpdatedAt)
	return v, err
}

func (s *Store) CreateSecretVault(ctx context.Context, v SecretVault) error {
	v, err := NormalizeSecretVault(v)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO secret_vaults (user_id, unlock_mode, kdf, kdf_iters, salt, wrap_iv, wrapped_dek)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, v.UserID, v.UnlockMode, v.KDF, v.KDFIters, v.Salt, v.WrapIV, v.WrappedDEK)
	if IsUniqueViolation(err) {
		return ErrSecretVaultExists
	}
	return err
}

func (s *Store) UpdateSecretVaultWrap(ctx context.Context, v SecretVault) error {
	v, err := NormalizeSecretVault(v)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE secret_vaults
		SET unlock_mode = $2, kdf = $3, kdf_iters = $4, salt = $5, wrap_iv = $6, wrapped_dek = $7, updated_at = now()
		WHERE user_id = $1
	`, v.UserID, v.UnlockMode, v.KDF, v.KDFIters, v.Salt, v.WrapIV, v.WrappedDEK)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrSecretVaultMissing
	}
	return nil
}

func (s *Store) ListSecretGroups(ctx context.Context, userID string) ([]SecretGroup, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, name_iv, name_ct, created_at, updated_at
		FROM secret_groups WHERE user_id = $1
		ORDER BY created_at DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SecretGroup{}
	for rows.Next() {
		var g SecretGroup
		if err := rows.Scan(&g.ID, &g.UserID, &g.NameIV, &g.NameCT, &g.CreatedAt, &g.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) CountSecretGroups(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM secret_groups WHERE user_id = $1`, userID).Scan(&n)
	return n, err
}

func (s *Store) CountSecretItems(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM secret_items WHERE user_id = $1`, userID).Scan(&n)
	return n, err
}

func (s *Store) CreateSecretGroup(ctx context.Context, g SecretGroup) error {
	if strings.TrimSpace(g.ID) == "" || strings.TrimSpace(g.UserID) == "" {
		return ErrSecretInvalidBlob
	}
	iv, ct, err := NormalizeSecretCipher(g.NameIV, g.NameCT, 4096)
	if err != nil {
		return err
	}
	n, err := s.CountSecretGroups(ctx, g.UserID)
	if err != nil {
		return err
	}
	if n >= SecretMaxGroups {
		return ErrSecretLimit
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO secret_groups (id, user_id, name_iv, name_ct)
		VALUES ($1, $2, $3, $4)
	`, g.ID, g.UserID, iv, ct)
	return err
}

func (s *Store) UpdateSecretGroup(ctx context.Context, userID, id, nameIV, nameCT string) error {
	iv, ct, err := NormalizeSecretCipher(nameIV, nameCT, 4096)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE secret_groups SET name_iv = $3, name_ct = $4, updated_at = now()
		WHERE user_id = $1 AND id = $2
	`, userID, id, iv, ct)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrSecretGroupMissing
	}
	return nil
}

func (s *Store) DeleteSecretGroup(ctx context.Context, userID, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM secret_groups WHERE user_id = $1 AND id = $2`, userID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrSecretGroupMissing
	}
	return nil
}

func (s *Store) groupOwned(ctx context.Context, userID, groupID string) error {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" {
		return nil
	}
	var found string
	err := s.pool.QueryRow(ctx, `SELECT id FROM secret_groups WHERE user_id = $1 AND id = $2`, userID, groupID).Scan(&found)
	if err != nil {
		return ErrSecretGroupMismatch
	}
	return nil
}

func (s *Store) ListSecretItems(ctx context.Context, userID string) ([]SecretItem, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, COALESCE(group_id, ''), body_iv, body_ct, created_at, updated_at
		FROM secret_items WHERE user_id = $1
		ORDER BY created_at DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SecretItem{}
	for rows.Next() {
		var item SecretItem
		if err := rows.Scan(&item.ID, &item.UserID, &item.GroupID, &item.BodyIV, &item.BodyCT, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) CreateSecretItem(ctx context.Context, item SecretItem) error {
	return s.CreateSecretItems(ctx, item.UserID, []SecretItem{item})
}

func (s *Store) CreateSecretItems(ctx context.Context, userID string, items []SecretItem) error {
	if len(items) == 0 {
		return nil
	}
	if len(items) > SecretMaxBulkItems {
		return ErrSecretLimit
	}
	existing, err := s.CountSecretItems(ctx, userID)
	if err != nil {
		return err
	}
	if existing+len(items) > SecretMaxItems {
		return ErrSecretLimit
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, item := range items {
		if strings.TrimSpace(item.ID) == "" {
			return ErrSecretInvalidBlob
		}
		iv, ct, err := NormalizeSecretCipher(item.BodyIV, item.BodyCT, 32*1024)
		if err != nil {
			return err
		}
		groupID := strings.TrimSpace(item.GroupID)
		if groupID != "" {
			var found string
			if err := tx.QueryRow(ctx, `SELECT id FROM secret_groups WHERE user_id = $1 AND id = $2`, userID, groupID).Scan(&found); err != nil {
				return ErrSecretGroupMismatch
			}
		}
		var group any
		if groupID != "" {
			group = groupID
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO secret_items (id, user_id, group_id, body_iv, body_ct)
			VALUES ($1, $2, $3, $4, $5)
		`, item.ID, userID, group, iv, ct); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) UpdateSecretItem(ctx context.Context, userID, id, groupID, bodyIV, bodyCT string) error {
	iv, ct, err := NormalizeSecretCipher(bodyIV, bodyCT, 32*1024)
	if err != nil {
		return err
	}
	if err := s.groupOwned(ctx, userID, groupID); err != nil {
		return err
	}
	var group any
	if strings.TrimSpace(groupID) != "" {
		group = strings.TrimSpace(groupID)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE secret_items
		SET group_id = $3, body_iv = $4, body_ct = $5, updated_at = now()
		WHERE user_id = $1 AND id = $2
	`, userID, id, group, iv, ct)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrSecretItemMissing
	}
	return nil
}

func (s *Store) DeleteSecretItem(ctx context.Context, userID, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM secret_items WHERE user_id = $1 AND id = $2`, userID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrSecretItemMissing
	}
	return nil
}
