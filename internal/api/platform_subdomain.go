package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"deploy_platform/internal/edge"
	"deploy_platform/internal/store"
)

type platformSubdomainBody struct {
	Subdomain string `json:"subdomain"`
}

// setPlatformSubdomain claims or clears a free *.EdgeDomain hostname for a
// service (e.g. pawan.salesradar.live). Custom domains stay on external DNS;
// reserved platform hosts remain blocked and uniqueness is enforced.
func (s *Server) setPlatformSubdomain(w http.ResponseWriter, r *http.Request) {
	project, node, err := s.loadServiceNode(w, r)
	if err != nil {
		return
	}
	var body platformSubdomainBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "JSON body with subdomain is required")
		return
	}
	raw := strings.TrimSpace(body.Subdomain)
	if raw == "" {
		if err := s.store.SetPlatformSlug(r.Context(), project.ID, node.ID, ""); err != nil {
			writeErr(w, http.StatusInternalServerError, "db", "Could not clear platform subdomain")
			return
		}
		fresh, err := s.store.GetNode(r.Context(), project.ID, node.ID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "db", "Could not load service")
			return
		}
		named := s.namedServices(r.Context(), []store.Node{fresh})
		writeJSON(w, http.StatusOK, map[string]any{
			"platformSlug": "",
			"platformUrl":  s.publicNodeURL(fresh, named),
		})
		return
	}
	slug, err := edge.ValidatePlatformSlug(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_subdomain", err.Error())
		return
	}
	taken, err := s.store.PlatformSlugTaken(r.Context(), slug, node.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not check subdomain availability")
		return
	}
	if taken {
		writeErr(w, http.StatusConflict, "subdomain_taken", "That platform subdomain is already in use.")
		return
	}
	if err := s.store.SetPlatformSlug(r.Context(), project.ID, node.ID, slug); err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "unique") || strings.Contains(msg, "duplicate") {
			writeErr(w, http.StatusConflict, "subdomain_taken", "That platform subdomain is already in use.")
			return
		}
		writeErr(w, http.StatusInternalServerError, "db", "Could not set platform subdomain")
		return
	}
	fresh, err := s.store.GetNode(r.Context(), project.ID, node.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not load service")
		return
	}
	named := s.namedServices(r.Context(), []store.Node{fresh})
	writeJSON(w, http.StatusOK, map[string]any{
		"platformSlug": fresh.PlatformSlug,
		"platformUrl":  s.publicNodeURL(fresh, named),
	})
}
