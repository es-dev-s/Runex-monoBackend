package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"deploy_platform/internal/customdomain"
	"deploy_platform/internal/ident"
	"deploy_platform/internal/store"

	"github.com/jackc/pgx/v5"
)

func (s *Server) listDomains(w http.ResponseWriter, r *http.Request) {
	project, node, err := s.loadServiceNode(w, r)
	if err != nil {
		return
	}
	items, err := s.store.ListCustomDomains(r.Context(), userFrom(r).ID, project.ID, node.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not load domains")
		return
	}
	views := make([]domainView, 0, len(items))
	for _, item := range items {
		views = append(views, s.viewDomain(item))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"domains":     views,
		"platformUrl": s.publicNodeURL(node, s.namedServices(r.Context(), []store.Node{node})),
		"cnameTarget": s.cfg.DomainCNAME(),
	})
}

func (s *Server) addDomain(w http.ResponseWriter, r *http.Request) {
	if s.domainLimit != nil && !s.domainLimit.allow(userFrom(r).ID) {
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "Too many domain changes. Try again in a few minutes.")
		return
	}
	project, node, err := s.loadServiceNode(w, r)
	if err != nil {
		return
	}
	var body struct {
		Hostname string `json:"hostname"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "JSON body with hostname is required")
		return
	}
	hostname, err := customdomain.Normalize(body.Hostname)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_hostname", "Enter a valid domain such as app.example.com")
		return
	}
	if customdomain.PlatformHost(s.cfg, hostname) {
		writeErr(w, http.StatusBadRequest, "platform_hostname", "That hostname belongs to Runex. Set a platform subdomain on the service, or use your own custom domain.")
		return
	}
	if existing, getErr := s.store.GetCustomDomainByHost(r.Context(), hostname); getErr == nil {
		if existing.UserID == userFrom(r).ID && existing.ProjectID == project.ID && existing.NodeID == node.ID {
			writeJSON(w, http.StatusOK, map[string]any{"domain": s.viewDomain(existing)})
			return
		}
		writeErr(w, http.StatusConflict, "domain_taken", "This domain is already connected to another service.")
		return
	} else if !errors.Is(getErr, pgx.ErrNoRows) {
		writeErr(w, http.StatusInternalServerError, "db", "Could not add domain")
		return
	}
	created, err := s.store.CreateCustomDomain(r.Context(), store.CustomDomain{
		ID:         ident.New("dom_"),
		UserID:     userFrom(r).ID,
		ProjectID:  project.ID,
		NodeID:     node.ID,
		Hostname:   hostname,
		Status:     customdomain.StatusPending,
		Token:      customdomain.TokenFromSecret(ident.Secret(16)),
		EdgeStatus: s.initialEdgeStatus(),
	}, customdomain.MaxPerUser, customdomain.MaxPerNode)
	if errors.Is(err, store.ErrDomainLimit) {
		writeErr(w, http.StatusConflict, "domain_limit", "This account has reached the custom domain limit.")
		return
	}
	if errors.Is(err, store.ErrDomainTaken) {
		writeErr(w, http.StatusConflict, "domain_taken", "This domain is already connected to another service.")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not add domain")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"domain": s.viewDomain(created)})
}

func (s *Server) verifyDomain(w http.ResponseWriter, r *http.Request) {
	if s.domainLimit != nil && !s.domainLimit.allow("verify:"+userFrom(r).ID) {
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "Too many verify attempts. Try again shortly.")
		return
	}
	project, node, err := s.loadServiceNode(w, r)
	if err != nil {
		return
	}
	item, err := s.store.GetOwnedCustomDomain(r.Context(), userFrom(r).ID, project.ID, node.ID, r.PathValue("domainId"))
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "Domain not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not verify domain")
		return
	}
	updated, err := s.checkCustomDomain(r.Context(), item)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "dns", "Could not check DNS for this domain")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"domain": s.viewDomain(updated)})
}

func (s *Server) deleteDomain(w http.ResponseWriter, r *http.Request) {
	project, node, err := s.loadServiceNode(w, r)
	if err != nil {
		return
	}
	item, err := s.store.GetOwnedCustomDomain(r.Context(), userFrom(r).ID, project.ID, node.ID, r.PathValue("domainId"))
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "Domain not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not delete domain")
		return
	}
	s.unpublishDomain(r.Context(), item)
	if err := s.store.DeleteCustomDomain(r.Context(), userFrom(r).ID, item.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not delete domain")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) loadServiceNode(w http.ResponseWriter, r *http.Request) (store.Project, store.Node, error) {
	project, err := s.loadProject(w, r)
	if err != nil {
		return store.Project{}, store.Node{}, err
	}
	node, err := s.store.GetNode(r.Context(), project.ID, r.PathValue("nodeId"))
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "Service not found")
		return store.Project{}, store.Node{}, err
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not load service")
		return store.Project{}, store.Node{}, err
	}
	if node.Kind != "service" {
		writeErr(w, http.StatusBadRequest, "invalid_request", "Custom domains attach to a deployed service")
		return store.Project{}, store.Node{}, customdomain.ErrNotService
	}
	return project, node, nil
}

type domainView struct {
	ID          string                `json:"id"`
	Hostname    string                `json:"hostname"`
	Status      string                `json:"status"`
	URL         string                `json:"url,omitempty"`
	Records     []customdomain.Record `json:"records"`
	Error       string                `json:"error,omitempty"`
	EdgeStatus  string                `json:"edgeStatus,omitempty"`
	CNAMETarget string                `json:"cnameTarget"`
	CreatedAt   time.Time             `json:"createdAt"`
	VerifiedAt  *time.Time            `json:"verifiedAt,omitempty"`
	LastCheckAt *time.Time            `json:"lastCheckAt,omitempty"`
}

func (s *Server) viewDomain(d store.CustomDomain) domainView {
	target := s.cfg.DomainCNAME()
	view := domainView{
		ID:          d.ID,
		Hostname:    d.Hostname,
		Status:      d.Status,
		Records:     customdomain.Records(d.Hostname, d.Token, target),
		Error:       d.LastError,
		EdgeStatus:  d.EdgeStatus,
		CNAMETarget: target,
		CreatedAt:   d.CreatedAt,
		VerifiedAt:  d.VerifiedAt,
		LastCheckAt: d.LastCheckAt,
	}
	if d.Status == customdomain.StatusActive {
		view.URL = customdomain.PublicURL(d.Hostname)
	}
	return view
}

func (s *Server) checkCustomDomain(ctx context.Context, d store.CustomDomain) (store.CustomDomain, error) {
	_ = s.store.SetCustomDomainCheck(ctx, d.ID, customdomain.StatusVerifying, d.LastError, "", false)
	result := customdomain.Verify(ctx, s.domainResolver(), d.Hostname, d.Token, s.cfg.DomainCNAMETargets()...)
	status := customdomain.StatusPending
	edgeStatus := d.EdgeStatus
	errText := result.Error
	verified := false
	if result.Active {
		published, pubErr := s.publishDomain(ctx, d)
		if published != "" {
			edgeStatus = published
		}
		probe := customdomain.ProbeEdge(ctx, d.Hostname)
		if published == "provisioned" && (probe.Banned || probe.Tunnel) {
			probe = customdomain.ProbeEdgeUntilReady(ctx, d.Hostname, 4, 2*time.Second)
		}
		switch {
		case probe.Banned || probe.Tunnel:
			if pubErr != nil {
				status = customdomain.StatusError
				errText = pubErr.Error()
				break
			}
			if published == "provisioned" {
				status = customdomain.StatusVerifying
				errText = "Cloudflare is authorizing this hostname. Keep the Proxied CNAME to " + s.cfg.DomainCNAME() + ". This usually takes about a minute."
				break
			}
			status = customdomain.StatusError
			errText = probe.Detail
		case pubErr != nil:
			status = customdomain.StatusError
			errText = pubErr.Error()
		default:
			status = customdomain.StatusActive
			errText = ""
			verified = true
		}
	} else if result.Error != "" && (result.TXT || result.Route) {
		status = customdomain.StatusError
	}
	if err := s.store.SetCustomDomainCheck(ctx, d.ID, status, errText, edgeStatus, verified); err != nil {
		return d, err
	}
	updated, err := s.store.GetCustomDomainByHost(ctx, d.Hostname)
	if err != nil {
		return d, err
	}
	return updated, nil
}

func (s *Server) publishDomain(ctx context.Context, d store.CustomDomain) (string, error) {
	if s.tunnel == nil || !s.tunnel.Enabled() {
		return "manual", fmt.Errorf("Runex cannot authorize this hostname on the platform zone yet. Keep the Proxied CNAME to %s.", s.cfg.DomainCNAME())
	}
	if err := s.tunnel.EnsureHostname(ctx, d.Hostname); err != nil {
		return "error", err
	}
	return "provisioned", nil
}

func (s *Server) unpublishDomain(ctx context.Context, d store.CustomDomain) {
	if s.tunnel == nil || !s.tunnel.Enabled() {
		return
	}
	_ = s.tunnel.RemoveHostname(ctx, d.Hostname)
}

func (s *Server) unpublishProjectDomains(ctx context.Context, projectID string) {
	items, err := s.store.ListCustomDomainsByProject(ctx, projectID)
	if err != nil {
		return
	}
	for _, item := range items {
		s.unpublishDomain(ctx, item)
	}
}

func (s *Server) initialEdgeStatus() string {
	if s.tunnel != nil && s.tunnel.Enabled() {
		return "pending"
	}
	return "manual"
}

func (s *Server) domainResolver() customdomain.Resolver {
	if s.resolver != nil {
		return s.resolver
	}
	return customdomain.DefaultResolver()
}

func (s *Server) WatchDomains(ctx context.Context) {
	if s == nil || s.store == nil {
		return
	}
	s.reconcileDomains(ctx)
	ticker := time.NewTicker(45 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reconcileDomains(context.Background())
		}
	}
}

func (s *Server) reconcileDomains(ctx context.Context) {
	if s == nil || s.store == nil {
		return
	}
	items, err := s.store.ListPendingCustomDomains(ctx, 40)
	if err != nil {
		return
	}
	for _, item := range items {
		if item.LastCheckAt != nil && time.Since(*item.LastCheckAt) < 20*time.Second {
			continue
		}
		checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, _ = s.checkCustomDomain(checkCtx, item)
		cancel()
	}
}

func (s *Server) lookupCustomDomain(ctx context.Context, host string) (store.Node, error) {
	if s.store == nil {
		return store.Node{}, customdomain.ErrInvalid
	}
	hostname, err := customdomain.Normalize(host)
	if err != nil {
		return store.Node{}, err
	}
	if customdomain.PlatformHost(s.cfg, hostname) {
		return store.Node{}, customdomain.ErrPlatform
	}
	domain, err := s.store.GetActiveDomainByHost(ctx, hostname)
	if err != nil {
		return store.Node{}, err
	}
	return s.store.GetNode(ctx, domain.ProjectID, domain.NodeID)
}


func (s *Server) enablePlatformURL(w http.ResponseWriter, r *http.Request) {
	if s.domainLimit != nil && !s.domainLimit.allow("platform:"+userFrom(r).ID) {
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "Too many URL changes. Try again shortly.")
		return
	}
	project, node, err := s.loadServiceNode(w, r)
	if err != nil {
		return
	}
	if err := s.store.SetPlatformURL(r.Context(), node.ID, true, true); err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not restore the Runex URL")
		return
	}
	fresh, err := s.store.GetNode(r.Context(), project.ID, node.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not restore the Runex URL")
		return
	}
	url := s.platformNodeURL(fresh, nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"platformUrlEnabled": !fresh.PlatformURLRevoked,
		"platformUrl":        url,
	})
}

func (s *Server) disablePlatformURL(w http.ResponseWriter, r *http.Request) {
	if s.domainLimit != nil && !s.domainLimit.allow("platform:"+userFrom(r).ID) {
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "Too many URL changes. Try again shortly.")
		return
	}
	_, node, err := s.loadServiceNode(w, r)
	if err != nil {
		return
	}
	active, err := s.store.CountActiveCustomDomains(r.Context(), node.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not revoke the Runex URL")
		return
	}
	if active < 1 {
		writeErr(w, http.StatusConflict, "last_url", "Connect a custom domain before revoking the Runex URL.")
		return
	}
	if err := s.store.SetPlatformURL(r.Context(), node.ID, false, true); err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not revoke the Runex URL")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"platformUrlEnabled": false,
		"platformUrl":        "",
	})
}
