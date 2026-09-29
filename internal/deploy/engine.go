package deploy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"deploy_platform/internal/config"
	"deploy_platform/internal/detect"
	"deploy_platform/internal/edge"
	"deploy_platform/internal/envfile"
	"deploy_platform/internal/ident"
	"deploy_platform/internal/ingest"
	"deploy_platform/internal/logs"
	"deploy_platform/internal/pack"
	"deploy_platform/internal/runtime"
	"deploy_platform/internal/sidecarquery"
	"deploy_platform/internal/stackmeta"
	"deploy_platform/internal/store"
	"deploy_platform/internal/tcpproxy"
)

type Engine struct {
	cfg    config.Config
	store  *store.Store
	docker *runtime.Client
	detect *detect.Client
	hub    *logs.Hub
	Jobs   *JobGate
	portMu sync.Mutex
	// reserved holds ports handed to in-flight deploys that have not been
	// persisted yet. Without it, two concurrent deploys could bind-test the
	// same free port and both believe they own it.
	reserved     map[int]struct{}
	proxyMu      sync.Mutex
	proxies      map[string]*tcpproxy.Server
	sidecarMu    sync.Mutex
	sidecarLocks map[string]*sync.Mutex
	varsMu       sync.Mutex
	// sidecarVars fingerprints the connection keys already persisted per node,
	// so repeated reads do not rewrite identical rows.
	sidecarVars map[string]string
	portsMu     sync.Mutex
	// knownPorts caches the published host port per sidecar node so a read does
	// not re-inspect the container and re-save the same port.
	knownPorts map[string]int
	// pools keeps a warm connection per database node so browsing tables and
	// rows does not pay a TCP and SCRAM handshake on every request.
	pools *sidecarquery.Pools
}

func New(cfg config.Config, st *store.Store, docker *runtime.Client, detect *detect.Client, hub *logs.Hub) *Engine {
	return &Engine{
		cfg:          cfg,
		store:        st,
		docker:       docker,
		detect:       detect,
		hub:          hub,
		Jobs:         NewJobGate(),
		reserved:     map[int]struct{}{},
		proxies:      map[string]*tcpproxy.Server{},
		sidecarLocks: map[string]*sync.Mutex{},
		sidecarVars:  map[string]string{},
		knownPorts:   map[string]int{},
		pools:        sidecarquery.NewPools(),
	}
}

type Request struct {
	ProjectID    string
	ServiceID    string
	SourceType   string
	SourceRef    string
	Dir          string
	Trigger      string
	DeploymentID string
	StartedAt    time.Time
}

func (e *Engine) Stage(ctx context.Context, req Request) error {
	node, err := e.store.GetNode(ctx, req.ProjectID, req.ServiceID)
	if err != nil {
		return err
	}
	node.SourceType = strPtr(req.SourceType)
	node.SourceRef = strPtr(req.SourceRef)
	node.Title = serviceTitle(req.SourceType, req.SourceRef, node.Title)
	node.Error = nil
	detected, detectErr := e.detect.Detect(ctx, req.Dir, req.SourceType, req.SourceRef)
	if detectErr != nil {
		e.hub.Publish(req.ProjectID, logs.Line{
			Kind:   "log",
			Phase:  "build",
			Level:  "warn",
			Text:   "Stack detect fallback: " + detectErr.Error(),
			NodeID: req.ServiceID,
		})
	}
	plan := pack.FromDetection(req.Dir, detected.Apps)
	node.Languages = languagesFrom(detected)
	node.Stack = stackFrom(detected, plan)
	node.Framework = strPtr(plan.Framework)
	node.Caption = display(plan.Framework, "Service")
	node.Status = "ready"
	_ = e.store.SetNodeStack(ctx, req.ProjectID, req.ServiceID, node.Languages, node.Stack)
	_ = e.store.UpsertNode(ctx, node)
	_ = e.store.RollupProject(ctx, req.ProjectID)
	e.publishStack(req.ProjectID, req.ServiceID, plan.Framework, node)
	e.seedDetectedVars(ctx, req.ProjectID, req.ServiceID, req.Dir)
	info := detectLine(plan, node.Languages) + ". Add variables, then Deploy."
	e.hub.Publish(req.ProjectID, logs.Line{
		Kind:   "log",
		Phase:  "build",
		Level:  "info",
		Text:   info,
		NodeID: req.ServiceID,
	})
	e.recordStage(ctx, req, node, info, detectErr)
	e.hub.Publish(req.ProjectID, logs.Line{
		Kind:      "status",
		Status:    "ready",
		Phase:     "detect",
		Framework: plan.Framework,
		NodeID:    req.ServiceID,
		Languages: toLogLanguages(node.Languages),
		Stack:     toLogStack(node.Stack),
	})
	return nil
}

// recordStage keeps the detect line in deploy history. The live hub frame is
// gone after a refresh, and logs require a deployment row.
func (e *Engine) recordStage(ctx context.Context, req Request, node store.Node, text string, detectErr error) {
	now := time.Now()
	dep := store.Deployment{
		ID:         ident.New("dep_"),
		ProjectID:  req.ProjectID,
		NodeID:     req.ServiceID,
		Status:     "staged",
		Phase:      "detect",
		Trigger:    "stage",
		SourceType: req.SourceType,
		SourceRef:  strPtr(req.SourceRef),
		Framework:  node.Framework,
		CreatedAt:  now,
	}
	if err := e.store.CreateDeployment(ctx, dep); err != nil {
		return
	}
	_ = e.store.FinishDeployment(ctx, dep)
	write := func(level, line string) {
		_ = e.store.AppendLog(ctx, store.LogLine{
			ProjectID:    req.ProjectID,
			DeploymentID: dep.ID,
			NodeID:       req.ServiceID,
			TS:           now,
			Phase:        "build",
			Level:        level,
			Text:         line,
		})
	}
	if detectErr != nil {
		write("warn", "Stack detect fallback: "+detectErr.Error())
	}
	write("info", text)
}

func (e *Engine) Run(ctx context.Context, req Request) error {
	node, err := e.store.GetNode(ctx, req.ProjectID, req.ServiceID)
	if err != nil {
		return err
	}
	trigger := strings.TrimSpace(req.Trigger)
	if trigger == "" {
		trigger = "manual"
	}
	depID := strings.TrimSpace(req.DeploymentID)
	if depID == "" {
		depID = ident.New("dep_")
	}
	dep := store.Deployment{
		ID:         depID,
		ProjectID:  req.ProjectID,
		NodeID:     req.ServiceID,
		Status:     "running",
		Phase:      "build",
		Trigger:    trigger,
		SourceType: req.SourceType,
		SourceRef:  strPtr(req.SourceRef),
		CreatedAt:  req.StartedAt,
	}
	if strings.TrimSpace(req.DeploymentID) == "" {
		if err := e.store.CreateDeployment(ctx, dep); err != nil {
			return err
		}
	}

	log := func(phase, level, text string) {
		line := store.LogLine{
			ProjectID:    req.ProjectID,
			DeploymentID: dep.ID,
			NodeID:       req.ServiceID,
			TS:           time.Now(),
			Phase:        phase,
			Level:        level,
			Text:         text,
		}
		_ = e.store.AppendLog(context.Background(), line)
		e.hub.Publish(req.ProjectID, logs.Line{
			TS:     line.TS,
			Phase:  phase,
			Level:  level,
			Text:   text,
			Kind:   "log",
			NodeID: req.ServiceID,
		})
	}
	startedAt := req.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	setNodeStatus := func(status, phase string) {
		node.Status = status
		_ = e.store.UpsertNode(ctx, node)
		_ = e.store.RollupProject(ctx, req.ProjectID)
		line := logs.Line{Kind: "status", Status: status, Phase: phase, NodeID: req.ServiceID}
		if status == "building" {
			t := startedAt
			line.StartedAt = &t
		}
		e.hub.Publish(req.ProjectID, line)
	}

	node.SourceType = strPtr(req.SourceType)
	node.SourceRef = strPtr(req.SourceRef)
	node.Title = serviceTitle(req.SourceType, req.SourceRef, node.Title)
	node.Error = nil
	_ = e.store.UpsertNode(ctx, node)
	setNodeStatus("building", "build")
	log("build", "info", "Preparing isolated workspace")

	detected, detectErr := e.detect.Detect(ctx, req.Dir, req.SourceType, req.SourceRef)
	if detectErr != nil {
		log("build", "warn", "Stack detect fallback: "+detectErr.Error())
	}
	plan := pack.FromDetection(req.Dir, detected.Apps)
	node.Languages = languagesFrom(detected)
	node.Stack = stackFrom(detected, plan)
	node.Framework = strPtr(plan.Framework)
	node.Caption = display(plan.Framework, "Service")
	_ = e.store.SetNodeStack(ctx, req.ProjectID, req.ServiceID, node.Languages, node.Stack)
	_ = e.store.UpsertNode(ctx, node)
	_ = e.store.RollupProject(ctx, req.ProjectID)
	e.publishStack(req.ProjectID, req.ServiceID, plan.Framework, node)
	e.seedDetectedVars(ctx, req.ProjectID, req.ServiceID, req.Dir)
	if strings.EqualFold(plan.Framework, "nextjs") {
		if err := e.ensureAuthSecrets(ctx, req.ProjectID, req.ServiceID, log); err != nil {
			return e.fail(ctx, node, dep, log, err)
		}
	}
	env, routed, err := e.runtimeEnv(ctx, req.ProjectID, req.ServiceID, plan.ContainerPort)
	if err != nil {
		return e.fail(ctx, node, dep, log, err)
	}
	if err := pack.WriteDockerfile(req.Dir, plan); err != nil {
		return e.fail(ctx, node, dep, log, err)
	}
	if err := e.writeBuildEnv(req.Dir, req.ProjectID, req.ServiceID); err != nil {
		return e.fail(ctx, node, dep, log, err)
	}
	log("build", "info", detectLine(plan, node.Languages))
	if plan.Image != "" {
		log("build", "info", "Runtime image "+plan.Image)
	}
	log("containerize", "cmd", "docker build")

	setNodeStatus("building", "containerize")
	image := runtime.ImageName(req.ProjectID, req.ServiceID)
	buildCtx, cancel := context.WithTimeout(ctx, e.cfg.BuildTimeout)
	defer cancel()
	if err := e.docker.Build(buildCtx, image, req.Dir, func(line string) {
		if KeepDeployLog("info", line) {
			log("containerize", "info", line)
		}
	}); err != nil {
		return e.fail(ctx, node, dep, log, err)
	}

	network := runtime.NetworkName(req.ProjectID)
	if err := e.docker.EnsureNetwork(ctx, network); err != nil {
		return e.fail(ctx, node, dep, log, err)
	}
	if len(routed) > 0 {
		log("start", "info", "Routed "+strings.Join(routed, ", ")+" to attached PostgreSQL/Redis")
	}

	// Blue/green cutover. The candidate is started on a fresh port under its own
	// name while the previous container keeps serving, so a version that fails to
	// boot leaves the live site untouched instead of taking it down.
	previous := e.docker.ServiceContainers(ctx, req.ProjectID, req.ServiceID)
	appName := runtime.ReleaseName(req.ProjectID, req.ServiceID, dep.ID)
	servingBefore := e.servingContainer(ctx, previous, appName)

	port, releasePort, err := e.reservePort(ctx, req.ProjectID, req.ServiceID)
	if err != nil {
		return e.fail(ctx, node, dep, log, err)
	}
	defer releasePort()

	// Reclaim a same-named leftover from an interrupted attempt at this release.
	e.docker.RemoveContainer(ctx, appName)

	// rollback abandons the candidate and reports why. The previous container is
	// never touched here, which is what keeps the old version online.
	rollback := func(cause error) error {
		if out := e.docker.Logs(context.Background(), appName, 80); out != "" {
			log("start", "error", out)
		}
		e.docker.RemoveContainer(context.Background(), appName)
		if servingBefore != "" {
			log("start", "warn", "Deploy failed — keeping the previous version online")
			return e.failKeepingPrevious(ctx, node, dep, log, cause)
		}
		return e.fail(ctx, node, dep, log, cause)
	}

	log("start", "cmd", "docker run --network "+network)
	setNodeStatus("building", "start")
	runCtx, cancelRun := context.WithTimeout(ctx, e.dockerRunTimeout())
	err = e.docker.RunApp(runCtx, runtime.RunOpts{
		Name:          appName,
		Image:         image,
		Network:       network,
		Alias:         runtime.NetworkAlias(req.ProjectID, req.ServiceID),
		ProjectID:     req.ProjectID,
		ServiceID:     req.ServiceID,
		HostPort:      port,
		ContainerPort: plan.ContainerPort,
		Memory:        e.dockerMemory(node),
		StorageSize:   e.dockerStorage(node),
		CPUs:          e.cfg.CPULimit,
		Env:           env,
	})
	cancelRun()
	if err != nil {
		if e.docker.Running(context.Background(), appName) {
			log("start", "warn", "docker run did not return, but the container is up")
		} else {
			if runCtx.Err() != nil && !errors.Is(err, context.Canceled) {
				err = fmt.Errorf("docker run did not return within %s; the daemon may be stuck. Click Deploy to retry", e.dockerRunTimeout())
			}
			return rollback(err)
		}
	}
	log("start", "info", "Container started, waiting for port "+fmt.Sprintf("%d", port))

	if ctx.Err() != nil {
		e.docker.RemoveContainer(context.Background(), appName)
		return ctx.Err()
	}
	if err := e.waitReady(ctx, port, appName, log); err != nil {
		if ctx.Err() != nil {
			e.docker.RemoveContainer(context.Background(), appName)
			return ctx.Err()
		}
		return rollback(err)
	}
	if ctx.Err() != nil {
		e.docker.RemoveContainer(context.Background(), appName)
		return ctx.Err()
	}

	// The candidate is healthy. Retire the old containers, then publish the new
	// port and URL. Ordering matters: nothing points at the candidate until it
	// has already answered on its port.
	for _, name := range previous {
		if name == appName {
			continue
		}
		e.docker.RemoveContainer(ctx, name)
	}
	for _, name := range runtime.AppNames(req.ProjectID, req.ServiceID, node.ContainerName) {
		if name == appName {
			continue
		}
		e.docker.RemoveContainer(ctx, name)
	}
	if err := e.store.SetNodePort(ctx, req.ProjectID, req.ServiceID, port); err != nil {
		return rollback(err)
	}
	url := e.appURLFor(ctx, node, port)

	node.Status = "running"
	node.Framework = strPtr(plan.Framework)
	node.URL = &url
	node.Port = &port
	node.ContainerName = &appName
	node.Error = nil
	node.Caption = display(plan.Framework, "Service")
	if err := e.store.UpsertNode(ctx, node); err != nil {
		return err
	}
	if err := e.store.RollupProject(ctx, req.ProjectID); err != nil {
		return err
	}
	dep.Status = "success"
	dep.Phase = "start"
	dep.Framework = node.Framework
	dep.URL = node.URL
	dep.Port = node.Port
	_ = e.store.FinishDeployment(ctx, dep)
	log("start", "success", "Live at "+url)
	e.publishStack(req.ProjectID, req.ServiceID, plan.Framework, node)
	e.hub.Publish(req.ProjectID, logs.Line{
		Kind:      "ready",
		Status:    "running",
		URL:       url,
		Text:      url,
		Framework: plan.Framework,
		NodeID:    req.ServiceID,
		Languages: toLogLanguages(node.Languages),
		Stack:     toLogStack(node.Stack),
	})
	return nil
}

func (e *Engine) publishStack(projectID, nodeID, framework string, node store.Node) {
	e.hub.Publish(projectID, logs.Line{
		Kind:      "stack",
		Status:    node.Status,
		Framework: framework,
		NodeID:    nodeID,
		Languages: toLogLanguages(node.Languages),
		Stack:     toLogStack(node.Stack),
		URL:       deref(node.URL),
	})
}

func (e *Engine) fail(ctx context.Context, node store.Node, dep store.Deployment, log func(string, string, string), err error) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return err
	}
	msg := err.Error()
	log("start", "error", msg)
	node.Status = "failed"
	node.Error = &msg
	_ = e.store.UpsertNode(ctx, node)
	_ = e.store.RollupProject(ctx, node.ProjectID)
	dep.Status = "failed"
	dep.Phase = "start"
	dep.Error = &msg
	_ = e.store.FinishDeployment(ctx, dep)
	e.hub.Publish(node.ProjectID, logs.Line{Kind: "status", Status: "failed", Phase: "start", Text: msg, NodeID: node.ID})
	return err
}

// failKeepingPrevious records a failed deploy while leaving the node marked as
// running. The previous container was never stopped, so the service really is
// still live on its old port and reporting it as failed would be a lie that
// also hides the working URL from the dashboard.
func (e *Engine) failKeepingPrevious(ctx context.Context, node store.Node, dep store.Deployment, log func(string, string, string), err error) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return err
	}
	msg := err.Error()
	log("start", "error", msg)

	node.Status = "running"
	rolledBack := msg + " (rolled back; previous version is still serving)"
	node.Error = &rolledBack
	_ = e.store.UpsertNode(ctx, node)
	_ = e.store.RollupProject(ctx, node.ProjectID)

	dep.Status = "failed"
	dep.Phase = "start"
	dep.Error = &rolledBack
	_ = e.store.FinishDeployment(ctx, dep)

	e.hub.Publish(node.ProjectID, logs.Line{
		Kind:   "status",
		Status: "running",
		Phase:  "start",
		Text:   rolledBack,
		URL:    deref(node.URL),
		NodeID: node.ID,
	})
	return err
}

// servingContainer returns the first container that is actually up, ignoring
// the candidate itself. An empty result means there is nothing to roll back to
// and a failure should be reported as a plain failure.
func (e *Engine) servingContainer(ctx context.Context, names []string, exclude string) string {
	for _, name := range names {
		if name == exclude {
			continue
		}
		if e.docker.Running(ctx, name) {
			return name
		}
	}
	return ""
}

// reservePort picks a free host port for a candidate container without writing
// it to the database. The reservation is held in memory and released by the
// returned function, so a deploy that fails never leaves a port marked as
// taken. The port is only persisted once the candidate passes its health check.
//
// The service's current port is deliberately treated as unavailable: the
// previous container is still bound to it and must keep serving until cutover.
func (e *Engine) reservePort(ctx context.Context, projectID, serviceID string) (int, func(), error) {
	return e.reservePortOn(ctx, "127.0.0.1")
}

func (e *Engine) reservePortOn(ctx context.Context, bind string) (int, func(), error) {
	e.portMu.Lock()
	defer e.portMu.Unlock()

	used := map[int]struct{}{}
	ports, err := e.store.UsedPorts(ctx)
	if err != nil {
		return 0, nil, err
	}
	for _, port := range ports {
		used[port] = struct{}{}
	}
	for _, port := range e.docker.PublishedHostPorts(ctx) {
		used[port] = struct{}{}
	}
	for port := range e.reserved {
		used[port] = struct{}{}
	}
	if bind == "" {
		bind = "127.0.0.1"
	}

	for port := e.cfg.PortMin; port <= e.cfg.PortMax; port++ {
		if _, taken := used[port]; taken {
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", bind, port))
		if err != nil {
			continue
		}
		_ = ln.Close()
		e.reserved[port] = struct{}{}
		return port, func() { e.releasePort(port) }, nil
	}
	return 0, nil, fmt.Errorf("no free publish ports in range %d-%d", e.cfg.PortMin, e.cfg.PortMax)
}

func (e *Engine) releasePort(port int) {
	e.portMu.Lock()
	delete(e.reserved, port)
	e.portMu.Unlock()
}

func (e *Engine) dockerRunTimeout() time.Duration {
	d := e.cfg.StartTimeout
	if d < 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

func (e *Engine) waitReady(ctx context.Context, port int, appName string, log func(string, string, string)) error {
	deadline := time.Now().Add(e.cfg.StartTimeout)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	last := time.Time{}
	crashed := 0
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		status := e.docker.ContainerStatus(ctx, appName)
		if status != "" && status != "running" && status != "created" {
			crashed++
			if crashed >= 2 {
				return fmt.Errorf("container %s before opening %s", status, addr)
			}
			time.Sleep(400 * time.Millisecond)
			continue
		}
		crashed = 0
		conn, err := net.DialTimeout("tcp", addr, 800*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if log != nil && time.Since(last) >= 5*time.Second {
			log("start", "info", "Waiting for "+addr)
			last = time.Now()
		}
		time.Sleep(400 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s", addr)
}

func (e *Engine) sidecarIndexFor(ctx context.Context, projectID, kind, nodeID string) int {
	nodes, err := e.store.ListNodes(ctx, projectID)
	if err != nil {
		return 0
	}
	n := 0
	for _, node := range nodes {
		if node.Kind != kind {
			continue
		}
		if node.ID == nodeID {
			return n
		}
		n++
	}
	return n
}

func (e *Engine) DestroyProject(ctx context.Context, projectID string) {
	nodes, _ := e.store.ListNodes(ctx, projectID)
	for _, node := range nodes {
		if node.Kind == "service" {
			for _, name := range runtime.AppNames(projectID, node.ID, node.ContainerName) {
				e.docker.RemoveContainer(ctx, name)
			}
			e.docker.RemoveImage(ctx, runtime.ImageName(projectID, node.ID))
		}
		if node.Kind == "postgresql" {
			e.StopSidecar(ctx, projectID, node.ID, "postgresql", node.ContainerName)
		}
		if node.Kind == "redis" {
			e.StopSidecar(ctx, projectID, node.ID, "redis", node.ContainerName)
		}
	}
	e.docker.RemoveContainer(ctx, runtime.AppName(projectID, "service:"+projectID))
	e.docker.RemoveContainer(ctx, "deploy-"+projectID+"-app")
	e.docker.RemoveContainer(ctx, runtime.LegacyPostgresName(projectID))
	e.docker.RemoveContainer(ctx, runtime.LegacyRedisName(projectID))
	e.docker.PurgeProject(ctx, projectID)
	_ = os.RemoveAll(filepath.Join(e.cfg.WorkDir, "projects", projectID))
}

func (e *Engine) StopService(ctx context.Context, projectID, serviceID string) error {
	node, err := e.store.GetNode(ctx, projectID, serviceID)
	if err != nil {
		return err
	}
	if node.Kind == "postgresql" || node.Kind == "redis" {
		if err := e.docker.Stop(ctx, e.sidecarName(node)); err != nil {
			return err
		}
		e.closeProxy(node.ID)
		node.Status = "stopped"
		_ = e.store.UpsertNode(ctx, node)
		return e.store.RollupProject(ctx, projectID)
	}
	if err := e.withApp(ctx, projectID, serviceID, e.docker.Stop); err != nil {
		return err
	}
	node.Status = "stopped"
	_ = e.store.UpsertNode(ctx, node)
	return e.store.RollupProject(ctx, projectID)
}

func (e *Engine) StartService(ctx context.Context, projectID, serviceID string) error {
	node, err := e.store.GetNode(ctx, projectID, serviceID)
	if err != nil {
		return err
	}
	if node.Kind == "postgresql" || node.Kind == "redis" {
		if err := e.docker.Start(ctx, e.sidecarName(node)); err != nil {
			return err
		}
		node.Status = "running"
		node.Error = nil
		_ = e.store.UpsertNode(ctx, node)
		_, _ = e.EnsureSidecar(ctx, projectID, serviceID)
		if latest, err := e.store.GetNode(ctx, projectID, serviceID); err == nil {
			_ = e.ApplyAlloc(ctx, latest)
		} else {
			_ = e.ApplyAlloc(ctx, node)
		}
		return e.store.RollupProject(ctx, projectID)
	}
	if err := e.withApp(ctx, projectID, serviceID, e.docker.Start); err != nil {
		return err
	}
	node.Status = "running"
	node.Error = nil
	_ = e.store.UpsertNode(ctx, node)
	_ = e.ApplyAlloc(ctx, node)
	return e.store.RollupProject(ctx, projectID)
}

func (e *Engine) RemoveService(ctx context.Context, projectID, serviceID string) {
	node, _ := e.store.GetNode(ctx, projectID, serviceID)
	for _, name := range runtime.AppNames(projectID, serviceID, node.ContainerName) {
		e.docker.RemoveContainer(ctx, name)
	}
	root := filepath.Join(e.cfg.WorkDir, "projects", projectID)
	_ = os.RemoveAll(filepath.Join(root, "services", runtime.DockerSafe(serviceID)))
	if serviceID == "service:"+projectID {
		_ = os.RemoveAll(filepath.Join(root, "src"))
	}
}

func (e *Engine) withApp(ctx context.Context, projectID, serviceID string, fn func(context.Context, string) error) error {
	node, _ := e.store.GetNode(ctx, projectID, serviceID)
	var last error
	for _, name := range runtime.AppNames(projectID, serviceID, node.ContainerName) {
		if err := fn(ctx, name); err == nil {
			return nil
		} else {
			last = err
		}
	}
	if last == nil {
		return fmt.Errorf("container not found")
	}
	return last
}

func (e *Engine) ApplyEnv(ctx context.Context, projectID, serviceID string) error {
	nodes, err := e.store.ListNodes(ctx, projectID)
	if err != nil {
		return err
	}
	var last error
	for _, node := range nodes {
		if node.Kind != "service" {
			continue
		}
		if serviceID != "" && node.ID != serviceID {
			continue
		}
		name := e.appName(ctx, projectID, node)
		info, err := e.docker.InspectApp(ctx, name)
		if err != nil {
			continue
		}
		port := info.ContainerPort
		if port == 0 {
			port = 8080
		}
		host := info.HostPort
		if host == 0 && node.Port != nil {
			host = *node.Port
		}
		env, _, err := e.runtimeEnv(ctx, projectID, node.ID, port)
		if err != nil {
			last = err
			continue
		}
		wasRunning := e.docker.Running(ctx, name)
		e.docker.RemoveContainer(ctx, name)
		runCtx, cancel := context.WithTimeout(ctx, e.dockerRunTimeout())
		err = e.docker.RunApp(runCtx, runtime.RunOpts{
			Name:          name,
			Image:         info.Image,
			Network:       runtime.NetworkName(projectID),
			Alias:         runtime.NetworkAlias(projectID, node.ID),
			ProjectID:     projectID,
			ServiceID:     node.ID,
			HostPort:      host,
			ContainerPort: port,
			Memory:        e.dockerMemory(node),
			StorageSize:   e.dockerStorage(node),
			CPUs:          e.cfg.CPULimit,
			Env:           env,
		})
		cancel()
		if err != nil {
			last = err
			continue
		}
		if !wasRunning {
			_ = e.docker.Stop(ctx, name)
		}
	}
	return last
}

func (e *Engine) runtimeEnv(ctx context.Context, projectID, serviceID string, containerPort int) (map[string]string, []string, error) {
	env := map[string]string{
		"PORT":            fmt.Sprintf("%d", containerPort),
		"HOST":            "0.0.0.0",
		"HOSTNAME":        "0.0.0.0",
		"ADDR":            fmt.Sprintf(":%d", containerPort),
		"APP_ENV":         "production",
		"NODE_ENV":        "production",
		"WEB_CONCURRENCY": "1",
		"ALLOWED_ORIGINS": "*",
		"CORS_ORIGINS":    "*",
	}
	vars, _ := e.store.ListVars(ctx, projectID, serviceID)
	for _, item := range vars {
		env[item.Key] = item.Value
	}
	env["PORT"] = fmt.Sprintf("%d", containerPort)
	env["HOST"] = "0.0.0.0"
	env["HOSTNAME"] = "0.0.0.0"
	env["ADDR"] = fmt.Sprintf(":%d", containerPort)
	env["NODE_ENV"] = "production"
	if node, err := e.store.GetNode(ctx, projectID, serviceID); err == nil {
		if public := e.appURLFor(ctx, node, 0); public != "" {
			env["HARBOR_PUBLIC_URL"] = public
		}
	}

	postgresLink, redisLink := e.sidecarLink(ctx, projectID, "postgresql"), e.sidecarLink(ctx, projectID, "redis")
	routed := pinSidecarURLs(env, postgresLink, redisLink)
	if keys := unreachablePrivateKeys(env); len(keys) > 0 {
		return nil, nil, privateMeshError(keys)
	}
	return env, routed, nil
}

func (e *Engine) seedDetectedVars(ctx context.Context, projectID, serviceID, dir string) {
	for _, pair := range envfile.Discover(dir) {
		if envfile.IsNoiseKey(pair.Key) {
			continue
		}
		_ = e.store.UpsertVar(ctx, projectID, store.EnvVar{
			ID:     ident.New("var_"),
			NodeID: serviceID,
			Key:    pair.Key,
			Value:  pair.Value,
			Source: "detected",
		})
	}
}

func (e *Engine) ensureAuthSecrets(ctx context.Context, projectID, serviceID string, log func(string, string, string)) error {
	vars, err := e.store.ListVars(ctx, projectID, serviceID)
	if err != nil {
		return err
	}
	byKey := map[string]store.EnvVar{}
	for _, item := range vars {
		byKey[item.Key] = item
	}
	secret := strings.TrimSpace(byKey["AUTH_SECRET"].Value)
	if secret == "" {
		secret = strings.TrimSpace(byKey["NEXTAUTH_SECRET"].Value)
	}
	if secret == "" {
		secret = envfile.RandomSecret()
	}
	for _, key := range []string{"AUTH_SECRET", "NEXTAUTH_SECRET"} {
		current := strings.TrimSpace(byKey[key].Value)
		if current != "" {
			continue
		}
		if err := e.store.UpsertVar(ctx, projectID, store.EnvVar{
			ID:     ident.New("var_"),
			NodeID: serviceID,
			Key:    key,
			Value:  secret,
			Source: "platform",
		}); err != nil {
			return err
		}
		log("build", "info", "Provisioned "+key+" for this service")
	}
	return nil
}

func (e *Engine) writeBuildEnv(dir, projectID, serviceID string) error {
	vars, err := e.store.ListVars(context.Background(), projectID, serviceID)
	if err != nil {
		return err
	}
	pairs := []envfile.Pair{{Key: "NODE_ENV", Value: "production"}}
	seen := map[string]struct{}{"NODE_ENV": {}}
	for _, item := range vars {
		if strings.TrimSpace(item.Value) == "" || envfile.IsNoiseKey(item.Key) {
			continue
		}
		if _, ok := seen[item.Key]; ok {
			continue
		}
		seen[item.Key] = struct{}{}
		pairs = append(pairs, envfile.Pair{Key: item.Key, Value: item.Value})
	}
	return envfile.Write(filepath.Join(dir, ".env.production"), pairs)
}

func SourceDir(cfg config.Config, projectID, serviceID string) string {
	return filepath.Join(cfg.WorkDir, "projects", projectID, "services", runtime.DockerSafe(serviceID), "src")
}

func languagesFrom(result detect.Result) []store.Language {
	merged := map[string]store.Language{}
	var order []string
	for _, item := range result.Languages {
		if !stackmeta.Keep(item.Name) {
			continue
		}
		name := stackmeta.Normalize(item.Name)
		if name == "" {
			continue
		}
		current, ok := merged[name]
		if !ok {
			order = append(order, name)
			merged[name] = store.Language{
				Name:       name,
				Percentage: item.Percentage,
				Bytes:      item.Bytes,
				Color:      stackmeta.Color(name),
			}
			continue
		}
		current.Percentage += item.Percentage
		current.Bytes += item.Bytes
		merged[name] = current
	}
	out := make([]store.Language, 0, len(order))
	for _, name := range order {
		out = append(out, merged[name])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Bytes == out[j].Bytes {
			return out[i].Percentage > out[j].Percentage
		}
		return out[i].Bytes > out[j].Bytes
	})
	var total float64
	for _, item := range out {
		total += item.Percentage
	}
	if total > 0 {
		for i := range out {
			out[i].Percentage = out[i].Percentage / total * 100
		}
	}
	return out
}

func languageSummary(langs []store.Language) string {
	if len(langs) == 0 {
		return ""
	}
	parts := make([]string, 0, 4)
	for i, lang := range langs {
		if i >= 4 {
			break
		}
		parts = append(parts, fmt.Sprintf("%s %.0f%%", lang.Name, lang.Percentage))
	}
	return " · " + strings.Join(parts, ", ")
}

func toLogLanguages(in []store.Language) []logs.Language {
	out := make([]logs.Language, 0, len(in))
	for _, item := range in {
		out = append(out, logs.Language{
			Name:       item.Name,
			Percentage: item.Percentage,
			Bytes:      item.Bytes,
			Color:      item.Color,
		})
	}
	return out
}

func toLogStack(in []store.StackItem) []logs.StackItem {
	out := make([]logs.StackItem, 0, len(in))
	for _, item := range in {
		out = append(out, logs.StackItem{
			Name:  item.Name,
			Kind:  item.Kind,
			Color: item.Color,
		})
	}
	return out
}

func stackFrom(result detect.Result, plan pack.Plan) []store.StackItem {
	seen := map[string]struct{}{}
	var out []store.StackItem
	add := func(name, kind string) {
		if kind == "infra" || !stackmeta.Keep(name) {
			return
		}
		label := stackmeta.Normalize(name)
		if label == "" {
			return
		}
		key := strings.ToLower(kind + ":" + label)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, store.StackItem{Name: label, Kind: kind, Color: stackmeta.Color(name)})
	}
	for _, lang := range result.Languages {
		add(lang.Name, "language")
	}
	for _, app := range result.Apps {
		if app.Language != "" {
			add(app.Language, "language")
		}
		if app.Runtime != nil {
			add(app.Runtime.Name, "runtime")
		}
		if app.Framework != nil {
			add(app.Framework.Name, "framework")
		}
		for _, db := range app.Databases {
			add(db.Name, "database")
		}
	}
	for _, tech := range result.Infra {
		add(tech.Name, "infra")
	}
	if plan.Framework != "" {
		add(plan.Framework, "framework")
	}
	if plan.Runtime != "" {
		add(plan.Runtime, "runtime")
	}
	return out
}

func strPtr(v string) *string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return &v
}

func display(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func detectLine(plan pack.Plan, langs []store.Language) string {
	label := display(plan.Framework, "stack")
	if strings.TrimSpace(plan.RuntimeVersion) != "" {
		label += " " + plan.RuntimeVersion
	}
	return "Detected " + label + languageSummary(langs)
}

func PrepareDir(cfg config.Config, projectID, serviceID string) (string, error) {
	dir := SourceDir(cfg, projectID, serviceID)
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func (e *Engine) appName(ctx context.Context, projectID string, node store.Node) string {
	for _, name := range runtime.AppNames(projectID, node.ID, node.ContainerName) {
		if _, err := e.docker.InspectApp(ctx, name); err == nil {
			return name
		}
	}
	return runtime.AppName(projectID, node.ID)
}

func serviceTitle(sourceType, sourceRef, fallback string) string {
	ref := strings.TrimSpace(sourceRef)
	if ref == "" {
		return fallback
	}
	if sourceType == "github" {
		ref = strings.TrimSuffix(ref, ".git")
		parts := strings.Split(ref, "/")
		if name := strings.TrimSpace(parts[len(parts)-1]); name != "" {
			return name
		}
	}
	base := filepath.Base(ref)
	name := strings.TrimSuffix(base, filepath.Ext(base))
	if name != "" {
		return name
	}
	return fallback
}

func (e *Engine) appURLFor(ctx context.Context, node store.Node, port int) string {
	return edge.AppURL(e.cfg, node.ID, port, e.appNames(ctx, node)...)
}

func (e *Engine) appNames(ctx context.Context, node store.Node) []string {
	nodes, err := e.store.ListServiceNodes(ctx)
	if err != nil {
		return []string{node.Title}
	}
	named := make([]edge.Named, 0, len(nodes)+1)
	found := false
	for _, item := range nodes {
		named = append(named, edge.Named{ID: item.ID, Title: item.Title})
		if item.ID == node.ID {
			found = true
		}
	}
	if !found {
		named = append(named, edge.Named{ID: node.ID, Title: node.Title})
	}
	return []string{edge.HostSlug(node.ID, node.Title, named)}
}

func deref(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

var ParseGitHub = ingest.ParseGitHub
