package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"deploy_platform/internal/ident"
	"deploy_platform/internal/store"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

func (s *Server) adminRevokeUser(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	target, err := s.store.UserByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "That account does not exist")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not update that account")
		return
	}
	admins, err := s.store.CountActiveAdmins(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not update that account")
		return
	}
	if err := revokeGuard(actor, target, admins); err != nil {
		writeErr(w, http.StatusConflict, "revoke_refused", err.Error())
		return
	}
	if target.RevokedAt != nil {
		_ = s.store.DeleteUserSessions(r.Context(), target.ID)
		s.forgetUserSessions(target.ID)
		s.kick(target.ID)
		s.wakeAdmins()
		s.writeAdminUser(w, r, target.ID)
		return
	}
	if err := s.store.DeleteUserSessions(r.Context(), target.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not sign that account out")
		return
	}
	s.forgetUserSessions(target.ID)
	s.kick(target.ID)
	if _, err := s.store.SetRevoked(r.Context(), target.ID, true); err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not revoke that account")
		return
	}
	s.wakeAdmins()
	s.writeAdminUser(w, r, target.ID)
}

func (s *Server) adminRestoreUser(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	id := r.PathValue("id")
	if _, err := s.store.UserByID(r.Context(), id); errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "That account does not exist")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not update that account")
		return
	}
	if _, err := s.store.SetRevoked(r.Context(), id, false); err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not restore that account")
		return
	}
	s.wakeAdmins()
	s.writeAdminUser(w, r, id)
}

func (s *Server) writeAdminUser(w http.ResponseWriter, r *http.Request, id string) {
	if row, ok := s.adminRow(r.Context(), id); ok {
		writeJSON(w, http.StatusOK, map[string]any{"user": row})
		return
	}
	writeErr(w, http.StatusInternalServerError, "db", "Could not load that account")
}

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	user := userFrom(r)
	if user.Role != store.RoleAdmin || user.RevokedAt != nil {
		writeErr(w, http.StatusForbidden, "forbidden", "You don't have access to this.")
		return store.User{}, false
	}
	return user, true
}

func revokeGuard(actor, target store.User, activeAdmins int) error {
	if actor.ID == "" || target.ID == "" {
		return errors.New("That account does not exist")
	}
	if actor.ID == target.ID {
		return errors.New("You can't revoke your own access.")
	}
	if target.Role == store.RoleAdmin && target.RevokedAt == nil && activeAdmins <= 1 {
		return errors.New("The last administrator can't be revoked.")
	}
	return nil
}

// SeedAdmin creates or updates the predefined administrator.
// Credentials come from the caller. This function never logs them.
func SeedAdmin(ctx context.Context, st *store.Store, username, email, password string) (created bool, err error) {
	username = strings.TrimSpace(username)
	email = strings.ToLower(strings.TrimSpace(email))
	if err := validateUsername(username); err != nil {
		return false, err
	}
	if err := validateEmail(email); err != nil {
		return false, err
	}
	if err := validatePassword(password); err != nil {
		return false, err
	}
	byEmail, emailErr := st.UserByLogin(ctx, email)
	byName, nameErr := st.UserByLogin(ctx, username)
	emailUser, err := foundUser(byEmail, emailErr)
	if err != nil {
		return false, err
	}
	nameUser, err := foundUser(byName, nameErr)
	if err != nil {
		return false, err
	}
	existingID, err := resolveAdminSeed(emailUser, nameUser, username, email)
	if err != nil {
		return false, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return false, errors.New("Could not prepare the administrator")
	}
	if existingID == "" {
		user := store.User{
			ID:           ident.New("usr_"),
			Username:     username,
			Email:        email,
			PasswordHash: string(hash),
			Name:         username,
		}
		if err := st.CreateUser(ctx, user); err != nil {
			return false, err
		}
		existingID = user.ID
		created = true
	}
	if err := st.GrantAdmin(ctx, existingID, string(hash)); err != nil {
		return false, err
	}
	return created, nil
}

func foundUser(user store.User, err error) (*store.User, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &user, nil
}

func resolveAdminSeed(emailUser, nameUser *store.User, username, email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	username = strings.ToLower(strings.TrimSpace(username))
	switch {
	case emailUser == nil && nameUser == nil:
		return "", nil
	case emailUser != nil && nameUser != nil && emailUser.ID != nameUser.ID:
		return "", errors.New("That username and email belong to different accounts")
	}
	existing := emailUser
	if existing == nil {
		existing = nameUser
	}
	if !strings.EqualFold(existing.Username, username) || !strings.EqualFold(existing.Email, email) {
		return "", errors.New("That username or email is already used by another account")
	}
	return existing.ID, nil
}
