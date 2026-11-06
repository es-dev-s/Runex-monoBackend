package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"deploy_platform/internal/alloc"
	"deploy_platform/internal/cache"
	"deploy_platform/internal/cloudflare"
	"deploy_platform/internal/config"
	"deploy_platform/internal/customdomain"
	"deploy_platform/internal/deploy"
	"deploy_platform/internal/edge"
	"deploy_platform/internal/envfile"
	"deploy_platform/internal/githubapp"
	"deploy_platform/internal/ident"
	"deploy_platform/internal/ingest"
	"deploy_platform/internal/logs"
	"deploy_platform/internal/store"
	"deploy_platform/internal/usage"

	"github.com/jackc/pgx/v5"
)

type Server struct {
	cfg     config.Config
	store   *store.Store
	engine  *deploy.Engine
	hub     *logs.Hub
	github  *githubapp.Client
	cache   *cache.Store
	sampler *usage.Sampler

	vaultOnce sync.Once
	vaultKey  []byte
	vaultErr  error

	sessionMu sync.RWMutex
	sessions  map[string]sessionEntry

	liveMu    sync.Mutex
	adminWait map[chan struct{}]struct{}
	kickWait  map[string]map[chan struct{}]struct{}

	tunnel      *cloudflare.Tunnel
	domainLimit *limiter
	resolver    customdomain.Resolver
}

func New(cfg config.Config, st *store.Store, engine *deploy.Engine, hub *logs.Hub, ch *cache.Store, sampler *usage.Sampler) *Server {
	return &Server{
		cfg:         cfg,
		store:       st,
		engine:      engine,
		hub:         hub,
		github:      githubapp.New(cfg),
		cache:       ch,
		sampler:     sampler,
		tunnel:      cloudflare.NewTunnel(cfg),
		domainLimit: newLimiter(12, 10*time.Minute),
		sessions:    map[string]sessionEntry{},
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.health)
	mux.HandleFunc("GET /v1/activity", s.activity)
	mux.HandleFunc("GET /v1/activity/logs", s.activityLogs)
	mux.HandleFunc("GET /v1/usage", s.workspaceUsage)
	mux.HandleFunc("GET /v1/projects", s.listProjects)
	mux.HandleFunc("POST /v1/projects", s.createProject)
	mux.HandleFunc("POST /v1/projects/bulk-delete", s.bulkDeleteProjects)
	mux.HandleFunc("GET /v1/projects/{id}", s.getProject)
	mux.HandleFunc("POST /v1/projects/{id}/environments", s.ensureEnvironment)
	mux.HandleFunc("DELETE /v1/projects/{id}", s.deleteProject)
	mux.HandleFunc("POST /v1/projects/{id}/stop", s.stopProject)
	mux.HandleFunc("POST /v1/projects/{id}/start", s.startProject)
	mux.HandleFunc("GET /v1/projects/{id}/nodes", s.listNodes)
	mux.HandleFunc("GET /v1/projects/{id}/usage", s.projectUsage)
	mux.HandleFunc("POST /v1/projects/{id}/nodes", s.addNode)
	mux.HandleFunc("PATCH /v1/projects/{id}/nodes/{nodeId}", s.moveNode)
	mux.HandleFunc("POST /v1/projects/{id}/nodes/{nodeId}/deploy", s.deployNode)
	mux.HandleFunc("POST /v1/projects/{id}/nodes/{nodeId}/cancel", s.cancelDeploy)
	mux.HandleFunc("DELETE /v1/projects/{id}/nodes/{nodeId}", s.deleteNode)
	mux.HandleFunc("POST /v1/projects/{id}/deploy/github", s.deployGitHub)
	mux.HandleFunc("POST /v1/projects/{id}/deploy/zip", s.deployZip)
	mux.HandleFunc("POST /v1/projects/{id}/deploy/html", s.deployHTML)
	mux.HandleFunc("GET /v1/projects/{id}/logs", s.listLogs)
	mux.HandleFunc("GET /v1/projects/{id}/events", s.events)
	mux.HandleFunc("GET /v1/projects/{id}/variables", s.listVars)
	mux.HandleFunc("PUT /v1/projects/{id}/variables", s.replaceVars)
	mux.HandleFunc("POST /v1/projects/{id}/variables", s.upsertVar)
	mux.HandleFunc("POST /v1/projects/{id}/variables/import", s.importVars)
	mux.HandleFunc("DELETE /v1/projects/{id}/variables/{varId}", s.deleteVar)
	mux.HandleFunc("POST /v1/projects/{id}/variables/apply", s.applyVars)
	mux.HandleFunc("GET /v1/projects/{id}/nodes/{nodeId}/database", s.databaseOverview)
	mux.HandleFunc("GET /v1/projects/{id}/nodes/{nodeId}/database/tables", s.databaseTables)
	mux.HandleFunc("GET /v1/projects/{id}/nodes/{nodeId}/database/rows", s.databaseRows)
	mux.HandleFunc("GET /v1/projects/{id}/nodes/{nodeId}/database/keys", s.databaseKeys)
	mux.HandleFunc("GET /v1/projects/{id}/nodes/{nodeId}/database/key", s.databaseKey)
	mux.HandleFunc("GET /v1/github/status", s.githubStatus)
	mux.HandleFunc("GET /v1/github/install", s.githubInstall)
	mux.HandleFunc("GET /v1/github/setup", s.githubSetup)
	mux.HandleFunc("GET /v1/github/repos", s.githubRepos)
	mux.HandleFunc("GET /v1/github/repos/{owner}/{repo}/branches", s.githubBranches)
	mux.HandleFunc("POST /v1/github/webhook", s.githubWebhook)
	mux.HandleFunc("POST /v1/github/complete", s.githubComplete)
	mux.HandleFunc("POST /v1/auth/register", s.register)
	mux.HandleFunc("POST /v1/auth/login", s.login)
	mux.HandleFunc("GET /v1/auth/google", s.googleLogin)
	mux.HandleFunc("GET /v1/auth/google/callback", s.googleCallback)
	mux.HandleFunc("POST /v1/auth/logout", s.logout)
	mux.HandleFunc("GET /v1/auth/me", s.me)
	mux.HandleFunc("PATCH /v1/auth/me/preferences", s.updatePreferences)
	mux.HandleFunc("POST /v1/auth/me/onboarding", s.completeOnboarding)
	mux.HandleFunc("GET /v1/auth/watch", s.watchSession)
	mux.HandleFunc("GET /v1/admin/users", s.adminListUsers)
	mux.HandleFunc("GET /v1/admin/live", s.adminLive)
	mux.HandleFunc("POST /v1/admin/users/{id}/revoke", s.adminRevokeUser)
	mux.HandleFunc("POST /v1/admin/users/{id}/restore", s.adminRestoreUser)
	mux.HandleFunc("GET /v1/secrets", s.secretsBundle)
	mux.HandleFunc("PUT /v1/secrets/vault", s.putSecretVault)
	mux.HandleFunc("POST /v1/secrets/vault/attach", s.attachSecretVault)
	mux.HandleFunc("POST /v1/secrets/groups", s.createSecretGroup)
	mux.HandleFunc("PATCH /v1/secrets/groups/{id}", s.patchSecretGroup)
	mux.HandleFunc("DELETE /v1/secrets/groups/{id}", s.deleteSecretGroup)
	mux.HandleFunc("POST /v1/secrets/items", s.createSecretItems)
	mux.HandleFunc("PATCH /v1/secrets/items/{id}", s.patchSecretItem)
	mux.HandleFunc("DELETE /v1/secrets/items/{id}", s.deleteSecretItem)
	mux.HandleFunc("GET /v1/projects/{id}/nodes/{nodeId}/domains", s.listDomains)
	mux.HandleFunc("POST /v1/projects/{id}/nodes/{nodeId}/domains", s.addDomain)
	mux.HandleFunc("POST /v1/projects/{id}/nodes/{nodeId}/domains/{domainId}/verify", s.verifyDomain)
	mux.HandleFunc("DELETE /v1/projects/{id}/nodes/{nodeId}/domains/{domainId}", s.deleteDomain)
	mux.HandleFunc("POST /v1/projects/{id}/nodes/{nodeId}/platform-url", s.enablePlatformURL)
	mux.HandleFunc("DELETE /v1/projects/{id}/nodes/{nodeId}/platform-url", s.disablePlatformURL)
	mux.HandleFunc("PUT /v1/projects/{id}/nodes/{nodeId}/platform-subdomain", s.setPlatformSubdomain)
	// CORS must wrap auth. A 401 from withAuth used to skip withCommon, so the
	// browser hid the body as a network failure and the dashboard showed
	// "can't reach the control plane" instead of the sign-in page.
	return s.withEdge(s.withRateLimit(s.withCommon(s.withAuth(mux))))
}

func (s *Server) withCommon(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		// HSTS is also set at Cloudflare; send it from origin so direct hits are covered.
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		origin := r.Header.Get("Origin")
		if s.originAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept, Cache-Control")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Max-Age", "600")
			w.Header().Add("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	// Public probe: no infra fingerprinting on success or failure.
	if err := s.store.Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "degraded"})
		return
	}
	if s.cache != nil {
		if err := s.cache.Ping(r.Context()); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "degraded"})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	items, err := s.store.ListProjects(r.Context(), user.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not list projects")
		return
	}
	nodes, err := s.store.ListNodesForUser(r.Context(), user.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not list project nodes")
		return
	}
	// One URL pass and one open-build query for the whole workspace. Doing
	// either per project made the project list wait on N round trips.
	nodes = s.exposeNodeURLs(r.Context(), nodes)
	if starts, err := s.store.OpenBuildStartsForUser(r.Context(), user.ID); err == nil {
		nodes = applyBuildClocks(nodes, starts)
	}
	byProject := map[string][]store.Node{}
	for _, node := range nodes {
		byProject[node.ProjectID] = append(byProject[node.ProjectID], node)
	}
	type item struct {
		store.Project
		Nodes []store.Node `json:"nodes"`
	}
	out := make([]item, 0, len(items))
	for _, project := range items {
		ns := byProject[project.ID]
		if ns == nil {
			ns = []store.Node{}
		}
		out = append(out, item{Project: s.exposeProjectURL(project, ns), Nodes: ns})
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": out})
}

type createBody struct {
	Name     string `json:"name"`
	Subtitle string `json:"subtitle"`
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var body createBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request", "Name is required")
		return
	}
	project := store.Project{
		ID:          ident.New("prj_"),
		Name:        strings.TrimSpace(body.Name),
		Subtitle:    strings.TrimSpace(body.Subtitle),
		Status:      "idle",
		UserID:      userFrom(r).ID,
		Environment: store.EnvProduction,
	}
	project.FamilyID = project.ID
	if project.Subtitle == "" {
		project.Subtitle = "Untitled workspace"
	}
	now := time.Now().UTC()
	project.CreatedAt = now
	project.UpdatedAt = now
	if err := s.store.CreateProject(r.Context(), project); err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not create project")
		return
	}
	node := store.Node{
		ID:        ident.New("svc_"),
		ProjectID: project.ID,
		Kind:      "service",
		Title:     project.Name,
		Caption:   "Service",
		Status:    "idle",
	}
	if err := s.store.UpsertNode(r.Context(), node); err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not create project")
		return
	}
	// Return the row we just wrote. The canvas refreshes the full bundle in
	// the background, so create does not wait on URL stamping or build clocks.
	writeJSON(w, http.StatusCreated, map[string]any{
		"project": project,
		"nodes":   []store.Node{node},
	})
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	project, err := s.loadProject(w, r)
	if err != nil {
		return
	}
	s.writeProjectBundleCtx(r.Context(), w, http.StatusOK, project)
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	project, err := s.store.GetOwnedProject(r.Context(), userFrom(r).ID, r.PathValue("id"))
	if errors.Is(err, pgx.ErrNoRows) {
		// Same status as loadProject: do not claim success for foreign/unknown IDs.
		writeErr(w, http.StatusNotFound, "not_found", "Project not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not load project")
		return
	}
	if err := s.purgeFamilyOrProject(r.Context(), userFrom(r).ID, project); err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not delete project")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) bulkDeleteProjects(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "JSON body with ids is required")
		return
	}
	ids := uniqueProjectIDs(body.IDs, 100)
	if len(ids) == 0 {
		writeErr(w, http.StatusBadRequest, "invalid_request", "Select at least one project")
		return
	}
	deleted := make([]string, 0, len(ids))
	type failedItem struct {
		ID    string `json:"id"`
		Error string `json:"error"`
	}
	failed := make([]failedItem, 0)
	for _, id := range ids {
		project, err := s.store.GetOwnedProject(r.Context(), userFrom(r).ID, id)
		if errors.Is(err, pgx.ErrNoRows) {
			// Not owned / does not exist — never report as deleted.
			failed = append(failed, failedItem{ID: id, Error: "Project not found"})
			continue
		}
		if err != nil {
			failed = append(failed, failedItem{ID: id, Error: "Could not load project"})
			continue
		}
		if err := s.purgeFamilyOrProject(r.Context(), userFrom(r).ID, project); err != nil {
			failed = append(failed, failedItem{ID: id, Error: "Could not delete project"})
			continue
		}
		deleted = append(deleted, id)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted": deleted,
		"failed":  failed,
	})
}

func (s *Server) purgeProject(ctx context.Context, id string) error {
	nodes, err := s.store.ListNodes(ctx, id)
	if err != nil {
		return err
	}
	for _, node := range nodes {
		if node.Kind == "service" {
			s.engine.Jobs.Cancel(node.ID)
		}
	}
	s.unpublishProjectDomains(ctx, id)
	s.engine.DestroyProject(ctx, id)
	return s.store.DeleteProject(ctx, id)
}

func uniqueProjectIDs(ids []string, max int) []string {
	if max <= 0 {
		max = 100
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
		if len(out) >= max {
			break
		}
	}
	return out
}

func (s *Server) stopProject(w http.ResponseWriter, r *http.Request) {
	project, err := s.loadProject(w, r)
	if err != nil {
		return
	}
	nodeID := strings.TrimSpace(r.URL.Query().Get("nodeId"))
	if nodeID != "" {
		node, err := s.store.GetNode(r.Context(), project.ID, nodeID)
		if err != nil {
			writeErr(w, http.StatusNotFound, "not_found", "Service not found")
			return
		}
		if node.Status == "building" {
			writeErr(w, http.StatusConflict, "deploy_in_progress", "Wait for this service to finish deploying")
			return
		}
		if err := s.engine.StopService(r.Context(), project.ID, nodeID); err != nil {
			writeErr(w, http.StatusBadGateway, "runtime", "Could not stop container")
			return
		}
	} else {
		nodes, _ := s.store.ListNodes(r.Context(), project.ID)
		for _, node := range nodes {
			if node.Kind == "postgresql" || node.Kind == "redis" || (node.Kind == "service" && node.Status != "building") {
				_ = s.engine.StopService(r.Context(), project.ID, node.ID)
			}
		}
	}
	updated, _ := s.store.GetProject(r.Context(), project.ID)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) startProject(w http.ResponseWriter, r *http.Request) {
	project, err := s.loadProject(w, r)
	if err != nil {
		return
	}
	if !s.requireCompute(w, r) {
		return
	}
	nodeID := strings.TrimSpace(r.URL.Query().Get("nodeId"))
	if nodeID != "" {
		node, err := s.store.GetNode(r.Context(), project.ID, nodeID)
		if err != nil {
			writeErr(w, http.StatusNotFound, "not_found", "Service not found")
			return
		}
		if node.Status == "building" {
			writeErr(w, http.StatusConflict, "deploy_in_progress", "Wait for this service to finish deploying")
			return
		}
		if err := s.engine.StartService(r.Context(), project.ID, nodeID); err != nil {
			writeErr(w, http.StatusBadGateway, "runtime", "Could not start container")
			return
		}
	} else {
		nodes, _ := s.store.ListNodes(r.Context(), project.ID)
		for _, node := range nodes {
			if node.Kind == "postgresql" || node.Kind == "redis" || (node.Kind == "service" && node.Status != "building") {
				_ = s.engine.StartService(r.Context(), project.ID, node.ID)
			}
		}
	}
	updated, _ := s.store.GetProject(r.Context(), project.ID)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) listNodes(w http.ResponseWriter, r *http.Request) {
	project, err := s.loadProject(w, r)
	if err != nil {
		return
	}
	nodes, err := s.store.ListNodes(r.Context(), project.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not load nodes")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes})
}

type nodeBody struct {
	Kind    string  `json:"kind"`
	Title   string  `json:"title"`
	Caption string  `json:"caption"`
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
}

func (s *Server) addNode(w http.ResponseWriter, r *http.Request) {
	project, err := s.loadProject(w, r)
	if err != nil {
		return
	}
	var body nodeBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "JSON body is required")
		return
	}
	kind := strings.ToLower(strings.TrimSpace(body.Kind))
	switch kind {
	case "service", "railway", "postgresql", "redis", "container":
	default:
		writeErr(w, http.StatusBadRequest, "invalid_request", "Unknown node kind")
		return
	}
	prefix := "nod_"
	if kind == "service" {
		prefix = "svc_"
	}
	node := store.Node{
		ID:        ident.New(prefix),
		ProjectID: project.ID,
		Kind:      kind,
		Title:     strings.TrimSpace(body.Title),
		Caption:   strings.TrimSpace(body.Caption),
		X:         body.X,
		Y:         body.Y,
		Status:    "idle",
		MemoryMB:  alloc.DefaultMemoryMB(kind),
		DiskMB:    alloc.DefaultDiskMB(kind),
	}
	if node.Title == "" {
		if kind == "service" {
			node.Title = "Service"
		} else {
			node.Title = strings.ToUpper(kind[:1]) + kind[1:]
		}
	}
	if kind == "service" {
		node.Caption = "Service"
		if node.X == 0 && node.Y == 0 {
			existing, _ := s.store.ListNodes(r.Context(), project.ID)
			node.X, node.Y = nextServicePoint(existing)
		}
	}
	if (kind == "postgresql" || kind == "redis") && !s.requireCompute(w, r) {
		return
	}
	if kind == "postgresql" || kind == "redis" {
		node.Status = "building"
		node.Caption = "Starting"
		if err := s.store.UpsertNode(r.Context(), node); err != nil {
			writeErr(w, http.StatusInternalServerError, "db", "Could not save node")
			return
		}
		started, err := s.engine.AttachSidecar(node)
		if err != nil {
			msg := "Could not prepare the database"
			node.Status = "failed"
			node.Caption = "Failed"
			node.Error = &msg
			_ = s.store.UpsertNode(r.Context(), node)
			writeErr(w, http.StatusBadGateway, "runtime", "Could not start isolated "+kind+" container")
			return
		}
		writeJSON(w, http.StatusCreated, started)
		return
	}
	if err := s.store.UpsertNode(r.Context(), node); err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not save node")
		return
	}
	writeJSON(w, http.StatusCreated, node)
}

type moveBody struct {
	X            *float64 `json:"x"`
	Y            *float64 `json:"y"`
	AutoDeploy   *bool    `json:"autoDeploy"`
	GitHubBranch *string  `json:"githubBranch"`
	MemoryMB     *int     `json:"memoryMb"`
	DiskMB       *int     `json:"diskMb"`
}

func (s *Server) moveNode(w http.ResponseWriter, r *http.Request) {
	project, err := s.loadProject(w, r)
	if err != nil {
		return
	}
	node, err := s.store.GetNode(r.Context(), project.ID, r.PathValue("nodeId"))
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "Node not found")
		return
	}
	var body moveBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "JSON body is required")
		return
	}
	if body.X != nil {
		node.X = *body.X
	}
	if body.Y != nil {
		node.Y = *body.Y
	}
	if body.X != nil || body.Y != nil {
		if err := s.store.MoveNode(r.Context(), project.ID, node.ID, node.X, node.Y); err != nil {
			writeErr(w, http.StatusInternalServerError, "db", "Could not move node")
			return
		}
	}
	if body.AutoDeploy != nil {
		node.AutoDeploy = *body.AutoDeploy
		if err := s.store.SetAutoDeploy(r.Context(), project.ID, node.ID, *body.AutoDeploy); err != nil {
			writeErr(w, http.StatusInternalServerError, "db", "Could not update auto-deploy")
			return
		}
	}
	if body.GitHubBranch != nil {
		branch := strings.TrimSpace(*body.GitHubBranch)
		node.GitHubBranch = strPtr(branch)
		if err := s.store.SetGitHubBranch(r.Context(), project.ID, node.ID, branch); err != nil {
			writeErr(w, http.StatusInternalServerError, "db", "Could not update branch")
			return
		}
	}
	updated, ok := s.applyNodeAlloc(w, r, node, body.MemoryMB, body.DiskMB)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) deleteNode(w http.ResponseWriter, r *http.Request) {
	project, err := s.store.GetOwnedProject(r.Context(), userFrom(r).ID, r.PathValue("id"))
	if errors.Is(err, pgx.ErrNoRows) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not load project")
		return
	}
	node, err := s.store.GetNode(r.Context(), project.ID, r.PathValue("nodeId"))
	if errors.Is(err, pgx.ErrNoRows) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not load node")
		return
	}
	s.teardownNode(r.Context(), userFrom(r).ID, project.ID, node)
	nodes, err := s.store.ListNodes(r.Context(), project.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not delete node")
		return
	}
	if remainingNodes(nodes, node.ID) == 0 {
		s.engine.DestroyProject(r.Context(), project.ID)
		if err := s.store.DeleteProject(r.Context(), project.ID); err != nil {
			writeErr(w, http.StatusInternalServerError, "db", "Could not remove this project")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"deleted":        true,
			"projectDeleted": true,
			"projectId":      project.ID,
		})
		return
	}
	if err := s.store.DeleteNode(r.Context(), project.ID, node.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not delete node")
		return
	}
	_ = s.store.RollupProject(r.Context(), project.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) teardownNode(ctx context.Context, userID, projectID string, node store.Node) {
	_ = s.store.ReplaceVars(ctx, projectID, node.ID, nil)
	switch node.Kind {
	case "postgresql", "redis":
		s.engine.StopSidecar(ctx, projectID, node.ID, node.Kind, node.ContainerName)
	default:
		s.engine.Jobs.Cancel(node.ID)
		if domains, err := s.store.ListCustomDomains(ctx, userID, projectID, node.ID); err == nil {
			for _, item := range domains {
				s.unpublishDomain(ctx, item)
			}
		}
		s.engine.RemoveService(ctx, projectID, node.ID)
	}
}

func (s *Server) deployGitHub(w http.ResponseWriter, r *http.Request) {
	project, err := s.loadProject(w, r)
	if err != nil {
		return
	}
	if !s.requireCompute(w, r) {
		return
	}
	var body githubBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "JSON body with url or repo is required")
		return
	}
	ref, spec, err := s.parseGitHubTarget(body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_url", "Use owner/repo or a github.com URL")
		return
	}
	if spec.InstallationID != 0 {
		ok, ownErr := s.store.InstallationOwned(r.Context(), userFrom(r).ID, spec.InstallationID)
		if ownErr != nil || !ok {
			writeErr(w, http.StatusNotFound, "not_found", "GitHub installation not found")
			return
		}
	} else if s.github.Enabled() {
		installs, listErr := s.store.ListInstallations(r.Context(), userFrom(r).ID)
		if listErr == nil && len(installs) > 0 {
			spec.InstallationID = installs[0].ID
		}
	}
	// Branch lookup and the clone run detached from this request. A refresh
	// aborts the browser fetch, and that must not cancel the deploy.
	listCtx, listCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer listCancel()
	if spec.Branch == "" {
		def, _, listErr := s.github.ListBranches(listCtx, spec.InstallationID, ref.Owner, ref.Name, "")
		if listErr == nil {
			spec.Branch = def
		}
	}
	title := ref.Name
	node, err := s.resolveService(context.Background(), project, firstNonEmpty(body.NodeID, r.URL.Query().Get("nodeId")), title)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "Could not create service")
		return
	}
	source := ref.Owner + "/" + ref.Name
	if node.Status == "building" || s.engine.Jobs.Busy(node.ID) {
		writeJSON(w, http.StatusAccepted, map[string]any{
			"project": project,
			"node":    node,
			"status":  "building",
		})
		return
	}
	node.Title = title
	node.SourceType = strPtr("github")
	node.SourceRef = strPtr(source)
	node.Status = "building"
	node.Error = nil
	node.AutoDeploy = spec.AutoDeploy
	if spec.Branch != "" {
		node.GitHubBranch = strPtr(spec.Branch)
	}
	if spec.InstallationID != 0 {
		id := spec.InstallationID
		node.GitHubInstallationID = &id
	}
	runCtx, waitPrev, finish := s.engine.Jobs.Takeover(node.ID)
	s.beginDeploy(project, node, "github", source, "github", runCtx, waitPrev, finish, func(ctx context.Context, depID string) (string, error) {
		s.emitDeployLog(project.ID, node.ID, depID, "build", "info", "Fetching a fresh copy from GitHub — "+source+" @ "+firstNonEmpty(spec.Branch, "default"))
		return s.cloneGitHubSource(ctx, &node, ref, spec)
	})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"project": project,
		"node":    node,
		"status":  "building",
	})
}

func (s *Server) deployZip(w http.ResponseWriter, r *http.Request) {
	project, err := s.loadProject(w, r)
	if err != nil {
		return
	}
	if !s.requireCompute(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes)
	if err := r.ParseMultipartForm(s.cfg.MaxUploadBytes); err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "too_large", "Zip exceeds upload limit")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_zip", "Multipart field file is required")
		return
	}
	defer file.Close()
	if !strings.HasSuffix(strings.ToLower(header.Filename), ".zip") {
		writeErr(w, http.StatusBadRequest, "invalid_zip", "File must be a .zip")
		return
	}
	tmp, err := os.CreateTemp(s.cfg.WorkDir, "upload-*.zip")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "workspace", "Could not store upload")
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := io.Copy(tmp, file); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_zip", "Could not read zip upload")
		return
	}
	info, _ := tmp.Stat()
	if _, err := tmp.Seek(0, 0); err != nil {
		writeErr(w, http.StatusInternalServerError, "workspace", "Could not rewind upload")
		return
	}
	title := strings.TrimSuffix(header.Filename, filepath.Ext(header.Filename))
	node, err := s.resolveService(r.Context(), project, firstNonEmpty(r.FormValue("nodeId"), r.URL.Query().Get("nodeId")), title)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "Could not create service")
		return
	}
	s.occupyService(r.Context(), project.ID, &node, "zip", header.Filename)
	dir, err := deploy.PrepareDir(s.cfg, project.ID, node.ID)
	if err != nil {
		s.releaseService(project.ID, &node)
		writeErr(w, http.StatusInternalServerError, "workspace", "Could not create workspace")
		return
	}
	if err := ingest.ExtractZip(tmp, info.Size(), dir, s.cfg.MaxZipFiles, s.cfg.MaxUncompressed); err != nil {
		s.releaseService(project.ID, &node)
		writeErr(w, http.StatusBadRequest, "invalid_zip", "Could not extract zip")
		return
	}
	s.finishStaging(w, r, project, node, "zip", header.Filename, dir)
}

func (s *Server) deployHTML(w http.ResponseWriter, r *http.Request) {
	project, err := s.loadProject(w, r)
	if err != nil {
		return
	}
	if !s.requireCompute(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes)
	if err := r.ParseMultipartForm(s.cfg.MaxUploadBytes); err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "too_large", "File exceeds upload limit")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_html", "Multipart field file is required")
		return
	}
	defer file.Close()
	name := strings.ToLower(header.Filename)
	if !strings.HasSuffix(name, ".html") && !strings.HasSuffix(name, ".htm") {
		writeErr(w, http.StatusBadRequest, "invalid_html", "File must be .html")
		return
	}
	body, err := io.ReadAll(io.LimitReader(file, s.cfg.MaxUploadBytes))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_html", "Could not read HTML file")
		return
	}
	title := strings.TrimSuffix(header.Filename, filepath.Ext(header.Filename))
	node, err := s.resolveService(r.Context(), project, firstNonEmpty(r.FormValue("nodeId"), r.URL.Query().Get("nodeId")), title)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "Could not create service")
		return
	}
	s.occupyService(r.Context(), project.ID, &node, "html", header.Filename)
	dir, err := deploy.PrepareDir(s.cfg, project.ID, node.ID)
	if err != nil {
		s.releaseService(project.ID, &node)
		writeErr(w, http.StatusInternalServerError, "workspace", "Could not create workspace")
		return
	}
	if err := ingest.WriteHTML(dir, header.Filename, string(body)); err != nil {
		s.releaseService(project.ID, &node)
		writeErr(w, http.StatusInternalServerError, "workspace", "Could not store HTML")
		return
	}
	s.finishStaging(w, r, project, node, "html", header.Filename, dir)
}

func (s *Server) finishStaging(w http.ResponseWriter, r *http.Request, project store.Project, node store.Node, sourceType, sourceRef, dir string) {
	// Detect keeps running after the browser leaves. The request context is
	// cancelled by a refresh, which used to throw away the deployment.
	stageCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := s.engine.Stage(stageCtx, deploy.Request{
		ProjectID:  project.ID,
		ServiceID:  node.ID,
		SourceType: sourceType,
		SourceRef:  sourceRef,
		Dir:        dir,
	}); err != nil {
		msg := err.Error()
		node.Status = "failed"
		node.Error = &msg
		_ = s.store.UpsertNode(context.Background(), node)
		_ = s.store.RollupProject(context.Background(), project.ID)
		writeErr(w, http.StatusBadGateway, "stage_failed", "Could not inspect this codebase")
		return
	}
	updated, _ := s.store.GetProject(context.Background(), project.ID)
	staged, err := s.store.GetNode(context.Background(), project.ID, node.ID)
	if err != nil {
		staged = node
		staged.Status = "ready"
		staged.SourceType = strPtr(sourceType)
		staged.SourceRef = strPtr(sourceRef)
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"project": updated,
		"node":    staged,
		"status":  "ready",
	})
}

func (s *Server) deployNode(w http.ResponseWriter, r *http.Request) {
	project, err := s.loadProject(w, r)
	if err != nil {
		return
	}
	if !s.requireCompute(w, r) {
		return
	}
	node, err := s.store.GetNode(r.Context(), project.ID, r.PathValue("nodeId"))
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "Service not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not load service")
		return
	}
	if node.Kind != "service" {
		writeErr(w, http.StatusBadRequest, "invalid_request", "Deploy from a service node")
		return
	}
	sourceType := deref(node.SourceType)
	if sourceType == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request", "Add a codebase before deploying")
		return
	}
	if node.Status == "building" {
		writeErr(w, http.StatusConflict, "deploy_in_progress", "This service is already deploying")
		return
	}
	dir := deploy.SourceDir(s.cfg, project.ID, node.ID)
	trigger := "manual"
	var body struct {
		Trigger string `json:"trigger"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err == nil {
		switch strings.TrimSpace(body.Trigger) {
		case "settings", "github":
			trigger = strings.TrimSpace(body.Trigger)
		}
	}
	var ref ingest.GitRef
	branch := ""
	if sourceType == "github" {
		parsed, parseErr := ingest.ParseGitHub(deref(node.SourceRef))
		if parseErr != nil {
			writeErr(w, http.StatusBadRequest, "invalid_url", "Saved GitHub source is invalid")
			return
		}
		ref = parsed
		branch = deref(node.GitHubBranch)
	} else if _, err := os.Stat(dir); err != nil {
		writeErr(w, http.StatusBadRequest, "missing_source", "Re-add this codebase before deploying")
		return
	}
	runCtx, waitPrev, finish := s.engine.Jobs.Takeover(node.ID)
	s.beginDeploy(project, node, sourceType, deref(node.SourceRef), trigger, runCtx, waitPrev, finish, func(ctx context.Context, depID string) (string, error) {
		if sourceType != "github" {
			return dir, nil
		}
		s.emitDeployLog(project.ID, node.ID, depID, "build", "info", "Fetching a fresh copy from GitHub — "+ref.Owner+"/"+ref.Name+" @ "+firstNonEmpty(branch, "default"))
		return s.cloneGitHubSource(ctx, &node, ref, githubClone{
			Branch:         branch,
			InstallationID: derefInt64(node.GitHubInstallationID),
			AutoDeploy:     node.AutoDeploy,
		})
	})
	updated, _ := s.store.GetProject(context.Background(), project.ID)
	node.Status = "building"
	writeJSON(w, http.StatusAccepted, map[string]any{
		"project": updated,
		"node":    node,
		"status":  "building",
	})
}

func (s *Server) cancelDeploy(w http.ResponseWriter, r *http.Request) {
	project, err := s.loadProject(w, r)
	if err != nil {
		return
	}
	node, err := s.store.GetNode(r.Context(), project.ID, r.PathValue("nodeId"))
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "Service not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not load service")
		return
	}
	if node.Kind != "service" {
		writeErr(w, http.StatusBadRequest, "invalid_request", "Cancel from a service node")
		return
	}
	if node.Status == "building" || s.engine.Jobs.Busy(node.ID) {
		s.engine.CancelDeploy(node)
		if fresh, err := s.store.GetNode(r.Context(), project.ID, node.ID); err == nil {
			node = fresh
		}
	}
	updated, _ := s.store.GetProject(r.Context(), project.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"project": updated,
		"node":    node,
		"status":  node.Status,
	})
}

func (s *Server) beginDeploy(project store.Project, node store.Node, sourceType, sourceRef, trigger string, runCtx context.Context, waitPrev func(), finish func(), prepare func(context.Context, string) (string, error)) {
	started := time.Now()
	depID := ident.New("dep_")
	node.Status = "building"
	node.SourceType = strPtr(sourceType)
	node.SourceRef = strPtr(sourceRef)
	node.Error = nil
	_ = s.store.UpsertNode(context.Background(), node)
	_ = s.store.RollupProject(context.Background(), project.ID)
	_ = s.store.CreateDeployment(context.Background(), store.Deployment{
		ID:         depID,
		ProjectID:  project.ID,
		NodeID:     node.ID,
		Status:     "running",
		Phase:      "build",
		Trigger:    firstNonEmpty(trigger, "manual"),
		SourceType: sourceType,
		SourceRef:  strPtr(sourceRef),
	})
	s.publishBuilding(project.ID, node, "build", started)
	s.emitDeployLog(project.ID, node.ID, depID, "build", "info", "Deploy started — "+firstNonEmpty(sourceRef, sourceType))
	go func() {
		abandoned := true
		defer func() {
			finish()
			if abandoned {
				s.stopDeploy(project, node, depID, "Deploy cancelled")
			}
		}()
		if waitPrev != nil {
			waitPrev()
		}
		if runCtx.Err() != nil {
			return
		}
		dir := ""
		if prepare != nil {
			prepared, err := prepare(runCtx, depID)
			if runCtx.Err() != nil {
				return
			}
			if err != nil {
				s.failKick(project, node, depID, err)
				abandoned = false
				return
			}
			dir = prepared
		}
		if runCtx.Err() != nil {
			return
		}
		if strings.TrimSpace(dir) == "" {
			s.failKick(project, node, depID, fmt.Errorf("missing source"))
			abandoned = false
			return
		}
		ctx, cancel := context.WithTimeout(runCtx, s.cfg.BuildTimeout+s.cfg.StartTimeout+time.Minute)
		defer cancel()
		_ = s.engine.Run(ctx, deploy.Request{
			ProjectID:    project.ID,
			ServiceID:    node.ID,
			SourceType:   sourceType,
			SourceRef:    sourceRef,
			Dir:          dir,
			Trigger:      firstNonEmpty(trigger, "manual"),
			DeploymentID: depID,
			StartedAt:    started,
		})
		if runCtx.Err() != nil {
			return
		}
		abandoned = false
	}()
}

func (s *Server) failKick(project store.Project, node store.Node, depID string, err error) {
	s.recordDeployFailure(project, node, depID, cloneMessage(err), true)
}

// stopDeploy records a cancelled deploy after its job slot is gone. A newer
// job for the same service is left alone.
func (s *Server) stopDeploy(project store.Project, node store.Node, depID, msg string) {
	s.recordDeployFailure(project, node, depID, msg, false)
}

func (s *Server) recordDeployFailure(project store.Project, node store.Node, depID, msg string, force bool) {
	if fresh, err := s.store.GetNode(context.Background(), project.ID, node.ID); err == nil {
		node = fresh
	}
	if node.Status != "building" {
		return
	}
	if !force && s.engine.Jobs != nil && s.engine.Jobs.Busy(node.ID) {
		return
	}
	node.Status = "failed"
	node.Error = &msg
	_ = s.store.UpsertNode(context.Background(), node)
	_ = s.store.RollupProject(context.Background(), project.ID)
	s.emitDeployLog(project.ID, node.ID, depID, "build", "error", msg)
	s.hub.Publish(project.ID, logs.Line{
		Kind:   "status",
		Status: "failed",
		Phase:  "build",
		Text:   msg,
		NodeID: node.ID,
	})
	_ = s.store.FinishOpenDeployments(context.Background(), project.ID, node.ID, "failed", msg)
}

func (s *Server) emitDeployLog(projectID, nodeID, depID, phase, level, text string) {
	text = strings.TrimSpace(text)
	if text == "" || depID == "" {
		return
	}
	now := time.Now().UTC()
	_ = s.store.AppendLog(context.Background(), store.LogLine{
		ProjectID:    projectID,
		DeploymentID: depID,
		NodeID:       nodeID,
		TS:           now,
		Phase:        phase,
		Level:        level,
		Text:         text,
	})
	s.hub.Publish(projectID, logs.Line{
		TS:     now,
		Kind:   "log",
		Phase:  phase,
		Level:  level,
		Text:   text,
		NodeID: nodeID,
	})
}

func (s *Server) resolveService(ctx context.Context, project store.Project, nodeID, title string) (store.Node, error) {
	if strings.TrimSpace(nodeID) != "" {
		node, err := s.store.GetNode(ctx, project.ID, nodeID)
		if err != nil {
			return store.Node{}, err
		}
		if node.Kind != "service" {
			return store.Node{}, fmt.Errorf("not a service")
		}
		return node, nil
	}
	nodes, err := s.store.ListNodes(ctx, project.ID)
	if err != nil {
		return store.Node{}, err
	}
	var services []store.Node
	for _, node := range nodes {
		if node.Kind == "service" {
			services = append(services, node)
		}
	}
	if idle, ok := pickIdleService(services, title); ok {
		return idle, nil
	}
	x, y := nextServicePoint(nodes)
	if strings.TrimSpace(title) == "" {
		title = "Service"
	}
	node := store.Node{
		ID:        ident.New("svc_"),
		ProjectID: project.ID,
		Kind:      "service",
		Title:     title,
		Caption:   "Service",
		X:         x,
		Y:         y,
		Status:    "idle",
	}
	if err := s.store.UpsertNode(ctx, node); err != nil {
		return store.Node{}, err
	}
	return node, nil
}

func (s *Server) occupyService(ctx context.Context, projectID string, node *store.Node, sourceType, sourceRef string) {
	node.SourceType = strPtr(sourceType)
	node.SourceRef = strPtr(sourceRef)
	node.Error = nil
	_ = s.store.UpsertNode(ctx, *node)
	_ = s.store.RollupProject(ctx, projectID)
}

func (s *Server) releaseService(projectID string, node *store.Node) {
	node.SourceType = nil
	node.SourceRef = nil
	node.GitHubInstallationID = nil
	node.GitHubBranch = nil
	node.AutoDeploy = false
	node.Status = "idle"
	node.Error = nil
	// Recorded on a detached context so a refresh cannot drop the write.
	_ = s.store.UpsertNode(context.Background(), *node)
	_ = s.store.SetAutoDeploy(context.Background(), projectID, node.ID, false)
	_ = s.store.RollupProject(context.Background(), projectID)
}

func (s *Server) claimService(ctx context.Context, projectID string, node *store.Node) {
	node.Status = "building"
	node.Error = nil
	_ = s.store.UpsertNode(ctx, *node)
	_ = s.store.RollupProject(ctx, projectID)
	s.publishBuilding(projectID, *node, "build", time.Now())
}

func (s *Server) publishBuilding(projectID string, node store.Node, phase string, started time.Time) {
	if started.IsZero() {
		started = time.Now()
	}
	line := logs.Line{
		Kind:      "status",
		Status:    "building",
		Phase:     phase,
		NodeID:    node.ID,
		Framework: deref(node.Framework),
		URL:       s.visitNodeURL(context.Background(), node, s.namedServices(context.Background(), []store.Node{node})),
		StartedAt: &started,
	}
	s.hub.Publish(projectID, line)
}

func serviceOccupied(node store.Node) bool {
	if node.SourceType != nil || node.SourceRef != nil || node.URL != nil || node.ContainerName != nil {
		return true
	}
	switch node.Status {
	case "idle", "":
		return false
	default:
		return true
	}
}

func remainingNodes(nodes []store.Node, deletingID string) int {
	n := 0
	for _, node := range nodes {
		if node.ID != deletingID {
			n++
		}
	}
	return n
}

func (s *Server) hydrateLegacyService(ctx context.Context, project store.Project, nodes []store.Node) {
	for _, node := range nodes {
		if node.Kind != "service" || node.ID != "service:"+project.ID {
			continue
		}
		if len(node.Languages) > 0 || node.URL != nil {
			return
		}
		if project.URL == nil && len(project.Languages) == 0 {
			return
		}
		node.URL = project.URL
		node.Port = project.Port
		node.Framework = project.Framework
		node.SourceType = project.SourceType
		node.SourceRef = project.SourceRef
		node.ContainerName = project.ContainerName
		node.Languages = project.Languages
		node.Stack = project.Stack
		if project.Status != "" {
			node.Status = project.Status
		}
		if project.Framework != nil && strings.TrimSpace(*project.Framework) != "" {
			node.Caption = *project.Framework
		}
		_ = s.store.UpsertNode(ctx, node)
		return
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func strPtr(v string) *string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return &v
}

func deref(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func derefInt64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func (s *Server) listLogs(w http.ResponseWriter, r *http.Request) {
	project, err := s.loadProject(w, r)
	if err != nil {
		return
	}
	q := store.LogQuery{
		ProjectID:    project.ID,
		NodeID:       strings.TrimSpace(r.URL.Query().Get("node")),
		DeploymentID: strings.TrimSpace(r.URL.Query().Get("deployment")),
		Limit:        800,
	}
	lines, err := s.store.QueryLogs(r.Context(), q)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not load logs")
		return
	}
	kept := lines[:0]
	for _, line := range lines {
		if deploy.KeepDeployLog(line.Level, line.Text) {
			kept = append(kept, line)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": kept})
}

func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	projectID := strings.TrimSpace(r.URL.Query().Get("project"))
	user := userFrom(r)
	if projectID != "" {
		if _, err := s.store.GetOwnedProject(r.Context(), user.ID, projectID); err != nil {
			writeErr(w, http.StatusNotFound, "not_found", "Project not found")
			return
		}
	}
	hour := strings.TrimSpace(r.URL.Query().Get("window")) == "hour"
	stats := []store.ActivityStats{}
	if !hour {
		var err error
		stats, err = s.store.ListActivityStats(r.Context(), user.ID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "db", "Could not load activity")
			return
		}
	}
	events, err := s.store.ListActivity(r.Context(), user.ID, projectID, 120, hour)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not load activity")
		return
	}
	if projectID != "" {
		filtered := stats[:0]
		for _, item := range stats {
			if item.ProjectID == projectID {
				filtered = append(filtered, item)
			}
		}
		stats = filtered
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"projects": stats,
		"events":   events,
	})
}

func (s *Server) activityLogs(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("deployment"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request", "Deployment is required")
		return
	}
	user := userFrom(r)
	dep, err := s.store.OwnedDeployment(r.Context(), user.ID, id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "Deployment not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not load logs")
		return
	}
	lines, err := s.store.ListRecentDeploymentLogs(r.Context(), dep.ProjectID, dep.ID, 200)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not load logs")
		return
	}
	kept := lines[:0]
	for _, line := range lines {
		if deploy.KeepDeployLog(line.Level, line.Text) {
			kept = append(kept, line)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": kept})
}

func connectionVarsReady(items []store.EnvVar, kind string) bool {
	want := []string{"DATABASE_URL", "DATABASE_PUBLIC_URL"}
	if kind == "redis" {
		want = []string{"REDIS_URL", "REDIS_PUBLIC_URL"}
	}
	found := map[string]bool{}
	for _, item := range items {
		if strings.TrimSpace(item.Value) != "" {
			found[item.Key] = true
		}
	}
	for _, key := range want {
		if !found[key] {
			return false
		}
	}
	return true
}

func (s *Server) listVars(w http.ResponseWriter, r *http.Request) {
	project, err := s.loadProject(w, r)
	if err != nil {
		return
	}
	nodeID := strings.TrimSpace(r.URL.Query().Get("node"))
	items, err := s.store.ListVars(r.Context(), project.ID, nodeID)
	if err == nil && len(items) == 0 {
		s.syncDetectedVars(r.Context(), project.ID, nodeID)
		items, err = s.store.ListVars(r.Context(), project.ID, nodeID)
	}
	if nodeID != "" {
		if node, nerr := s.store.GetNode(r.Context(), project.ID, nodeID); nerr == nil && (node.Kind == "postgresql" || node.Kind == "redis") {
			if !connectionVarsReady(items, node.Kind) {
				_, _ = s.engine.EnsureSidecar(r.Context(), project.ID, nodeID)
				items, err = s.store.ListVars(r.Context(), project.ID, nodeID)
			}
		}
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not load variables")
		return
	}
	filtered := items[:0]
	for _, item := range items {
		if envfile.IsNoiseKey(item.Key) && item.Source == "detected" {
			_ = s.store.DeleteVar(r.Context(), project.ID, item.ID)
			continue
		}
		filtered = append(filtered, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"variables": filtered})
}

type varBody struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Source string `json:"source"`
}

func (s *Server) upsertVar(w http.ResponseWriter, r *http.Request) {
	project, err := s.loadProject(w, r)
	if err != nil {
		return
	}
	var body varBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "JSON body is required")
		return
	}
	key := strings.TrimSpace(body.Key)
	if !validEnvKey(key) {
		writeErr(w, http.StatusBadRequest, "invalid_key", "Use a valid environment variable name")
		return
	}
	if msg := validateUserEnvValue(key, body.Value); msg != "" {
		writeErr(w, http.StatusBadRequest, "invalid_value", msg)
		return
	}
	item := store.EnvVar{
		ID:     ident.New("var_"),
		NodeID: strings.TrimSpace(r.URL.Query().Get("node")),
		Key:    key,
		Value:  body.Value,
		Source: "user",
	}
	if err := s.store.UpsertVar(r.Context(), project.ID, item); err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not save variable")
		return
	}
	items, _ := s.store.ListVars(r.Context(), project.ID, item.NodeID)
	writeJSON(w, http.StatusOK, map[string]any{"variables": items})
}

func (s *Server) importVars(w http.ResponseWriter, r *http.Request) {
	s.saveVars(w, r, true)
}

func (s *Server) replaceVars(w http.ResponseWriter, r *http.Request) {
	s.saveVars(w, r, false)
}

type replaceVarsBody struct {
	Variables []varBody `json:"variables"`
	Raw       string    `json:"raw"`
	Merge     bool      `json:"merge"`
}

func (s *Server) saveVars(w http.ResponseWriter, r *http.Request, forceMerge bool) {
	project, err := s.loadProject(w, r)
	if err != nil {
		return
	}
	var body replaceVarsBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "JSON body is required")
		return
	}
	if forceMerge {
		body.Merge = true
	}
	nodeID := strings.TrimSpace(r.URL.Query().Get("node"))
	pairs := body.Variables
	if strings.TrimSpace(body.Raw) != "" {
		pairs = nil
		for _, parsed := range envfile.Parse(body.Raw) {
			pairs = append(pairs, varBody{Key: parsed.Key, Value: parsed.Value})
		}
	}
	seen := map[string]struct{}{}
	var items []store.EnvVar
	for _, pair := range pairs {
		key := strings.TrimSpace(pair.Key)
		if !validEnvKey(key) {
			continue
		}
		if msg := validateUserEnvValue(key, pair.Value); msg != "" {
			writeErr(w, http.StatusBadRequest, "invalid_value", msg)
			return
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		items = append(items, store.EnvVar{
			ID:     ident.New("var_"),
			NodeID: nodeID,
			Key:    key,
			Value:  pair.Value,
			Source: "user",
		})
	}
	if len(items) == 0 {
		writeErr(w, http.StatusBadRequest, "invalid_request", "No valid KEY=value lines to add.")
		return
	}

	if body.Merge {
		added, updated, skipped, err := s.store.MergeUserVars(r.Context(), project.ID, nodeID, items)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "db", "Could not save variables")
			return
		}
		if added == 0 && updated == 0 {
			writeErr(w, http.StatusConflict, "managed", "Those keys are managed by Runex and cannot be overwritten.")
			return
		}
		saved, _ := s.store.ListVars(r.Context(), project.ID, nodeID)
		writeJSON(w, http.StatusOK, map[string]any{
			"variables": saved,
			"added":     added,
			"updated":   updated,
			"skipped":   skipped,
		})
		return
	}

	existing, err := s.store.ListVars(r.Context(), project.ID, nodeID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not save variables")
		return
	}
	kept := items
	for _, item := range existing {
		if item.Source != "platform" {
			continue
		}
		if _, clash := seen[item.Key]; clash {
			continue
		}
		kept = append(kept, item)
	}
	if err := s.store.ReplaceVars(r.Context(), project.ID, nodeID, kept); err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not save variables")
		return
	}
	saved, _ := s.store.ListVars(r.Context(), project.ID, nodeID)
	writeJSON(w, http.StatusOK, map[string]any{"variables": saved})
}

func (s *Server) deleteVar(w http.ResponseWriter, r *http.Request) {
	project, err := s.loadProject(w, r)
	if err != nil {
		return
	}
	if err := s.store.DeleteVar(r.Context(), project.ID, r.PathValue("varId")); err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not delete variable")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) applyVars(w http.ResponseWriter, r *http.Request) {
	project, err := s.loadProject(w, r)
	if err != nil {
		return
	}
	if !s.requireCompute(w, r) {
		return
	}
	if err := s.engine.ApplyEnv(r.Context(), project.ID, strings.TrimSpace(r.URL.Query().Get("node"))); err != nil {
		writeErr(w, http.StatusBadGateway, "runtime", "Could not apply variables to the container")
		return
	}
	updated, _ := s.store.GetProject(r.Context(), project.ID)
	writeJSON(w, http.StatusOK, updated)
}

func validEnvKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		ok := r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (i > 0 && r >= '0' && r <= '9')
		if !ok {
			return false
		}
	}
	return true
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	project, err := s.loadProject(w, r)
	if err != nil {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "stream", "Streaming is unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	ch := s.hub.Subscribe(project.ID)
	defer s.hub.Unsubscribe(project.ID, ch)

	// Snapshot live node status only. Log history is loaded over REST — replaying
	// it here made reconnect look like a fresh deploy on whichever service last built.
	nodes, _ := s.store.ListNodes(r.Context(), project.ID)
	nodes = s.exposeNodeURLs(r.Context(), nodes)
	starts, _ := s.store.OpenBuildStarts(r.Context(), project.ID)
	for _, node := range nodes {
		if node.Kind != "service" {
			continue
		}
		event := map[string]any{
			"kind":               "status",
			"status":             node.Status,
			"url":                deref(node.URL),
			"platformUrl":        node.PlatformURL,
			"platformUrlEnabled": node.PlatformURLEnabled,
			"primaryKind":        node.PrimaryKind,
			"framework":          deref(node.Framework),
			"nodeId":             node.ID,
			"languages":          node.Languages,
			"stack":              node.Stack,
		}
		if node.Status == "building" {
			if t, ok := starts[node.ID]; ok {
				event["startedAt"] = t
			}
		}
		payload, _ := json.Marshal(event)
		fmt.Fprintf(w, "data: %s\n\n", payload)
	}
	current, _ := s.store.GetProject(r.Context(), project.ID)
	status, _ := json.Marshal(map[string]any{
		"kind":      "status",
		"status":    current.Status,
		"url":       current.URL,
		"framework": current.Framework,
		"languages": current.Languages,
		"stack":     current.Stack,
	})
	fmt.Fprintf(w, "data: %s\n\n", status)
	flusher.Flush()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case line, ok := <-ch:
			if !ok {
				return
			}
			payload, _ := json.Marshal(line)
			fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
		}
	}
}

func (s *Server) syncDetectedVars(ctx context.Context, projectID, nodeID string) {
	dir := deploy.SourceDir(s.cfg, projectID, nodeID)
	if _, err := os.Stat(dir); err != nil {
		return
	}
	for _, pair := range envfile.Discover(dir) {
		if envfile.IsNoiseKey(pair.Key) {
			continue
		}
		_ = s.store.UpsertVar(ctx, projectID, store.EnvVar{
			ID:     ident.New("var_"),
			NodeID: nodeID,
			Key:    pair.Key,
			Value:  pair.Value,
			Source: "detected",
		})
	}
}

func (s *Server) loadProject(w http.ResponseWriter, r *http.Request) (store.Project, error) {
	project, err := s.store.GetOwnedProject(r.Context(), userFrom(r).ID, r.PathValue("id"))
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "Project not found")
		return store.Project{}, err
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not load project")
		return store.Project{}, err
	}
	return project, nil
}

func writeErr(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func cloneMessage(err error) string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "github app is not configured"):
		return "GitHub App is not configured on the platform."
	case strings.Contains(msg, "github token"):
		return "Could not mint a GitHub installation token. Reconnect the app."
	case strings.Contains(msg, "authentication") || strings.Contains(msg, "401") || strings.Contains(msg, "403"):
		return "GitHub denied access to that repository. Reconnect the app and grant Contents: Read."
	case strings.Contains(msg, "not found") || strings.Contains(msg, "repository not found"):
		return "Repository not found or not granted to the GitHub App."
	case strings.Contains(msg, "invalid branch"):
		return "That branch does not exist."
	default:
		return "Could not clone the GitHub repository"
	}
}

func githubBranchMessage(err error) string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "invalid repository"):
		return "Use a valid owner/repository"
	case strings.Contains(msg, "not found"):
		return "Repository not found or not granted to the GitHub App."
	case strings.Contains(msg, "401") || strings.Contains(msg, "403") || strings.Contains(msg, "authentication"):
		return "GitHub denied access to those branches. Reconnect the app."
	default:
		return "Could not list branches for this repository"
	}
}

func githubNeedsReconnect(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "401") ||
		strings.Contains(msg, "403") ||
		strings.Contains(msg, "404") ||
		strings.Contains(msg, "bad credentials") ||
		strings.Contains(msg, "not found") ||
		strings.Contains(msg, "public key") ||
		strings.Contains(msg, "integration") ||
		strings.Contains(msg, "jwt") ||
		strings.Contains(msg, "web token")
}

func githubReposMessage(err error) string {
	if err == nil {
		return "Could not list GitHub repositories"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "public key") || strings.Contains(msg, "jwt") || strings.Contains(msg, "integration") || strings.Contains(msg, "web token"):
		return "GitHub App credentials are invalid. Recreate the private key for this app."
	case strings.Contains(msg, "rate limit"):
		return "GitHub rate-limited repository listing. Try again in a minute."
	case githubNeedsReconnect(err):
		return "GitHub denied repository access. Connect GitHub again and grant the repositories you want to deploy."
	default:
		return "Could not list GitHub repositories. Reconnect GitHub and grant repository access."
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writePrivateJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Cache-Control", "private, no-store")
	writeJSON(w, status, payload)
}

func NewHTTPServer(cfg config.Config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       3 * time.Minute,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}
}

func (s *Server) projectName(ctx context.Context, projectID string) string {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return ""
	}
	return project.Name
}

func (s *Server) namedServices(ctx context.Context, extra []store.Node) []edge.Named {
	nodes, err := s.store.ListServiceNodes(ctx)
	if err != nil {
		return namedServices(extra, nil)
	}
	return namedServices(nodes, extra)
}

func (s *Server) exposeNodeURLs(ctx context.Context, nodes []store.Node) []store.Node {
	return s.applyVisitURLs(ctx, nodes)
}

func (s *Server) stampBuildClocks(ctx context.Context, projectID string, nodes []store.Node) []store.Node {
	starts, err := s.store.OpenBuildStarts(ctx, projectID)
	if err != nil || len(starts) == 0 {
		return nodes
	}
	return applyBuildClocks(nodes, starts)
}

func applyBuildClocks(nodes []store.Node, starts map[string]time.Time) []store.Node {
	if len(starts) == 0 {
		return nodes
	}
	out := append([]store.Node(nil), nodes...)
	for i := range out {
		t, ok := starts[out[i].ID]
		if !ok {
			continue
		}
		// Unfinished deployment = build still in flight. Keep the live clock and
		// never paint green from a stale "running" row while the job is open.
		started := t
		out[i].StartedAt = &started
		if out[i].Status != "building" {
			out[i].Status = "building"
			out[i].Error = nil
		}
	}
	return out
}

func (s *Server) exposeProjectURL(project store.Project, nodes []store.Node) store.Project {
	var urls []*string
	for _, node := range nodes {
		if node.Kind == "service" && strings.TrimSpace(deref(node.URL)) != "" {
			urls = append(urls, node.URL)
		}
	}
	if len(urls) == 1 {
		project.URL = urls[0]
		return project
	}
	project.URL = nil
	return project
}

func (s *Server) publicNodeURL(node store.Node, named []edge.Named) string {
	if node.Kind != "service" {
		return deref(node.URL)
	}
	// Empty slots have no address. Once a codebase is attached, the platform
	// hostname is stable and stays visible before the first deploy succeeds.
	if !serviceHasAddress(node) {
		return ""
	}
	port := 0
	if node.Port != nil {
		port = *node.Port
	}
	if strings.TrimSpace(s.cfg.EdgeDomain) == "" && strings.TrimSpace(s.cfg.AppPathPrefix) == "" {
		return deref(node.URL)
	}
	if slug := edge.NormalizePlatformSlug(node.PlatformSlug); slug != "" && !edge.ReservedSlug(slug) {
		return edge.Origin(s.cfg, slug)
	}
	return edge.AppURL(s.cfg, node.ID, port, edge.HostSlug(node.ID, node.Title, named))
}

func serviceHasAddress(node store.Node) bool {
	if strings.TrimSpace(deref(node.SourceType)) != "" || strings.TrimSpace(deref(node.URL)) != "" {
		return true
	}
	switch node.Status {
	case "ready", "building", "running", "failed", "stopped":
		return true
	default:
		return false
	}
}

func pickIdleService(services []store.Node, title string) (store.Node, bool) {
	for _, node := range services {
		if serviceOccupied(node) {
			continue
		}
		if strings.TrimSpace(title) != "" {
			node.Title = title
		}
		return node, true
	}
	return store.Node{}, false
}

func nextServicePoint(nodes []store.Node) (float64, float64) {
	x, y := 0.0, 0.0
	for _, node := range nodes {
		switch node.Kind {
		case "service", "postgresql", "redis", "container":
		default:
			continue
		}
		if node.X >= x {
			x = node.X + 280
			y = node.Y
		}
	}
	return x, y
}

func WorkDir(cfg config.Config) error {
	return os.MkdirAll(filepath.Join(cfg.WorkDir, "projects"), 0o755)
}
