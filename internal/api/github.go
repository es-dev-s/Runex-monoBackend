package api

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"deploy_platform/internal/cache"
	"deploy_platform/internal/deploy"
	"deploy_platform/internal/githubapp"
	"deploy_platform/internal/ingest"
	"deploy_platform/internal/logs"
	"deploy_platform/internal/store"
)

type githubBody struct {
	URL            string `json:"url"`
	Repo           string `json:"repo"`
	Branch         string `json:"branch"`
	SHA            string `json:"sha"`
	NodeID         string `json:"nodeId"`
	InstallationID int64  `json:"installationId"`
	AutoDeploy     *bool  `json:"autoDeploy"`
}

type githubAppCache struct {
	Name     string `json:"name"`
	Verified bool   `json:"verified"`
}

func (s *Server) githubStatus(w http.ResponseWriter, r *http.Request) {
	appName := ""
	verified := false
	if s.github.Enabled() {
		var cached githubAppCache
		if s.cache != nil && s.cache.GetJSON(r.Context(), cache.KeyGitHubApp, &cached) {
			appName = cached.Name
			verified = cached.Verified
		} else if name, err := s.github.GetApp(); err == nil {
			appName = name
			verified = true
			if s.cache != nil {
				s.cache.SetJSON(r.Context(), cache.KeyGitHubApp, githubAppCache{Name: name, Verified: true}, 45*time.Second)
			}
		}
	}
	installs, err := s.store.ListInstallations(r.Context(), userFrom(r).ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not load GitHub installations")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": s.github.Enabled(),
		"verified":   verified,
		"name":       appName,
		"slug":       s.cfg.GitHubAppSlug,
		"appId":      s.cfg.GitHubAppID,
		"webhookUrl": s.cfg.PublicURL + "/v1/github/webhook",
		// Setup URL must hit THIS API. GitHub redirects the browser here after
		// install; the handler then bounces to the dashboard callback. Pointing
		// it at the Next app (or a stale /api/platform path) drops the install.
		"setupUrl":      s.cfg.PublicURL + "/v1/github/setup",
		"publicUrl":     s.cfg.PublicURL,
		"webUrl":        s.cfg.WebURL,
		"installations": installs,
		"connected":     len(installs) > 0,
		"webhookSecret": strings.TrimSpace(s.cfg.GitHubAppWebhookSecret) != "",
	})
}

func (s *Server) githubInstall(w http.ResponseWriter, r *http.Request) {
	if !s.github.Enabled() || s.cfg.GitHubAppSlug == "" {
		writeErr(w, http.StatusServiceUnavailable, "github_unconfigured", "Create a GitHub App and add GITHUB_APP_ID, GITHUB_APP_SLUG, and GITHUB_APP_PRIVATE_KEY to backend/.env")
		return
	}
	user := userFrom(r)
	returnPath := githubapp.SafeReturnPath(r.URL.Query().Get("return"))
	intentID, err := s.store.CreateConnectIntent(r.Context(), user.ID, returnPath, 30*time.Minute)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "github_connect", "Could not start GitHub connect")
		return
	}
	http.SetCookie(w, s.connectCookieFor(r, intentID, 30*time.Minute))
	if dash := s.dashboardFromRequest(r); dash != "" {
		http.SetCookie(w, s.dashboardCookie(dash, 30*time.Minute))
	}
	state := githubapp.EncodeIntentState(s.cfg.GitHubAppWebhookSecret, intentID)
	writeJSON(w, http.StatusOK, map[string]any{
		"url": s.github.InstallURL(state),
	})
}

func (s *Server) githubSetup(w http.ResponseWriter, r *http.Request) {
	returnPath := "/deployments"
	userID := ""
	intentID := ""
	if id, err := githubapp.DecodeIntentState(s.cfg.GitHubAppWebhookSecret, r.URL.Query().Get("state")); err == nil {
		intentID = id
	} else if cookie, err := r.Cookie(connectCookie); err == nil {
		intentID = cookie.Value
	}
	if intentID != "" {
		if intent, err := s.store.GetConnectIntent(r.Context(), intentID); err == nil {
			userID = intent.UserID
			returnPath = githubapp.SafeReturnPath(intent.ReturnPath)
		}
	}
	if userID == "" {
		if uid, decoded, err := githubapp.DecodeUserState(s.cfg.GitHubAppWebhookSecret, r.URL.Query().Get("state")); err == nil && uid != "" {
			userID = uid
			returnPath = decoded
		}
	}
	if session, err := s.sessionUser(r); err == nil {
		userID = session.ID
	}
	installationID, _ := githubapp.ParseID(r.URL.Query().Get("installation_id"))
	status := "connected"
	if userID == "" {
		status = "signin"
	} else if installationID == 0 {
		status = "missing"
	} else if err := s.saveUserInstallation(r.Context(), userID, installationID); err != nil {
		status = "error"
	} else {
		s.store.DeleteConnectIntent(r.Context(), intentID)
		if s.cache != nil {
			s.cache.Delete(r.Context(), cache.KeyGitHubSync)
			s.cache.Delete(r.Context(), cache.ReposKey(installationID))
		}
	}
	s.clearConnectCookie(w, r)
	query := url.Values{}
	query.Set("github", status)
	query.Set("return", returnPath)
	dash := strings.TrimRight(s.cfg.WebURL, "/")
	if fromCookie := s.dashboardFromCookie(r); fromCookie != "" {
		dash = fromCookie
	}
	target := dash + "/github/callback?" + query.Encode()
	http.Redirect(w, r, target, http.StatusFound)
}

func (s *Server) githubComplete(w http.ResponseWriter, r *http.Request) {
	// Linking happens only from GET /v1/github/setup with GitHub's
	// installation_id. Do not scan every App install and attach the only one —
	// that handed another user's GitHub to the next Runex session.
	s.clearConnectCookie(w, r)
	s.githubStatus(w, r)
}

func (s *Server) saveUserInstallation(ctx context.Context, userID string, installationID int64) error {
	if userID == "" || installationID == 0 {
		return fmt.Errorf("missing github install")
	}
	inst := store.Installation{ID: installationID, Account: "GitHub", Type: "User", UserID: userID}
	if s.github.Enabled() {
		if remote, err := s.github.GetInstallation(installationID); err == nil {
			inst.Account = remote.Account
			inst.Type = remote.Type
		}
	}
	if err := s.store.UpsertInstallation(ctx, inst); err != nil {
		return err
	}
	return s.store.LinkUserInstallation(ctx, userID, installationID)
}

const connectCookie = "github_connect"

func (s *Server) clearConnectCookie(w http.ResponseWriter, r *http.Request) {
	c := s.connectCookieFor(r, "", -1)
	c.MaxAge = -1
	c.Expires = time.Unix(0, 0)
	http.SetCookie(w, c)
	d := s.dashboardCookie("", -1)
	d.MaxAge = -1
	d.Expires = time.Unix(0, 0)
	http.SetCookie(w, d)
}

func (s *Server) githubRepos(w http.ResponseWriter, r *http.Request) {
	if !s.github.Enabled() {
		writeErr(w, http.StatusServiceUnavailable, "github_unconfigured", "GitHub App is not configured")
		return
	}
	installationID, _ := githubapp.ParseID(r.URL.Query().Get("installationId"))
	if installationID == 0 {
		installs, err := s.store.ListInstallations(r.Context(), userFrom(r).ID)
		if err != nil || len(installs) == 0 {
			writeJSON(w, http.StatusOK, map[string]any{"repos": []githubapp.Repo{}, "installationId": 0})
			return
		}
		installationID = installs[0].ID
	} else if ok, err := s.store.InstallationOwned(r.Context(), userFrom(r).ID, installationID); err != nil || !ok {
		writeJSON(w, http.StatusOK, map[string]any{"repos": []githubapp.Repo{}, "installationId": 0})
		return
	}
	key := cache.ReposKey(installationID)
	if s.cache != nil {
		var cached cachedRepoList
		if s.cache.GetJSON(r.Context(), key, &cached) && cached.At > 0 {
			age := time.Since(time.Unix(cached.At, 0))
			if age < repoCacheTTL {
				if age > 20*time.Second && s.cache.Claim(r.Context(), key+":refresh", 15*time.Second) {
					go s.refreshRepos(installationID)
				}
				writeRepoList(w, installationID, cached.Repos)
				return
			}
		}
	}
	repos, err := s.listReposFresh(installationID)
	if err != nil {
		code := "github_repos"
		status := http.StatusBadGateway
		if githubNeedsReconnect(err) {
			code = "github_reconnect"
			status = http.StatusConflict
		}
		writeErr(w, status, code, githubReposMessage(err))
		return
	}
	if s.cache != nil {
		s.cache.SetJSON(r.Context(), key, cachedRepoList{At: time.Now().Unix(), Repos: repos}, repoCacheTTL)
	}
	writeRepoList(w, installationID, repos)
}

const repoCacheTTL = 3 * time.Minute

type cachedRepoList struct {
	At    int64            `json:"at"`
	Repos []githubapp.Repo `json:"repos"`
}

func writeRepoList(w http.ResponseWriter, installationID int64, repos []githubapp.Repo) {
	if repos == nil {
		repos = []githubapp.Repo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"installationId": installationID,
		"repos":          repos,
	})
}

func (s *Server) listReposFresh(installationID int64) ([]githubapp.Repo, error) {
	return s.github.ListRepos(installationID)
}

func (s *Server) refreshRepos(installationID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	listed, err := s.listReposFresh(installationID)
	if err != nil || s.cache == nil {
		return
	}
	s.cache.SetJSON(ctx, cache.ReposKey(installationID), cachedRepoList{At: time.Now().Unix(), Repos: listed}, repoCacheTTL)
}

func (s *Server) githubBranches(w http.ResponseWriter, r *http.Request) {
	owner := strings.TrimSpace(r.PathValue("owner"))
	repo := strings.TrimSpace(r.PathValue("repo"))
	if !githubapp.ValidGitHubName(owner) || !githubapp.ValidGitHubName(repo) {
		writeErr(w, http.StatusBadRequest, "invalid_repo", "Use a valid owner/repository")
		return
	}
	var installationID int64
	if r.URL.Query().Has("installationId") {
		installationID, _ = githubapp.ParseID(r.URL.Query().Get("installationId"))
	} else if s.github.Enabled() {
		installs, err := s.store.ListInstallations(r.Context(), userFrom(r).ID)
		if err == nil && len(installs) > 0 {
			installationID = installs[0].ID
		}
	}
	if installationID == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"defaultBranch": "", "branches": []githubapp.Branch{}})
		return
	}
	if ok, err := s.store.InstallationOwned(r.Context(), userFrom(r).ID, installationID); err != nil || !ok {
		writeJSON(w, http.StatusOK, map[string]any{"defaultBranch": "", "branches": []githubapp.Branch{}})
		return
	}
	fullName := owner + "/" + repo
	type payload struct {
		DefaultBranch string             `json:"defaultBranch"`
		Branches      []githubapp.Branch `json:"branches"`
	}
	var cached payload
	key := cache.BranchesKey(installationID, fullName)
	if s.cache != nil && s.cache.GetJSON(r.Context(), key, &cached) {
		writeJSON(w, http.StatusOK, cached)
		return
	}
	known := strings.TrimSpace(r.URL.Query().Get("default"))
	defaultBranch, branches, err := s.github.ListBranches(r.Context(), installationID, owner, repo, known)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "github_branches", githubBranchMessage(err))
		return
	}
	out := payload{DefaultBranch: defaultBranch, Branches: branches}
	if s.cache != nil {
		s.cache.SetJSON(r.Context(), key, out, 20*time.Second)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) githubWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "Could not read webhook")
		return
	}
	if !githubapp.VerifyWebhook(s.cfg.GitHubAppWebhookSecret, r.Header.Get("X-Hub-Signature-256"), body) {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "Invalid webhook signature")
		return
	}
	// GitHub delivers at least once: a timeout, a 5xx, or an operator clicking
	// "Redeliver" replays the same event. Without this claim each replay starts
	// another build of the same commit, and concurrent builds of one service
	// fight over the same container name and port.
	if delivery := strings.TrimSpace(r.Header.Get("X-GitHub-Delivery")); delivery != "" {
		if !s.cache.Claim(r.Context(), cache.DeliveryKey(delivery), 10*time.Minute) {
			writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate"})
			return
		}
	}
	switch strings.TrimSpace(r.Header.Get("X-GitHub-Event")) {
	case "ping":
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case "installation":
		s.handleInstallationWebhook(w, body)
	case "installation_repositories":
		s.handleInstallationWebhook(w, body)
	case "push":
		s.handlePushWebhook(w, body)
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
	}
}

func (s *Server) handleInstallationWebhook(w http.ResponseWriter, body []byte) {
	change, err := githubapp.ParseInstallation(body)
	if err != nil || change.InstallationID == 0 {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	switch change.Action {
	case "deleted", "suspend":
		_ = s.store.DeleteInstallation(context.Background(), change.InstallationID)
	default:
		_ = s.store.UpsertInstallation(context.Background(), store.Installation{
			ID:      change.InstallationID,
			Account: change.Account,
			Type:    change.Type,
		})
	}
	if s.cache != nil {
		s.cache.Delete(context.Background(), cache.KeyGitHubSync)
		s.cache.Delete(context.Background(), cache.ReposKey(change.InstallationID))
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handlePushWebhook(w http.ResponseWriter, body []byte) {
	evt, err := githubapp.ParsePush(body)
	if err != nil || evt.Deleted || evt.Branch == "" || evt.InstallationID == 0 {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	if strings.Trim(evt.After, "0") == "" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	if s.cache != nil {
		s.cache.Delete(context.Background(), cache.ReposKey(evt.InstallationID))
		s.cache.Delete(context.Background(), cache.BranchesKey(evt.InstallationID, evt.FullName))
	}
	go s.redeployPush(evt)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

func (s *Server) redeployPush(evt githubapp.PushEvent) {
	ctx := context.Background()
	nodes, err := s.store.ListAutoDeployNodes(ctx, evt.InstallationID, evt.FullName, evt.Branch)
	if err != nil {
		return
	}
	if len(nodes) == 0 {
		s.warnPushIgnored(ctx, evt)
		return
	}
	ref, err := ingest.ParseGitHub(evt.FullName)
	if err != nil {
		return
	}
	sha := evt.After
	if len(sha) > 7 {
		sha = sha[:7]
	}
	for _, node := range nodes {
		project, err := s.store.GetProject(ctx, node.ProjectID)
		if err != nil {
			continue
		}
		if !s.computeAllowedFor(ctx, project.ID) {
			s.hub.Publish(project.ID, logs.Line{
				Kind:   "log",
				Phase:  "build",
				Level:  "warn",
				Text:   "GitHub push on " + evt.Branch + " skipped: the free trial has ended. Contact support to deploy again.",
				NodeID: node.ID,
			})
			continue
		}
		branch := evt.Branch
		if node.GitHubBranch != nil && strings.TrimSpace(*node.GitHubBranch) != "" {
			branch = strings.TrimSpace(*node.GitHubBranch)
		}
		_ = s.cloneAndKickGitHub(ctx, project, node, ref, githubClone{
			Branch:         branch,
			SHA:            evt.After,
			InstallationID: evt.InstallationID,
			AutoDeploy:     true,
		}, fmt.Sprintf("GitHub push on %s @ %s — fetching a fresh copy of %s", evt.Branch, sha, evt.FullName))
	}
}

type githubClone struct {
	Branch         string
	SHA            string
	InstallationID int64
	AutoDeploy     bool
}

func (s *Server) cloneAndKickGitHub(_ context.Context, project store.Project, node store.Node, ref ingest.GitRef, spec githubClone, kickoffLog string) error {
	runCtx, waitPrev, finish := s.engine.Jobs.Takeover(node.ID)
	s.beginDeploy(project, node, "github", ref.Owner+"/"+ref.Name, "github", runCtx, waitPrev, finish, func(jobCtx context.Context, depID string) (string, error) {
		if strings.TrimSpace(kickoffLog) != "" {
			s.emitDeployLog(project.ID, node.ID, depID, "build", "info", kickoffLog)
		}
		return s.cloneGitHubSource(jobCtx, &node, ref, spec)
	})
	return nil
}

func (s *Server) warnPushIgnored(ctx context.Context, evt githubapp.PushEvent) {
	nodes, err := s.store.ListGitHubNodes(ctx, evt.InstallationID, evt.FullName)
	if err != nil || len(nodes) == 0 {
		return
	}
	for _, node := range nodes {
		project, err := s.store.GetProject(ctx, node.ProjectID)
		if err != nil {
			continue
		}
		text := fmt.Sprintf("GitHub push on %s @ %s — auto-deploy is off", evt.Branch, evt.FullName)
		if node.GitHubBranch != nil {
			watched := strings.TrimSpace(*node.GitHubBranch)
			if watched != "" && watched != evt.Branch && watched != "refs/heads/"+evt.Branch {
				text = fmt.Sprintf("GitHub push on %s — this service watches %s", evt.Branch, watched)
			}
		}
		s.hub.Publish(project.ID, logs.Line{
			Kind:   "log",
			Phase:  "build",
			Level:  "warn",
			Text:   text,
			NodeID: node.ID,
		})
	}
}

func (s *Server) cloneGitHubSource(ctx context.Context, node *store.Node, ref ingest.GitRef, spec githubClone) (string, error) {
	token, err := s.githubToken(spec.InstallationID)
	if err != nil {
		return "", err
	}
	dir, err := deploy.PrepareDir(s.cfg, node.ProjectID, node.ID)
	if err != nil {
		return "", err
	}
	cloneCtx, cancel := context.WithTimeout(ctx, s.cfg.CloneTimeout)
	defer cancel()
	if err := ingest.CloneGitHub(cloneCtx, dir, ref, ingest.CloneOpts{
		Token:  token,
		Branch: spec.Branch,
		SHA:    spec.SHA,
	}); err != nil {
		return "", err
	}
	if spec.InstallationID != 0 {
		id := spec.InstallationID
		node.GitHubInstallationID = &id
	}
	node.GitHubBranch = strPtr(spec.Branch)
	node.AutoDeploy = spec.AutoDeploy
	node.SourceType = strPtr("github")
	node.SourceRef = strPtr(ref.Owner + "/" + ref.Name)
	_ = s.store.UpsertNode(ctx, *node)
	_ = s.store.SetAutoDeploy(ctx, node.ProjectID, node.ID, spec.AutoDeploy)
	return dir, nil
}

func (s *Server) githubToken(installationID int64) (string, error) {
	if installationID != 0 {
		if !s.github.Enabled() {
			return "", fmt.Errorf("github app is not configured")
		}
		return s.github.InstallationToken(installationID)
	}
	// No installation: clone as a public repository. A personal token is optional.
	return strings.TrimSpace(s.cfg.GitHubToken), nil
}

func (s *Server) parseGitHubTarget(body githubBody) (ingest.GitRef, githubClone, error) {
	raw := firstNonEmpty(body.Repo, body.URL)
	ref, err := ingest.ParseGitHub(raw)
	if err != nil {
		return ingest.GitRef{}, githubClone{}, err
	}
	auto := true
	if body.AutoDeploy != nil {
		auto = *body.AutoDeploy
	}
	return ref, githubClone{
		Branch:         strings.TrimSpace(body.Branch),
		SHA:            strings.TrimSpace(body.SHA),
		InstallationID: body.InstallationID,
		AutoDeploy:     auto,
	}, nil
}

func (s *Server) originAllowed(origin string) bool {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return false
	}
	allowed := append([]string{s.cfg.WebURL, s.cfg.PublicURL}, s.cfg.CORSOrigins...)
	for candidate := range allowedOrigins(allowed...) {
		if origin == candidate {
			return true
		}
	}
	if s.cfg.Production() {
		return false
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func allowedOrigins(urls ...string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, raw := range urls {
		raw = strings.TrimRight(strings.TrimSpace(raw), "/")
		if raw == "" {
			continue
		}
		out[raw] = struct{}{}
		out[strings.Replace(raw, "localhost", "127.0.0.1", 1)] = struct{}{}
		out[strings.Replace(raw, "127.0.0.1", "localhost", 1)] = struct{}{}
		parsed, err := url.Parse(raw)
		if err != nil {
			continue
		}
		host := parsed.Hostname()
		if host == "" || host == "localhost" || host == "127.0.0.1" || host == "::1" {
			continue
		}
		port := parsed.Port()
		if strings.HasPrefix(strings.ToLower(host), "www.") {
			parsed.Host = originHost(host[4:], port)
		} else {
			parsed.Host = originHost("www."+host, port)
		}
		out[strings.TrimRight(parsed.String(), "/")] = struct{}{}
	}
	return out
}

func originHost(host, port string) string {
	if port == "" {
		return host
	}
	return net.JoinHostPort(host, port)
}
