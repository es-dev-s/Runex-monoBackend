package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"deploy_platform/internal/edge"
	"deploy_platform/internal/store"
)

type adminDomain struct {
	Host   string `json:"host"`
	URL    string `json:"url,omitempty"`
	Kind   string `json:"kind"`
	Status string `json:"status,omitempty"`
}

type adminProject struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	RAMUsedBytes int64         `json:"ramUsedBytes"`
	CPUPercent   float64       `json:"cpuPercent"`
	Domains      []adminDomain `json:"domains"`
}

type adminAccount struct {
	ID           string         `json:"id"`
	Username     string         `json:"username"`
	Email        string         `json:"email"`
	Name         string         `json:"name"`
	Role         string         `json:"role"`
	CreatedAt    time.Time      `json:"createdAt"`
	RevokedAt    *time.Time     `json:"revokedAt,omitempty"`
	SignedIn     bool           `json:"signedIn"`
	Days         int            `json:"days"`
	RAMUsedBytes int64          `json:"ramUsedBytes"`
	CPUPercent   float64        `json:"cpuPercent"`
	Projects     []adminProject `json:"projects"`
	Plan         planInfo       `json:"plan"`
	// PlanChangedAt is when an administrator last switched this plan.
	PlanChangedAt *time.Time `json:"planChangedAt,omitempty"`
}

func (s *Server) adminListUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	users, err := s.adminDirectory(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not load accounts")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

func (s *Server) adminLive(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "stream", "Streaming is unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	wake := s.subscribeAdmins()
	defer s.unsubscribeAdmins(wake)
	if !s.writeAdminSnapshot(w, r) {
		return
	}
	flusher.Flush()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-wake:
		case <-ticker.C:
		}
		if !s.writeAdminSnapshot(w, r) {
			return
		}
		flusher.Flush()
	}
}

func (s *Server) watchSession(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "Sign in to continue")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "stream", "Streaming is unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprintf(w, "data: %s\n\n", `{"kind":"ok"}`)
	flusher.Flush()

	kick := s.subscribeKick(user.ID)
	// planKey tracks the account's plan so an admin change (or the trial
	// running out) reaches the open dashboard within a second.
	planKey := ""
	defer s.unsubscribeKick(user.ID, kick)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-kick:
			fmt.Fprintf(w, "data: %s\n\n", `{"kind":"revoked"}`)
			flusher.Flush()
			return
		case <-ticker.C:
			fresh, err := s.store.UserBySession(r.Context(), cookie.Value)
			if err != nil {
				fmt.Fprintf(w, "data: %s\n\n", `{"kind":"revoked"}`)
				flusher.Flush()
				return
			}
			if key := planKeyFor(fresh, time.Now()); key != planKey {
				if planKey != "" {
					fmt.Fprintf(w, "data: %s\n\n", `{"kind":"plan"}`)
					flusher.Flush()
				}
				planKey = key
			}
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) writeAdminSnapshot(w http.ResponseWriter, r *http.Request) bool {
	users, err := s.adminDirectory(r.Context())
	if err != nil {
		return false
	}
	payload, err := json.Marshal(map[string]any{"users": users})
	if err != nil {
		return false
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", payload)
	return err == nil
}

func (s *Server) adminDirectory(ctx context.Context) ([]adminAccount, error) {
	accounts, err := s.store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	workloads, err := s.store.ListAdminWorkloads(ctx)
	if err != nil {
		return nil, err
	}
	hosts, err := s.store.ListAdminHosts(ctx)
	if err != nil {
		return nil, err
	}
	return assembleDirectory(accounts, workloads, hosts, func(nodeID string) (int64, float64) {
		if s.sampler == nil {
			return 0, 0
		}
		return s.sampler.Read(nodeID)
	}, func(slug string) string {
		return edge.Origin(s.cfg, slug)
	}, time.Now()), nil
}

func (s *Server) adminRow(ctx context.Context, id string) (adminAccount, bool) {
	users, err := s.adminDirectory(ctx)
	if err != nil {
		return adminAccount{}, false
	}
	for _, user := range users {
		if user.ID == id {
			return user, true
		}
	}
	return adminAccount{}, false
}

func assembleDirectory(accounts []store.Account, workloads []store.AdminWorkload, hosts []store.AdminHost, live func(string) (int64, float64), platform func(string) string, now time.Time) []adminAccount {
	projects := map[string]map[string]*adminProject{}
	order := map[string][]string{}
	seenNode := map[string]struct{}{}
	for _, row := range workloads {
		byUser := projects[row.UserID]
		if byUser == nil {
			byUser = map[string]*adminProject{}
			projects[row.UserID] = byUser
		}
		item := byUser[row.ProjectID]
		if item == nil {
			item = &adminProject{ID: row.ProjectID, Name: row.ProjectName, Domains: []adminDomain{}}
			byUser[row.ProjectID] = item
			order[row.UserID] = append(order[row.UserID], row.ProjectID)
		}
		if row.NodeID == "" {
			continue
		}
		if _, ok := seenNode[row.NodeID]; ok {
			continue
		}
		seenNode[row.NodeID] = struct{}{}
		ram, cpu := live(row.NodeID)
		item.RAMUsedBytes += ram
		item.CPUPercent += cpu
		if row.Kind == "service" && strings.TrimSpace(row.PlatformSlug) != "" && !row.PlatformRevoked {
			raw := platform(row.PlatformSlug)
			if host := hostOf(raw); host != "" && !hasDomain(item.Domains, host) {
				item.Domains = append(item.Domains, adminDomain{Host: host, URL: raw, Kind: "platform", Status: "active"})
			}
		}
	}
	for _, host := range hosts {
		byUser := projects[host.UserID]
		if byUser == nil {
			continue
		}
		item := byUser[host.ProjectID]
		if item == nil || host.Hostname == "" || hasDomain(item.Domains, host.Hostname) {
			continue
		}
		status := host.Status
		if status == "" {
			status = "pending"
		}
		item.Domains = append(item.Domains, adminDomain{
			Host:   host.Hostname,
			URL:    "https://" + host.Hostname,
			Kind:   "custom",
			Status: status,
		})
	}

	out := make([]adminAccount, 0, len(accounts))
	for _, account := range accounts {
		role := account.Role
		if role == "" {
			role = store.RoleUser
		}
		view := adminAccount{
			ID:            account.ID,
			Username:      account.Username,
			Email:         account.Email,
			Name:          account.Name,
			Role:          role,
			CreatedAt:     account.CreatedAt,
			RevokedAt:     account.RevokedAt,
			SignedIn:      account.SignedIn,
			Days:          daysOnPlatform(account.CreatedAt, now),
			Projects:      []adminProject{},
			Plan:          accountPlan(account, now),
			PlanChangedAt: account.PlanChangedAt,
		}
		for _, id := range order[account.ID] {
			item := projects[account.ID][id]
			if item.Domains == nil {
				item.Domains = []adminDomain{}
			}
			view.Projects = append(view.Projects, *item)
			view.RAMUsedBytes += item.RAMUsedBytes
			view.CPUPercent += item.CPUPercent
		}
		out = append(out, view)
	}
	return out
}

func daysOnPlatform(created, now time.Time) int {
	if created.IsZero() {
		return 0
	}
	start := time.Date(created.UTC().Year(), created.UTC().Month(), created.UTC().Day(), 0, 0, 0, 0, time.UTC)
	today := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	days := int(today.Sub(start).Hours()/24) + 1
	if days < 1 {
		return 1
	}
	return days
}

func hostOf(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return strings.TrimSpace(raw)
	}
	return parsed.Host
}

func hasDomain(domains []adminDomain, host string) bool {
	for _, domain := range domains {
		if strings.EqualFold(domain.Host, host) {
			return true
		}
	}
	return false
}

func (s *Server) subscribeAdmins() chan struct{} {
	ch := make(chan struct{}, 1)
	s.liveMu.Lock()
	if s.adminWait == nil {
		s.adminWait = map[chan struct{}]struct{}{}
	}
	s.adminWait[ch] = struct{}{}
	s.liveMu.Unlock()
	return ch
}

func (s *Server) unsubscribeAdmins(ch chan struct{}) {
	s.liveMu.Lock()
	delete(s.adminWait, ch)
	s.liveMu.Unlock()
}

func (s *Server) wakeAdmins() {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	for ch := range s.adminWait {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (s *Server) subscribeKick(userID string) chan struct{} {
	ch := make(chan struct{}, 1)
	s.liveMu.Lock()
	if s.kickWait == nil {
		s.kickWait = map[string]map[chan struct{}]struct{}{}
	}
	if s.kickWait[userID] == nil {
		s.kickWait[userID] = map[chan struct{}]struct{}{}
	}
	s.kickWait[userID][ch] = struct{}{}
	s.liveMu.Unlock()
	return ch
}

func (s *Server) unsubscribeKick(userID string, ch chan struct{}) {
	s.liveMu.Lock()
	delete(s.kickWait[userID], ch)
	if len(s.kickWait[userID]) == 0 {
		delete(s.kickWait, userID)
	}
	s.liveMu.Unlock()
}

func (s *Server) kick(userID string) {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	for ch := range s.kickWait[userID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
