package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"deploy_platform/internal/ident"
	"deploy_platform/internal/store"
	"deploy_platform/internal/vaultkey"

	"github.com/jackc/pgx/v5"
)

func (s *Server) secretsBundle(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	vault, err := s.ensureUserVault(r.Context(), user.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not open the vault")
		return
	}
	groups, err := s.store.ListSecretGroups(r.Context(), user.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not load secret groups")
		return
	}
	items, err := s.store.ListSecretItems(r.Context(), user.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not load secrets")
		return
	}
	view, err := s.vaultViewForOwner(vault)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "vault", "Could not unlock the vault for this login")
		return
	}
	writePrivateJSON(w, http.StatusOK, map[string]any{
		"vault":  view,
		"groups": groups,
		"items":  items,
	})
}

func vaultView(v store.SecretVault, exists bool) map[string]any {
	if !exists {
		return map[string]any{"exists": false, "unlock": ""}
	}
	view := map[string]any{
		"exists":    true,
		"unlock":    store.SecretUnlockPass,
		"createdAt": v.CreatedAt,
		"updatedAt": v.UpdatedAt,
	}
	if v.SessionUnlock() {
		view["unlock"] = store.SecretUnlockSession
		return view
	}
	view["kdf"] = v.KDF
	view["kdfIters"] = v.KDFIters
	view["salt"] = v.Salt
	view["wrapIv"] = v.WrapIV
	view["wrappedDek"] = v.WrappedDEK
	return view
}

func (s *Server) putSecretVault(w http.ResponseWriter, r *http.Request) {
	writeErr(w, http.StatusGone, "vault_login", "This vault opens with your Runex login. Sign in instead of setting a passphrase.")
}

func (s *Server) attachSecretVault(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	var body struct {
		DEK string `json:"dek"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "Invalid vault payload")
		return
	}
	dek, err := vaultkey.ParseDEK(body.DEK)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "That key cannot be attached to this login")
		return
	}
	vault, err := s.store.GetSecretVault(r.Context(), user.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if _, err := s.writeSessionVault(r.Context(), user.ID, dek, false); err != nil {
				writeSecretErr(w, err, "Could not attach the vault")
				return
			}
			writePrivateJSON(w, http.StatusCreated, map[string]any{"ok": true, "unlock": store.SecretUnlockSession})
			return
		}
		writeErr(w, http.StatusInternalServerError, "db", "Could not load the vault")
		return
	}
	if vault.SessionUnlock() {
		writePrivateJSON(w, http.StatusOK, map[string]any{"ok": true, "unlock": store.SecretUnlockSession})
		return
	}
	if _, err := s.writeSessionVault(r.Context(), user.ID, dek, true); err != nil {
		writeSecretErr(w, err, "Could not attach the vault")
		return
	}
	writePrivateJSON(w, http.StatusOK, map[string]any{"ok": true, "unlock": store.SecretUnlockSession})
}

func (s *Server) createSecretGroup(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	if !s.requireVault(w, r, user.ID) {
		return
	}
	var body struct {
		NameIV string `json:"nameIv"`
		NameCT string `json:"nameCt"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "Invalid group payload")
		return
	}
	group := store.SecretGroup{
		ID:     ident.New("sgrp_"),
		UserID: user.ID,
		NameIV: body.NameIV,
		NameCT: body.NameCT,
	}
	if err := s.store.CreateSecretGroup(r.Context(), group); err != nil {
		writeSecretErr(w, err, "Could not create the group")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"group": group})
}

func (s *Server) patchSecretGroup(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request", "Missing group")
		return
	}
	var body struct {
		NameIV string `json:"nameIv"`
		NameCT string `json:"nameCt"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "Invalid group payload")
		return
	}
	if err := s.store.UpdateSecretGroup(r.Context(), user.ID, id, body.NameIV, body.NameCT); err != nil {
		writeSecretErr(w, err, "Could not rename the group")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) deleteSecretGroup(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request", "Missing group")
		return
	}
	if err := s.store.DeleteSecretGroup(r.Context(), user.ID, id); err != nil {
		writeSecretErr(w, err, "Could not delete the group")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) createSecretItems(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	if !s.requireVault(w, r, user.ID) {
		return
	}
	var body struct {
		Items []struct {
			GroupID string `json:"groupId"`
			BodyIV  string `json:"bodyIv"`
			BodyCT  string `json:"bodyCt"`
		} `json:"items"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&body); err != nil || len(body.Items) == 0 {
		writeErr(w, http.StatusBadRequest, "invalid_request", "Add at least one encrypted secret")
		return
	}
	items := make([]store.SecretItem, 0, len(body.Items))
	for _, row := range body.Items {
		items = append(items, store.SecretItem{
			ID:      ident.New("sitm_"),
			UserID:  user.ID,
			GroupID: row.GroupID,
			BodyIV:  row.BodyIV,
			BodyCT:  row.BodyCT,
		})
	}
	if err := s.store.CreateSecretItems(r.Context(), user.ID, items); err != nil {
		writeSecretErr(w, err, "Could not save secrets")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"items": items})
}

func (s *Server) patchSecretItem(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request", "Missing secret")
		return
	}
	var body struct {
		GroupID *string `json:"groupId"`
		BodyIV  string  `json:"bodyIv"`
		BodyCT  string  `json:"bodyCt"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "Invalid secret payload")
		return
	}
	groupID := ""
	if body.GroupID != nil {
		groupID = *body.GroupID
	}
	if err := s.store.UpdateSecretItem(r.Context(), user.ID, id, groupID, body.BodyIV, body.BodyCT); err != nil {
		writeSecretErr(w, err, "Could not update the secret")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) deleteSecretItem(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request", "Missing secret")
		return
	}
	if err := s.store.DeleteSecretItem(r.Context(), user.ID, id); err != nil {
		writeSecretErr(w, err, "Could not delete the secret")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) ensureUserVault(ctx context.Context, userID string) (store.SecretVault, error) {
	vault, err := s.store.GetSecretVault(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		dek := make([]byte, vaultkey.DEKSize)
		if _, err := rand.Read(dek); err != nil {
			return store.SecretVault{}, err
		}
		return s.writeSessionVault(ctx, userID, dek, false)
	}
	if err != nil {
		return store.SecretVault{}, err
	}
	if vault.SessionUnlock() {
		return vault, nil
	}
	groups, err := s.store.CountSecretGroups(ctx, userID)
	if err != nil {
		return store.SecretVault{}, err
	}
	items, err := s.store.CountSecretItems(ctx, userID)
	if err != nil {
		return store.SecretVault{}, err
	}
	if groups == 0 && items == 0 {
		dek := make([]byte, vaultkey.DEKSize)
		if _, err := rand.Read(dek); err != nil {
			return store.SecretVault{}, err
		}
		return s.writeSessionVault(ctx, userID, dek, true)
	}
	return vault, nil
}

func (s *Server) writeSessionVault(ctx context.Context, userID string, dek []byte, replace bool) (store.SecretVault, error) {
	root, err := s.platformVaultKey()
	if err != nil {
		return store.SecretVault{}, err
	}
	iv, ct, err := vaultkey.WrapDEK(root, dek)
	if err != nil {
		return store.SecretVault{}, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return store.SecretVault{}, err
	}
	vault := store.SecretVault{
		UserID:     userID,
		UnlockMode: store.SecretUnlockSession,
		KDF:        store.SecretSessionKDF,
		KDFIters:   1,
		Salt:       base64.StdEncoding.EncodeToString(salt),
		WrapIV:     iv,
		WrappedDEK: ct,
	}
	if replace {
		if err := s.store.UpdateSecretVaultWrap(ctx, vault); err != nil {
			return store.SecretVault{}, err
		}
	} else if err := s.store.CreateSecretVault(ctx, vault); err != nil {
		if errors.Is(err, store.ErrSecretVaultExists) {
			return s.store.GetSecretVault(ctx, userID)
		}
		return store.SecretVault{}, err
	}
	return s.store.GetSecretVault(ctx, userID)
}

func (s *Server) vaultViewForOwner(v store.SecretVault) (map[string]any, error) {
	view := vaultView(v, v.UserID != "")
	if !v.SessionUnlock() {
		return view, nil
	}
	root, err := s.platformVaultKey()
	if err != nil {
		return nil, err
	}
	dek, err := vaultkey.UnwrapDEK(root, v.WrapIV, v.WrappedDEK)
	if err != nil {
		return nil, err
	}
	view["dek"] = base64.StdEncoding.EncodeToString(dek)
	delete(view, "salt")
	delete(view, "wrapIv")
	delete(view, "wrappedDek")
	return view, nil
}

func (s *Server) platformVaultKey() ([]byte, error) {
	s.vaultOnce.Do(func() {
		s.vaultKey, s.vaultErr = vaultkey.Load(s.cfg.VaultKey, filepath.Join(s.cfg.WorkDir, ".vault-root"))
	})
	return s.vaultKey, s.vaultErr
}

func (s *Server) requireVault(w http.ResponseWriter, r *http.Request, userID string) bool {
	if _, err := s.store.GetSecretVault(r.Context(), userID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeErr(w, http.StatusConflict, "vault_missing", "Create a vault passphrase first")
			return false
		}
		writeErr(w, http.StatusInternalServerError, "db", "Could not load the vault")
		return false
	}
	return true
}

func writeSecretErr(w http.ResponseWriter, err error, fallback string) {
	switch {
	case errors.Is(err, store.ErrSecretInvalidBlob):
		writeErr(w, http.StatusBadRequest, "invalid_request", "Ciphertext is not a valid encrypted secret")
	case errors.Is(err, store.ErrSecretVaultExists):
		writeErr(w, http.StatusConflict, "vault_exists", "A vault already exists for this account")
	case errors.Is(err, store.ErrSecretVaultMissing), errors.Is(err, store.ErrSecretGroupMissing), errors.Is(err, store.ErrSecretItemMissing):
		writeErr(w, http.StatusNotFound, "not_found", fallback)
	case errors.Is(err, store.ErrSecretLimit):
		writeErr(w, http.StatusConflict, "vault_full", "This vault has reached its size limit")
	case errors.Is(err, store.ErrSecretGroupMismatch):
		writeErr(w, http.StatusBadRequest, "invalid_request", "That group is not in this vault")
	default:
		writeErr(w, http.StatusInternalServerError, "db", fallback)
	}
}
