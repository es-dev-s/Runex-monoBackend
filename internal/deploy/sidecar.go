package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"deploy_platform/internal/alloc"
	"deploy_platform/internal/ident"
	"deploy_platform/internal/logs"
	"deploy_platform/internal/runtime"
	"deploy_platform/internal/sidecarquery"
	"deploy_platform/internal/store"
	"deploy_platform/internal/tcpproxy"
)

var ErrNotDatabase = errors.New("not a database")
var ErrDatabaseStopped = errors.New("database is not running")

type DatabaseOverview struct {
	Kind        string `json:"kind"`
	Status      string `json:"status"`
	Version     string `json:"version,omitempty"`
	InternalURL string `json:"internalUrl"`
	PublicURL   string `json:"publicUrl"`
	Host        string `json:"host"`
	PublicHost  string `json:"publicHost"`
	Port        int    `json:"port"`
	PublicPort  int    `json:"publicPort"`
	User        string `json:"user,omitempty"`
	Password    string `json:"password,omitempty"`
	Database    string `json:"database,omitempty"`
	Keys        int64  `json:"keys,omitempty"`
	Memory      string `json:"memory,omitempty"`
}

func sidecarContainer(projectID, nodeID, kind string) string {
	switch kind {
	case "postgresql":
		return runtime.PostgresName(projectID, nodeID)
	case "redis":
		return runtime.RedisName(projectID, nodeID)
	default:
		return ""
	}
}

type sidecarPlan struct {
	kind     string
	name     string
	alias    string
	password string
	port     int
	release  func()
	opts     runtime.SidecarOpts
}

// AttachSidecar writes the connection URLs immediately and boots the container
// in the background. The node row must already exist.
func (e *Engine) AttachSidecar(node store.Node) (store.Node, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	plan, err := e.allocateSidecar(ctx, node)
	if err != nil {
		return node, err
	}
	node.Port = &plan.port
	name := plan.name
	node.ContainerName = &name
	node.Status = "building"
	node.Caption = "Starting"
	node.Error = nil
	if err := e.store.UpsertNode(ctx, node); err != nil {
		plan.release()
		return node, err
	}
	plan.release()
	go e.bootSidecar(node, plan)
	return node, nil
}

func (e *Engine) StartSidecar(ctx context.Context, projectID, nodeID, kind string) (string, error) {
	node := store.Node{ID: nodeID, ProjectID: projectID, Kind: kind, Status: "building", Caption: "Starting"}
	if existing, err := e.store.GetNode(ctx, projectID, nodeID); err == nil {
		node = existing
		node.Kind = kind
	}
	plan, err := e.allocateSidecar(ctx, node)
	if err != nil {
		return "", err
	}
	defer plan.release()
	runCtx, cancel := context.WithTimeout(ctx, e.dockerRunTimeout())
	defer cancel()
	if err := e.runSidecarContainer(runCtx, plan); err != nil {
		return "", err
	}
	return plan.name, nil
}

// sidecarPassword returns the credential the container must come up with. A new
// database gets a fresh secret, but a Postgres data directory keeps whatever
// password it was initialised with: POSTGRES_PASSWORD is only honoured on first
// boot. So when a container is recreated over an existing volume, generating a
// new secret would publish a connection URL that cannot authenticate.
func (e *Engine) sidecarPassword(ctx context.Context, node store.Node, kind string) string {
	fresh := ident.Secret(24)
	volumeKey := "pg"
	if kind == "redis" {
		volumeKey = "redis"
	}
	if !e.docker.VolumeExists(ctx, runtime.VolumeName(node.ProjectID, node.ID, volumeKey)) {
		return fresh
	}
	env := e.docker.ContainerEnv(ctx, e.sidecarName(node))
	if kind == "redis" {
		if pass := sidecarquery.RedisPassword(env); pass != "" {
			return pass
		}
		return fresh
	}
	if pass := strings.TrimSpace(env["POSTGRES_PASSWORD"]); pass != "" {
		return pass
	}
	// The volume holds an initialised cluster but its password is no longer
	// recoverable. Reusing the stored one is the only chance of authenticating.
	if stored := e.storedSidecarPassword(ctx, node, kind); stored != "" {
		return stored
	}
	return fresh
}

func (e *Engine) storedSidecarPassword(ctx context.Context, node store.Node, kind string) string {
	vars, err := e.store.ListVars(ctx, node.ProjectID, node.ID)
	if err != nil {
		return ""
	}
	want := "PGPASSWORD"
	if kind == "redis" {
		want = "REDIS_PASSWORD"
	}
	for _, item := range vars {
		if item.Key == want {
			return strings.TrimSpace(item.Value)
		}
	}
	return ""
}

func (e *Engine) allocateSidecar(ctx context.Context, node store.Node) (sidecarPlan, error) {
	kind := node.Kind
	// The container about to be created gets a fresh address and password, so
	// any pool held for this node is already pointing at the old one.
	e.pools.Forget(node.ID)
	network := runtime.NetworkName(node.ProjectID)
	if err := e.docker.EnsureNetwork(ctx, network); err != nil {
		return sidecarPlan{}, err
	}
	index := e.sidecarIndexFor(ctx, node.ProjectID, kind, node.ID)
	alias := runtime.SidecarAlias(kind, node.ID, index)
	port, release, err := e.reservePortOn(ctx, "0.0.0.0")
	if err != nil {
		return sidecarPlan{}, err
	}
	fail := func(cause error) (sidecarPlan, error) {
		release()
		return sidecarPlan{}, cause
	}
	password := e.sidecarPassword(ctx, node, kind)
	memMB, _ := alloc.Resolve(kind, 0, 0, e.cfg.MaxMemoryMB, e.cfg.MaxDiskMB)
	if existing, err := e.store.GetNode(ctx, node.ProjectID, node.ID); err == nil {
		memMB, _ = e.resolveAlloc(existing)
	}
	memory := alloc.DockerMemory(memMB)
	plan := sidecarPlan{
		kind:     kind,
		alias:    alias,
		password: password,
		port:     port,
		release:  release,
	}
	switch kind {
	case "postgresql":
		plan.name = runtime.PostgresName(node.ProjectID, node.ID)
		vol := runtime.VolumeName(node.ProjectID, node.ID, "pg")
		if err := e.docker.EnsureVolume(ctx, vol, node.ProjectID, node.ID, "postgresql"); err != nil {
			return fail(err)
		}
		plan.opts = runtime.SidecarOpts{
			Name:          plan.name,
			Image:         "postgres:16-alpine",
			Network:       network,
			Alias:         alias,
			ProjectID:     node.ProjectID,
			NodeID:        node.ID,
			Role:          "postgresql",
			Memory:        memory,
			CPUs:          alloc.CPUs(kind, memMB),
			HostPort:      port,
			ContainerPort: 5432,
			Volume:        vol + ":/var/lib/postgresql/data",
			Cmd:           alloc.PostgresTuning(memMB),
			Env: map[string]string{
				"POSTGRES_USER":     "app",
				"POSTGRES_PASSWORD": password,
				"POSTGRES_DB":       "app",
			},
		}
		e.persistSidecarAccess(ctx, node.ProjectID, node.ID, kind, alias, sidecarquery.Creds{
			User: "app", Password: password, Database: "app",
		}, password, port)
		return plan, nil
	case "redis":
		plan.name = runtime.RedisName(node.ProjectID, node.ID)
		vol := runtime.VolumeName(node.ProjectID, node.ID, "redis")
		if err := e.docker.EnsureVolume(ctx, vol, node.ProjectID, node.ID, "redis"); err != nil {
			return fail(err)
		}
		plan.opts = runtime.SidecarOpts{
			Name:          plan.name,
			Image:         "redis:7-alpine",
			Network:       network,
			Alias:         alias,
			ProjectID:     node.ProjectID,
			NodeID:        node.ID,
			Role:          "redis",
			Memory:        memory,
			CPUs:          alloc.CPUs(kind, memMB),
			HostPort:      port,
			ContainerPort: 6379,
			Volume:        vol + ":/data",
			Cmd: []string{
				"redis-server",
				"--appendonly", "yes",
				"--requirepass", password,
				"--maxmemory", alloc.RedisMaxMemory(memMB),
				// This is the customer's database, not our cache. Evicting
				// would silently delete their keys, which contradicts keeping
				// an append-only file; refusing the write tells them instead.
				"--maxmemory-policy", "noeviction",
			},
			Env: map[string]string{
				"REDIS_PASSWORD": password,
			},
		}
		e.persistSidecarAccess(ctx, node.ProjectID, node.ID, kind, alias, sidecarquery.Creds{}, password, port)
		return plan, nil
	default:
		return fail(ErrNotDatabase)
	}
}

func (e *Engine) runSidecarContainer(ctx context.Context, plan sidecarPlan) error {
	return e.docker.RunSidecar(ctx, plan.opts)
}

func (e *Engine) bootSidecar(node store.Node, plan sidecarPlan) {
	ctx, cancel := context.WithTimeout(context.Background(), e.dockerRunTimeout())
	defer cancel()
	if err := e.runSidecarContainer(ctx, plan); err != nil {
		e.markSidecar(node, "failed", "Could not start the database")
		return
	}
	if err := e.waitSidecarAccept(ctx, plan.name, plan.port); err != nil {
		e.markSidecar(node, "failed", "The database did not accept connections in time")
		return
	}
	e.markSidecar(node, "running", "Database is ready")
}

func (e *Engine) markSidecar(node store.Node, status, text string) {
	node.Status = status
	if status == "running" {
		node.Caption = "Isolated"
		node.Error = nil
	} else {
		node.Caption = "Failed"
		msg := text
		node.Error = &msg
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = e.store.UpsertNode(ctx, node)
	e.hub.Publish(node.ProjectID, logs.Line{
		Kind:   "status",
		Status: status,
		Phase:  "start",
		Text:   text,
		NodeID: node.ID,
	})
}

func (e *Engine) waitSidecarAccept(ctx context.Context, name string, port int) error {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	var last error
	for {
		if err := ctx.Err(); err != nil {
			if last != nil {
				return last
			}
			return err
		}
		status := e.docker.ContainerStatus(ctx, name)
		if status == "exited" || status == "dead" || status == "removing" {
			return fmt.Errorf("database container stopped")
		}
		conn, err := net.DialTimeout("tcp", addr, 400*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		last = err
		timer := time.NewTimer(300 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return last
		case <-timer.C:
		}
	}
}

func (e *Engine) persistSidecarAccess(ctx context.Context, projectID, nodeID, kind, alias string, creds sidecarquery.Creds, redisPass string, port int) {
	_ = e.store.SetNodePort(ctx, projectID, nodeID, port)
	e.writeSidecarVars(ctx, projectID, nodeID, kind, alias, creds, redisPass, port)
}

func (e *Engine) writeSidecarVars(ctx context.Context, projectID, nodeID, kind, alias string, creds sidecarquery.Creds, redisPass string, port int) {
	publicHost := e.publicTCPHost()
	var pairs [][2]string
	switch kind {
	case "postgresql":
		pairs = [][2]string{
			{"DATABASE_URL", creds.PostgresURL(alias, 5432)},
			{"DATABASE_PUBLIC_URL", creds.PostgresURL(publicHost, port)},
			{"DATABASE_PRIVATE_URL", creds.PostgresURL(alias, 5432)},
			{"PGHOST", alias},
			{"PGPORT", "5432"},
			{"PGUSER", creds.User},
			{"PGPASSWORD", creds.Password},
			{"PGDATABASE", creds.Database},
		}
	case "redis":
		pairs = [][2]string{
			{"REDIS_URL", sidecarquery.RedisURL(alias, 6379, redisPass)},
			{"REDIS_PUBLIC_URL", sidecarquery.RedisURL(publicHost, port, redisPass)},
			{"REDIS_PRIVATE_URL", sidecarquery.RedisURL(alias, 6379, redisPass)},
			{"REDISHOST", alias},
			{"REDISPORT", "6379"},
			{"REDIS_PASSWORD", redisPass},
		}
	default:
		return
	}
	// Reads call this on every request to keep the URLs current. Rewriting
	// identical rows would add a round trip per key to every table listing.
	if !e.sidecarVarsChanged(nodeID, pairs) {
		return
	}
	vars := make([]store.EnvVar, 0, len(pairs))
	for _, pair := range pairs {
		vars = append(vars, store.EnvVar{
			ID:     ident.New("var_"),
			NodeID: nodeID,
			Key:    pair[0],
			Value:  pair[1],
			Source: "platform",
		})
	}
	if err := e.store.UpsertVars(ctx, projectID, vars); err != nil {
		e.forgetSidecarVars(nodeID)
	}
}

func (e *Engine) sidecarVarsChanged(nodeID string, pairs [][2]string) bool {
	sum := sha256.New()
	for _, pair := range pairs {
		sum.Write([]byte(pair[0]))
		sum.Write([]byte{0})
		sum.Write([]byte(pair[1]))
		sum.Write([]byte{0})
	}
	fingerprint := hex.EncodeToString(sum.Sum(nil))
	e.varsMu.Lock()
	defer e.varsMu.Unlock()
	if e.sidecarVars == nil {
		e.sidecarVars = map[string]string{}
	}
	if e.sidecarVars[nodeID] == fingerprint {
		return false
	}
	e.sidecarVars[nodeID] = fingerprint
	return true
}

func (e *Engine) forgetSidecarVars(nodeID string) {
	e.varsMu.Lock()
	delete(e.sidecarVars, nodeID)
	e.varsMu.Unlock()
}

func (e *Engine) publicTCPHost() string {
	for _, raw := range []string{e.cfg.PublicBase, e.cfg.WebURL, e.cfg.PublicURL} {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil {
			continue
		}
		host := u.Hostname()
		if host == "" || net.ParseIP(host) != nil {
			continue
		}
		return host
	}
	if host := strings.TrimSpace(e.cfg.EdgeDomain); host != "" {
		return host
	}
	return "127.0.0.1"
}

func (e *Engine) StopSidecar(ctx context.Context, projectID, nodeID, kind string, stored *string) {
	e.closeProxy(nodeID)
	e.forgetPort(nodeID)
	e.forgetSidecarVars(nodeID)
	e.pools.Forget(nodeID)
	switch kind {
	case "postgresql":
		e.docker.RemoveContainer(ctx, runtime.PostgresName(projectID, nodeID))
		e.docker.RemoveVolume(ctx, runtime.VolumeName(projectID, nodeID, "pg"))
	case "redis":
		e.docker.RemoveContainer(ctx, runtime.RedisName(projectID, nodeID))
		e.docker.RemoveVolume(ctx, runtime.VolumeName(projectID, nodeID, "redis"))
	}
	if stored != nil && strings.TrimSpace(*stored) != "" {
		e.docker.RemoveContainer(ctx, *stored)
	}
}

func (e *Engine) sidecarName(node store.Node) string {
	if node.ContainerName != nil && strings.TrimSpace(*node.ContainerName) != "" {
		return *node.ContainerName
	}
	return sidecarContainer(node.ProjectID, node.ID, node.Kind)
}

func (e *Engine) lockSidecar(nodeID string) func() {
	e.sidecarMu.Lock()
	if e.sidecarLocks == nil {
		e.sidecarLocks = map[string]*sync.Mutex{}
	}
	m, ok := e.sidecarLocks[nodeID]
	if !ok {
		m = &sync.Mutex{}
		e.sidecarLocks[nodeID] = m
	}
	e.sidecarMu.Unlock()
	m.Lock()
	return m.Unlock
}

func (e *Engine) EnsureSidecar(ctx context.Context, projectID, nodeID string) (store.Node, error) {
	unlock := e.lockSidecar(nodeID)
	defer unlock()
	node, err := e.store.GetNode(ctx, projectID, nodeID)
	if err != nil {
		return store.Node{}, err
	}
	if node.Kind != "postgresql" && node.Kind != "redis" {
		return store.Node{}, ErrNotDatabase
	}
	name := e.sidecarName(node)
	if !e.docker.Running(ctx, name) {
		return node, ErrDatabaseStopped
	}
	env := e.docker.ContainerEnv(ctx, name)
	creds := sidecarquery.PostgresCreds(env)
	redisPass := sidecarquery.RedisPassword(env)
	index := e.sidecarIndexFor(ctx, projectID, node.Kind, node.ID)
	alias := runtime.SidecarAlias(node.Kind, node.ID, index)
	port, err := e.ensurePublicPort(ctx, node, name, sidecarTargetPort(node.Kind))
	if err != nil {
		return node, err
	}
	node.Port = &port
	e.writeSidecarVars(ctx, projectID, nodeID, node.Kind, alias, creds, redisPass, port)
	return node, nil
}

func sidecarTargetPort(kind string) int {
	if kind == "redis" {
		return 6379
	}
	return 5432
}

func (e *Engine) ensurePublicPort(ctx context.Context, node store.Node, name string, target int) (int, error) {
	// A published port only changes when the container is recreated. Trusting
	// the cached value keeps reads off both the docker CLI and a port write.
	if cached, ok := e.cachedPort(node.ID); ok && node.Port != nil && *node.Port == cached {
		return cached, nil
	}
	if published := e.docker.PublishedPort(ctx, name); published > 0 {
		e.closeProxy(node.ID)
		if node.Port == nil || *node.Port != published {
			_ = e.store.SetNodePort(ctx, node.ProjectID, node.ID, published)
		}
		e.rememberPort(node.ID, published)
		return published, nil
	}
	port := 0
	if node.Port != nil && *node.Port > 0 {
		port = *node.Port
	} else {
		p, release, err := e.reservePortOn(ctx, "0.0.0.0")
		if err != nil {
			return 0, err
		}
		port = p
		defer release()
	}
	if err := e.startProxy(node.ID, port, name, target); err != nil {
		if node.Port != nil && *node.Port > 0 {
			p, release, err2 := e.reservePortOn(ctx, "0.0.0.0")
			if err2 != nil {
				return 0, err
			}
			defer release()
			port = p
			if err := e.startProxy(node.ID, port, name, target); err != nil {
				return 0, err
			}
		} else {
			return 0, err
		}
	}
	if node.Port == nil || *node.Port != port {
		_ = e.store.SetNodePort(ctx, node.ProjectID, node.ID, port)
	}
	e.rememberPort(node.ID, port)
	return port, nil
}

func (e *Engine) cachedPort(nodeID string) (int, bool) {
	e.portsMu.Lock()
	defer e.portsMu.Unlock()
	port, ok := e.knownPorts[nodeID]
	return port, ok && port > 0
}

func (e *Engine) rememberPort(nodeID string, port int) {
	if port <= 0 {
		return
	}
	e.portsMu.Lock()
	if e.knownPorts == nil {
		e.knownPorts = map[string]int{}
	}
	e.knownPorts[nodeID] = port
	e.portsMu.Unlock()
}

func (e *Engine) forgetPort(nodeID string) {
	e.portsMu.Lock()
	delete(e.knownPorts, nodeID)
	e.portsMu.Unlock()
}

func (e *Engine) startProxy(nodeID string, port int, container string, targetPort int) error {
	e.proxyMu.Lock()
	defer e.proxyMu.Unlock()
	if current, ok := e.proxies[nodeID]; ok {
		_ = current.Close()
		delete(e.proxies, nodeID)
	}
	server, err := tcpproxy.Listen(fmt.Sprintf("0.0.0.0:%d", port), func() (string, error) {
		ip, err := e.docker.ContainerIPv4(context.Background(), container)
		if err != nil {
			return "", err
		}
		return net.JoinHostPort(ip, fmt.Sprintf("%d", targetPort)), nil
	})
	if err != nil {
		return err
	}
	e.proxies[nodeID] = server
	return nil
}

func (e *Engine) closeProxy(nodeID string) {
	e.proxyMu.Lock()
	defer e.proxyMu.Unlock()
	if current, ok := e.proxies[nodeID]; ok {
		_ = current.Close()
		delete(e.proxies, nodeID)
	}
}

func (e *Engine) RestoreSidecarProxies(ctx context.Context) {
	nodes, err := e.store.ListAllSidecars(ctx)
	if err != nil {
		return
	}
	for _, node := range nodes {
		if node.Status != "running" {
			continue
		}
		name := e.sidecarName(node)
		if !e.docker.Running(ctx, name) {
			continue
		}
		if e.docker.PublishedPort(ctx, name) > 0 {
			continue
		}
		if node.Port == nil || *node.Port <= 0 {
			continue
		}
		_ = e.startProxy(node.ID, *node.Port, name, sidecarTargetPort(node.Kind))
	}
}

func (e *Engine) openSidecar(ctx context.Context, projectID, nodeID string) (store.Node, string, sidecarquery.Creds, string, error) {
	node, err := e.EnsureSidecar(ctx, projectID, nodeID)
	if err != nil {
		return store.Node{}, "", sidecarquery.Creds{}, "", err
	}
	name := e.sidecarName(node)
	env := e.docker.ContainerEnv(ctx, name)
	return node, name, sidecarquery.PostgresCreds(env), sidecarquery.RedisPassword(env), nil
}

func (e *Engine) DatabaseOverview(ctx context.Context, projectID, nodeID string) (DatabaseOverview, error) {
	node, err := e.EnsureSidecar(ctx, projectID, nodeID)
	if err != nil && !errors.Is(err, ErrDatabaseStopped) {
		return DatabaseOverview{}, err
	}
	index := e.sidecarIndexFor(ctx, projectID, node.Kind, node.ID)
	alias := runtime.SidecarAlias(node.Kind, node.ID, index)
	port := 0
	if node.Port != nil {
		port = *node.Port
	}
	out := DatabaseOverview{
		Kind:       node.Kind,
		Status:     node.Status,
		Host:       alias,
		PublicHost: e.publicTCPHost(),
		PublicPort: port,
	}
	name := e.sidecarName(node)
	env := e.docker.ContainerEnv(ctx, name)
	creds := sidecarquery.PostgresCreds(env)
	redisPass := sidecarquery.RedisPassword(env)
	if errors.Is(err, ErrDatabaseStopped) || !e.docker.Running(ctx, name) {
		if node.Status != "" && node.Status != "running" {
			out.Status = node.Status
		} else {
			out.Status = "stopped"
		}
		if node.Kind == "postgresql" {
			out.User, out.Password, out.Database = creds.User, creds.Password, creds.Database
			out.Port = 5432
			out.InternalURL = creds.PostgresURL(alias, 5432)
			if port > 0 {
				out.PublicURL = creds.PostgresURL(out.PublicHost, port)
			}
		} else {
			out.Port = 6379
			out.Password = redisPass
			out.InternalURL = sidecarquery.RedisURL(alias, 6379, redisPass)
			if port > 0 {
				out.PublicURL = sidecarquery.RedisURL(out.PublicHost, port, redisPass)
			}
		}
		if out.InternalURL == "" || out.Password == "" {
			e.fillOverviewFromVars(ctx, &out, node)
		}
		return out, nil
	}

	ip, err := e.docker.ContainerIPv4(ctx, name)
	if err != nil {
		return out, err
	}
	qctx, cancel := context.WithTimeout(ctx, sidecarquery.QueryTimeout)
	defer cancel()
	if node.Kind == "postgresql" {
		out.User, out.Password, out.Database = creds.User, creds.Password, creds.Database
		out.Port = 5432
		out.InternalURL = creds.PostgresURL(alias, 5432)
		if port > 0 {
			out.PublicURL = creds.PostgresURL(out.PublicHost, port)
		}
		conn, err := e.pools.Postgres(qctx, node.ID, ip, creds)
		if err != nil {
			return out, nil
		}
		defer conn.Release()
		out.Version = sidecarquery.PostgresVersion(qctx, conn.Conn())
		return out, nil
	}
	out.Port = 6379
	out.Password = redisPass
	out.InternalURL = sidecarquery.RedisURL(alias, 6379, redisPass)
	if port > 0 {
		out.PublicURL = sidecarquery.RedisURL(out.PublicHost, port, redisPass)
	}
	rdb, err := e.pools.Redis(qctx, node.ID, ip, redisPass)
	if err != nil {
		return out, nil
	}
	out.Keys, out.Memory, out.Version = sidecarquery.RedisInfo(qctx, rdb)
	return out, nil
}

func (e *Engine) fillOverviewFromVars(ctx context.Context, out *DatabaseOverview, node store.Node) {
	items, err := e.store.ListVars(ctx, node.ProjectID, node.ID)
	if err != nil {
		return
	}
	vals := map[string]string{}
	for _, item := range items {
		vals[item.Key] = item.Value
	}
	if node.Kind == "redis" {
		if vals["REDIS_URL"] != "" {
			out.InternalURL = vals["REDIS_URL"]
		}
		if vals["REDIS_PUBLIC_URL"] != "" {
			out.PublicURL = vals["REDIS_PUBLIC_URL"]
		}
		if vals["REDISHOST"] != "" {
			out.Host = vals["REDISHOST"]
		}
		if vals["REDIS_PASSWORD"] != "" {
			out.Password = vals["REDIS_PASSWORD"]
		}
		if out.Port == 0 {
			out.Port = 6379
		}
		return
	}
	if vals["DATABASE_URL"] != "" {
		out.InternalURL = vals["DATABASE_URL"]
	}
	if vals["DATABASE_PUBLIC_URL"] != "" {
		out.PublicURL = vals["DATABASE_PUBLIC_URL"]
	}
	if vals["PGHOST"] != "" {
		out.Host = vals["PGHOST"]
	}
	if vals["PGUSER"] != "" {
		out.User = vals["PGUSER"]
	}
	if vals["PGPASSWORD"] != "" {
		out.Password = vals["PGPASSWORD"]
	}
	if vals["PGDATABASE"] != "" {
		out.Database = vals["PGDATABASE"]
	}
	if out.Port == 0 {
		out.Port = 5432
	}
}

func (e *Engine) DatabaseTables(ctx context.Context, projectID, nodeID string) ([]sidecarquery.Table, error) {
	node, _, creds, _, err := e.openSidecar(ctx, projectID, nodeID)
	if err != nil {
		return nil, err
	}
	if node.Kind != "postgresql" {
		return nil, ErrNotDatabase
	}
	ip, err := e.docker.ContainerIPv4(ctx, e.sidecarName(node))
	if err != nil {
		return nil, err
	}
	qctx, cancel := context.WithTimeout(ctx, sidecarquery.QueryTimeout)
	defer cancel()
	conn, err := e.pools.Postgres(qctx, node.ID, ip, creds)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	return sidecarquery.ListTables(qctx, conn.Conn())
}

func (e *Engine) DatabaseRows(ctx context.Context, projectID, nodeID, schema, table string, limit, offset int, sort, q, after string) (sidecarquery.RowPage, error) {
	node, _, creds, _, err := e.openSidecar(ctx, projectID, nodeID)
	if err != nil {
		return sidecarquery.RowPage{}, err
	}
	if node.Kind != "postgresql" {
		return sidecarquery.RowPage{}, ErrNotDatabase
	}
	if schema == "" {
		schema = "public"
	}
	ip, err := e.docker.ContainerIPv4(ctx, e.sidecarName(node))
	if err != nil {
		return sidecarquery.RowPage{}, err
	}
	qctx, cancel := context.WithTimeout(ctx, sidecarquery.QueryTimeout)
	defer cancel()
	conn, err := e.pools.Postgres(qctx, node.ID, ip, creds)
	if err != nil {
		return sidecarquery.RowPage{}, err
	}
	defer conn.Release()
	return sidecarquery.TableRows(qctx, conn.Conn(), schema, table, limit, offset, sort, q, after)
}

func (e *Engine) DatabaseKeys(ctx context.Context, projectID, nodeID, match string, cursor uint64, limit int) (sidecarquery.KeyPage, error) {
	node, _, _, redisPass, err := e.openSidecar(ctx, projectID, nodeID)
	if err != nil {
		return sidecarquery.KeyPage{}, err
	}
	if node.Kind != "redis" {
		return sidecarquery.KeyPage{}, ErrNotDatabase
	}
	ip, err := e.docker.ContainerIPv4(ctx, e.sidecarName(node))
	if err != nil {
		return sidecarquery.KeyPage{}, err
	}
	qctx, cancel := context.WithTimeout(ctx, sidecarquery.QueryTimeout)
	defer cancel()
	rdb, err := e.pools.Redis(qctx, node.ID, ip, redisPass)
	if err != nil {
		return sidecarquery.KeyPage{}, err
	}
	return sidecarquery.ListKeys(qctx, rdb, match, cursor, limit)
}

func (e *Engine) DatabaseKey(ctx context.Context, projectID, nodeID, key string) (sidecarquery.KeyValue, error) {
	node, _, _, redisPass, err := e.openSidecar(ctx, projectID, nodeID)
	if err != nil {
		return sidecarquery.KeyValue{}, err
	}
	if node.Kind != "redis" {
		return sidecarquery.KeyValue{}, ErrNotDatabase
	}
	ip, err := e.docker.ContainerIPv4(ctx, e.sidecarName(node))
	if err != nil {
		return sidecarquery.KeyValue{}, err
	}
	qctx, cancel := context.WithTimeout(ctx, sidecarquery.QueryTimeout)
	defer cancel()
	rdb, err := e.pools.Redis(qctx, node.ID, ip, redisPass)
	if err != nil {
		return sidecarquery.KeyValue{}, err
	}
	return sidecarquery.ReadKey(qctx, rdb, key)
}

func (e *Engine) sidecarLink(ctx context.Context, projectID, kind string) *SidecarLink {
	nodes, err := e.store.ListNodes(ctx, projectID)
	if err != nil {
		return nil
	}
	index := 0
	for _, node := range nodes {
		if node.Kind != kind {
			continue
		}
		alias := runtime.SidecarAlias(kind, node.ID, index)
		index++
		name := e.sidecarName(node)
		env := e.docker.ContainerEnv(ctx, name)
		if kind == "postgresql" {
			creds := sidecarquery.PostgresCreds(env)
			return &SidecarLink{Host: alias, Port: "5432", User: creds.User, Password: creds.Password, Database: creds.Database}
		}
		pass := sidecarquery.RedisPassword(env)
		return &SidecarLink{Host: alias, Port: "6379", Password: pass}
	}
	return nil
}
