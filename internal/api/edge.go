package api

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"deploy_platform/internal/customdomain"
	"deploy_platform/internal/edge"
	"deploy_platform/internal/store"
)

func (s *Server) withEdge(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if platformAPIPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		prefix := strings.TrimSpace(s.cfg.AppPathPrefix)
		if prefix == "" {
			prefix = "/apps"
		}
		if slug, rest, ok := edge.ParseAppPath(r.URL.Path, prefix); ok && !edge.ReservedSlug(slug) {
			s.proxyEdge(w, r, slug, rest)
			return
		}
		slug, ok := edge.ParseAnyHost(r.Host, s.cfg.EdgeDomain, s.cfg.LanDomain)
		if ok && !edge.ReservedSlug(slug) {
			s.proxyEdge(w, r, slug, "")
			return
		}
		if node, err := s.lookupCustomDomainFromRequest(r); err == nil {
			s.serveEdge(w, r, node, "")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) lookupCustomDomainFromRequest(r *http.Request) (store.Node, error) {
	if node, err := s.lookupCustomDomain(r.Context(), r.Host); err == nil {
		return node, nil
	}
	if !edge.Loopback(r.RemoteAddr) {
		return store.Node{}, customdomain.ErrInvalid
	}
	forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if i := strings.IndexByte(forwarded, ','); i >= 0 {
		forwarded = strings.TrimSpace(forwarded[:i])
	}
	if forwarded == "" || strings.EqualFold(forwarded, r.Host) {
		return store.Node{}, customdomain.ErrInvalid
	}
	return s.lookupCustomDomain(r.Context(), forwarded)
}

func platformAPIPath(path string) bool {
	switch path {
	case "/healthz", "/readyz", "/v1":
		return true
	default:
		return strings.HasPrefix(path, "/v1/")
	}
}

func (s *Server) proxyEdge(w http.ResponseWriter, r *http.Request, slug, rest string) {
	node, err := s.lookupEdgeService(r.Context(), slug)
	if err != nil {
		// 404, not 502. Cloudflare replaces an origin 502 with its own
		// "Bad gateway" page, which hides the real reason.
		http.Error(w, "No service is published at this address.", http.StatusNotFound)
		return
	}
	if node.PlatformURLRevoked {
		http.Error(w, "This Runex URL is no longer active", http.StatusNotFound)
		return
	}
	s.serveEdge(w, r, node, rest)
}

func (s *Server) serveEdge(w http.ResponseWriter, r *http.Request, node store.Node, rest string) {
	if node.Port == nil || *node.Port <= 0 || (node.Status != "running" && !edgePortOpen(*node.Port)) {
		name := strings.TrimSpace(node.Title)
		if name == "" {
			name = "This service"
		}
		http.Error(w, name+" is not deployed yet. Deploy it from the Runex dashboard, then open this address again.", http.StatusNotFound)
		return
	}
	target, err := url.Parse("http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(*node.Port)))
	if err != nil {
		http.Error(w, "This service is not reachable", http.StatusBadGateway)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(rw http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(rw, "This service is not reachable", http.StatusBadGateway)
	}
	proxy.Transport = &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	incomingHost := r.Host
	director := proxy.Director
	proxy.Director = func(req *http.Request) {
		director(req)
		if rest != "" {
			req.URL.Path = rest
			req.URL.RawPath = ""
		}
		req.Host = incomingHost
		req.Header.Set("X-Forwarded-Host", incomingHost)
		req.Header.Set("X-Forwarded-Proto", edge.ForwardedProto(r))
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		edge.AllowDashboardFrame(resp.Header, s.cfg.WebURL)
		return nil
	}
	if rest != "" {
		r.URL.Path = rest
		r.URL.RawPath = ""
	}
	proxy.ServeHTTP(w, r)
}

func (s *Server) lookupEdgeService(ctx context.Context, slug string) (store.Node, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if slug == "" {
		return store.Node{}, fmt.Errorf("service %q not found", slug)
	}
	if node, err := s.store.GetServiceBySlug(ctx, slug); err == nil {
		return node, nil
	}
	nodes, err := s.store.ListServiceNodes(ctx)
	if err != nil {
		return store.Node{}, err
	}
	named := namedServices(nodes, nil)
	assigned := edge.HostSlugs(named)
	claimed := map[string]string{}
	for id, host := range assigned {
		claimed[host] = id
	}

	if id, ok := claimed[slug]; ok {
		if node, ok := nodeByID(nodes, id); ok {
			return preferRunning([]store.Node{node}), nil
		}
	}

	var aliases []store.Node
	for _, node := range nodes {
		if edge.MatchesSlug(slug, node.Title, node.ID) {
			if owner, taken := claimed[slug]; taken && owner != node.ID {
				continue
			}
			aliases = append(aliases, node)
		}
	}
	if node, ok := uniqueService(aliases); ok {
		return node, nil
	}

	if node, ok := s.lookupProjectNameAlias(ctx, slug, nodes, claimed); ok {
		return node, nil
	}
	return store.Node{}, fmt.Errorf("service %q not found", slug)
}

func edgePortOpen(port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 800*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func namedServices(nodes []store.Node, extra []store.Node) []edge.Named {
	seen := map[string]bool{}
	out := make([]edge.Named, 0, len(nodes)+len(extra))
	add := func(node store.Node) {
		if node.Kind != "service" || strings.TrimSpace(node.ID) == "" || seen[node.ID] {
			return
		}
		seen[node.ID] = true
		out = append(out, edge.Named{ID: node.ID, Title: node.Title, PreferredSlug: node.PlatformSlug})
	}
	for _, node := range nodes {
		add(node)
	}
	for _, node := range extra {
		add(node)
	}
	return out
}

func nodeByID(nodes []store.Node, id string) (store.Node, bool) {
	for _, node := range nodes {
		if node.ID == id {
			return node, true
		}
	}
	return store.Node{}, false
}

func preferRunning(nodes []store.Node) store.Node {
	for _, node := range nodes {
		if node.Status == "running" {
			return node
		}
	}
	if len(nodes) == 0 {
		return store.Node{}
	}
	return nodes[0]
}

func uniqueService(nodes []store.Node) (store.Node, bool) {
	if len(nodes) == 0 {
		return store.Node{}, false
	}
	running := make([]store.Node, 0, len(nodes))
	for _, node := range nodes {
		if node.Status == "running" {
			running = append(running, node)
		}
	}
	if len(running) == 1 {
		return running[0], true
	}
	if len(nodes) == 1 {
		return nodes[0], true
	}
	return store.Node{}, false
}

func (s *Server) lookupProjectNameAlias(ctx context.Context, slug string, nodes []store.Node, claimed map[string]string) (store.Node, bool) {
	if owner := claimed[slug]; owner != "" {
		return store.Node{}, false
	}
	byProject := map[string][]store.Node{}
	for _, node := range nodes {
		if !serviceOccupied(node) {
			continue
		}
		byProject[node.ProjectID] = append(byProject[node.ProjectID], node)
	}
	var matches []store.Node
	for projectID, group := range byProject {
		if len(group) != 1 {
			continue
		}
		project, err := s.store.GetProject(ctx, projectID)
		if err != nil {
			continue
		}
		if !edge.MatchesSlug(slug, project.Name) {
			continue
		}
		matches = append(matches, group[0])
	}
	return uniqueService(matches)
}
