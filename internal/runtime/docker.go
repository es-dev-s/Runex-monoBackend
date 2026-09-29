package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Client struct {
	mu       sync.Mutex
	cacheMu  sync.RWMutex
	inspects map[string]inspectEntry
	statuses map[string]statusEntry
}

type inspectEntry struct {
	env     map[string]string
	ip      string
	expires time.Time
}

// inspectTTL bounds how long a container's env and address are reused. Both are
// fixed for the life of a container, so a short window removes a docker CLI
// call from every database read without serving a stale address after a restart.
const inspectTTL = 20 * time.Second

type statusEntry struct {
	status  string
	expires time.Time
}

// statusTTL is far shorter than inspectTTL because status is the one thing that
// changes on its own: a container can exit at any moment. Real transitions are
// pushed to the dashboard over the log stream, so this window only collapses
// the repeated polls behind a single screen, each of which would otherwise
// spawn a docker process and cost around 25ms.
const statusTTL = 2 * time.Second

func (c *Client) cachedStatus(name string) (string, bool) {
	c.cacheMu.RLock()
	entry, ok := c.statuses[name]
	c.cacheMu.RUnlock()
	if !ok || time.Now().After(entry.expires) {
		return "", false
	}
	return entry.status, true
}

func (c *Client) storeStatus(name, status string) {
	c.cacheMu.Lock()
	if c.statuses == nil {
		c.statuses = map[string]statusEntry{}
	}
	c.statuses[name] = statusEntry{status: status, expires: time.Now().Add(statusTTL)}
	c.cacheMu.Unlock()
}

func New() *Client {
	return &Client{inspects: map[string]inspectEntry{}, statuses: map[string]statusEntry{}}
}

func (c *Client) cachedInspect(name string) (inspectEntry, bool) {
	c.cacheMu.RLock()
	entry, ok := c.inspects[name]
	c.cacheMu.RUnlock()
	if !ok || time.Now().After(entry.expires) {
		return inspectEntry{}, false
	}
	return entry, true
}

func (c *Client) storeInspect(name string, mutate func(*inspectEntry)) {
	c.cacheMu.Lock()
	if c.inspects == nil {
		c.inspects = map[string]inspectEntry{}
	}
	entry := c.inspects[name]
	if time.Now().After(entry.expires) {
		entry = inspectEntry{}
	}
	mutate(&entry)
	entry.expires = time.Now().Add(inspectTTL)
	c.inspects[name] = entry
	c.cacheMu.Unlock()
}

// ForgetContainer drops cached inspect data. Call it whenever a container is
// created, started, stopped, or removed.
func (c *Client) ForgetContainer(name string) {
	c.cacheMu.Lock()
	delete(c.inspects, name)
	delete(c.statuses, name)
	c.cacheMu.Unlock()
}

func (c *Client) EnsureNetwork(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := c.run(ctx, "network", "inspect", name); err == nil {
		return nil
	}
	return c.run(ctx, "network", "create",
		"--driver", "bridge",
		"--internal=false",
		"--opt", "com.docker.network.bridge.enable_icc=true",
		"--label", "deploy.platform=1",
		"--label", "deploy.network="+name,
		name,
	)
}

func (c *Client) RemoveNetwork(ctx context.Context, name string) {
	rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	_ = c.run(rmCtx, "network", "rm", name)
}

func (c *Client) Build(ctx context.Context, tag, dir string, log func(string)) error {
	cmd := dockerCmd(ctx, "build",
		"--pull=false",
		"--network=default",
		"-t", tag, dir)
	return c.stream(cmd, log)
}

func (c *Client) RemoveContainer(ctx context.Context, name string) {
	c.ForgetContainer(name)
	rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	_ = c.run(rmCtx, "rm", "-f", name)
}

func (c *Client) RemoveImage(ctx context.Context, tag string) {
	rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	_ = c.run(rmCtx, "rmi", "-f", tag)
}

func (c *Client) PurgeProject(ctx context.Context, projectID string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
	defer cancel()
	ids, _ := c.output(ctx, "ps", "-aq", "--filter", "label=deploy.project="+projectID)
	for _, id := range strings.Fields(ids) {
		c.RemoveContainer(ctx, id)
	}
	images, _ := c.output(ctx, "images", "--format", "{{.Repository}}:{{.Tag}}")
	for _, line := range strings.Split(images, "\n") {
		tag := strings.TrimSpace(line)
		if IsProjectImage(tag, projectID) {
			c.RemoveImage(ctx, tag)
		}
	}
	c.RemoveNetwork(ctx, NetworkName(projectID))
}

func IsProjectImage(tag, projectID string) bool {
	tag = strings.TrimSpace(tag)
	if tag == "" || strings.HasPrefix(tag, "<none>") {
		return false
	}
	return strings.HasPrefix(tag, "deploy-"+projectID+"-")
}

func (c *Client) RunApp(ctx context.Context, opts RunOpts) error {
	c.ForgetContainer(opts.Name)
	args := appArgs(opts, true)
	err := c.run(ctx, args...)
	if err != nil && opts.StorageSize != "" && storageOptUnsupported(err) {
		err = c.run(ctx, appArgs(opts, false)...)
	}
	c.ForgetContainer(opts.Name)
	return err
}

func appArgs(opts RunOpts, withStorage bool) []string {
	args := []string{
		"run", "-d",
		"--name", opts.Name,
		"--network", opts.Network,
		"--network-alias", aliasOr(opts.Alias, "app"),
		"--restart", "unless-stopped",
		"--init",
		"--memory", opts.Memory,
		"--memory-swap", opts.Memory,
		"--cpus", opts.CPUs,
		"--pids-limit", "1024",
		"--security-opt", "no-new-privileges:true",
		"--cap-drop", "ALL",
		"--cap-add", "CHOWN",
		"--cap-add", "SETUID",
		"--cap-add", "SETGID",
		"--cap-add", "NET_BIND_SERVICE",
		// --init runs tini as pid 1, and tini has to forward SIGTERM to a child
		// that has usually dropped to an unprivileged user. Signalling across
		// uids needs CAP_KILL, and without it tini exits fatally on every stop
		// instead of letting the process shut down.
		"--cap-add", "KILL",
		"--label", "deploy.platform=1",
		"--label", "deploy.project=" + opts.ProjectID,
		"--label", "deploy.role=app",
		"--label", "deploy.service=" + opts.ServiceID,
		"--label", "deploy.node=" + opts.ServiceID,
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", opts.HostPort, opts.ContainerPort),
	}
	if withStorage && strings.TrimSpace(opts.StorageSize) != "" {
		args = append(args, "--storage-opt", "size="+opts.StorageSize)
	}
	for key, value := range opts.Env {
		args = append(args, "-e", key+"="+value)
	}
	return append(args, opts.Image)
}

func (c *Client) RunSidecar(ctx context.Context, opts SidecarOpts) error {
	c.RemoveContainer(ctx, opts.Name)
	args := sidecarArgs(opts)
	err := c.run(ctx, args...)
	c.ForgetContainer(opts.Name)
	return err
}

func sidecarArgs(opts SidecarOpts) []string {
	cpus := opts.CPUs
	if cpus == "" {
		cpus = "0.5"
	}
	args := []string{
		"run", "-d",
		"--name", opts.Name,
		"--network", opts.Network,
		"--network-alias", opts.Alias,
		"--restart", "unless-stopped",
		"--init",
		"--memory", opts.Memory,
		"--memory-swap", opts.Memory,
		"--cpus", cpus,
		"--pids-limit", "128",
		"--security-opt", "no-new-privileges:true",
		"--cap-drop", "ALL",
		"--cap-add", "CHOWN",
		"--cap-add", "SETUID",
		"--cap-add", "SETGID",
		"--cap-add", "FOWNER",
		"--cap-add", "DAC_OVERRIDE",
		// tini forwards SIGTERM to postgres or redis, which run as their own
		// user. Without CAP_KILL that forward fails and tini aborts, so the
		// database is killed rather than shut down and has to run crash
		// recovery the next time it starts.
		"--cap-add", "KILL",
		"--label", "deploy.platform=1",
		"--label", "deploy.project=" + opts.ProjectID,
		"--label", "deploy.role=" + opts.Role,
		"--label", "deploy.node=" + opts.NodeID,
	}
	for key, value := range opts.Env {
		args = append(args, "-e", key+"="+value)
	}
	if opts.Volume != "" {
		args = append(args, "-v", opts.Volume)
	}
	if opts.HostPort > 0 {
		bind := opts.Bind
		if bind == "" {
			bind = "0.0.0.0"
		}
		inner := opts.ContainerPort
		if inner <= 0 {
			inner = 5432
		}
		args = append(args, "-p", fmt.Sprintf("%s:%d:%d", bind, opts.HostPort, inner))
	}
	args = append(args, opts.Image)
	args = append(args, opts.Cmd...)
	return args
}

func storageOptUnsupported(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "storage-opt") ||
		(strings.Contains(msg, "overlay2") && strings.Contains(msg, "quota")) ||
		strings.Contains(msg, "does not support quota")
}

func (c *Client) Logs(ctx context.Context, name string, n int) string {
	if n <= 0 {
		n = 80
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	out, err := c.output(ctx, "logs", "--tail", fmt.Sprintf("%d", n), name)
	if err != nil {
		return strings.TrimSpace(err.Error())
	}
	return strings.TrimSpace(out)
}

func (c *Client) Running(ctx context.Context, name string) bool {
	return c.ContainerStatus(ctx, name) == "running"
}

func (c *Client) ContainerStatus(ctx context.Context, name string) string {
	if status, ok := c.cachedStatus(name); ok {
		return status
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out, err := c.output(ctx, "inspect", "-f", "{{.State.Status}}", name)
	if err != nil {
		return ""
	}
	status := strings.TrimSpace(out)
	c.storeStatus(name, status)
	return status
}

func (c *Client) Stop(ctx context.Context, name string) error {
	c.ForgetContainer(name)
	stopCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return c.run(stopCtx, "stop", name)
}

func (c *Client) Start(ctx context.Context, name string) error {
	c.ForgetContainer(name)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	err := c.run(ctx, "start", name)
	c.ForgetContainer(name)
	return err
}

type AppInspect struct {
	Image         string
	HostPort      int
	ContainerPort int
}

func (c *Client) InspectApp(ctx context.Context, name string) (AppInspect, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	raw, err := c.output(ctx, "inspect", "--format", "{{json .}}", name)
	if err != nil {
		return AppInspect{}, fmt.Errorf("container not found")
	}
	var info struct {
		Config struct {
			Image string `json:"Image"`
		} `json:"Config"`
		HostConfig struct {
			PortBindings map[string][]struct {
				HostPort string `json:"HostPort"`
			} `json:"PortBindings"`
		} `json:"HostConfig"`
	}
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		return AppInspect{}, err
	}
	out := AppInspect{Image: info.Config.Image}
	for port, binds := range info.HostConfig.PortBindings {
		num := 0
		fmt.Sscanf(strings.Split(port, "/")[0], "%d", &num)
		if num > 0 {
			out.ContainerPort = num
		}
		if len(binds) > 0 {
			fmt.Sscanf(binds[0].HostPort, "%d", &out.HostPort)
		}
	}
	if out.Image == "" {
		return AppInspect{}, fmt.Errorf("missing image")
	}
	return out, nil
}

type RunOpts struct {
	Name          string
	Image         string
	Network       string
	Alias         string
	ProjectID     string
	ServiceID     string
	HostPort      int
	ContainerPort int
	Memory        string
	StorageSize   string
	CPUs          string
	Env           map[string]string
}

type SidecarOpts struct {
	Name          string
	Image         string
	Network       string
	Alias         string
	ProjectID     string
	NodeID        string
	Role          string
	Memory        string
	CPUs          string
	Env           map[string]string
	Bind          string
	HostPort      int
	ContainerPort int
	Volume        string
	Cmd           []string
}

func (c *Client) run(ctx context.Context, args ...string) error {
	cmd := dockerCmd(ctx, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &bytes.Buffer{}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("docker %s timed out", args[0])
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("docker %s: %s", args[0], msg)
	}
	return nil
}

func (c *Client) output(ctx context.Context, args ...string) (string, error) {
	cmd := dockerCmd(ctx, args...)
	out, err := cmd.Output()
	if err != nil && ctx.Err() != nil {
		return string(out), fmt.Errorf("docker %s timed out", args[0])
	}
	return string(out), err
}

func dockerCmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.WaitDelay = 3 * time.Second
	return cmd
}

func (c *Client) stream(cmd *exec.Cmd, log func(string)) error {
	cmd.WaitDelay = 3 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	var wg sync.WaitGroup
	wg.Add(2)
	pump := func(r io.Reader) {
		defer wg.Done()
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line != "" && log != nil {
				log(line)
			}
		}
	}
	go pump(stdout)
	go pump(stderr)
	wg.Wait()
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("docker build failed")
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func NetworkName(projectID string) string { return "deploy-" + projectID }

func AppName(projectID, serviceID string) string {
	safe := DockerSafe(serviceID)
	if safe == "" {
		safe = "svc"
	}
	return "deploy-" + projectID + "-" + safe
}

func ImageName(projectID, serviceID string) string {
	return AppName(projectID, serviceID) + ":latest"
}

// ReleaseName names a candidate container for one deploy attempt. Candidates
// carry a per-release suffix so a new version can be started and health
// checked while the previous one keeps serving traffic.
func ReleaseName(projectID, serviceID, release string) string {
	release = DockerSafe(strings.TrimSpace(release))
	if release == "" {
		return AppName(projectID, serviceID)
	}
	return AppName(projectID, serviceID) + "-" + truncate(release, 12)
}

// ServiceContainers lists every container Docker still holds for one service,
// newest first. Cleanup is driven off labels rather than name guesses so a
// candidate orphaned by a crash mid-cutover is still reclaimed.
func (c *Client) ServiceContainers(ctx context.Context, projectID, serviceID string) []string {
	out, err := c.output(ctx, "ps", "-a", "--format", "{{.Names}}",
		"--filter", "label=deploy.project="+projectID,
		"--filter", "label=deploy.service="+serviceID,
		"--filter", "label=deploy.role=app",
	)
	if err != nil {
		return nil
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func NetworkAlias(projectID, serviceID string) string {
	safe := DockerSafe(serviceID)
	if safe == "" {
		return "svc"
	}
	return safe
}

func AppNames(projectID, serviceID string, stored *string) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	if stored != nil {
		add(*stored)
	}
	add(AppName(projectID, serviceID))
	if serviceID == "" || serviceID == "service:"+projectID {
		add("deploy-" + projectID + "-app")
	}
	return out
}

func DockerSafe(id string) string {
	replacer := strings.NewReplacer(":", "-", "/", "-", "_", "-")
	return strings.ToLower(replacer.Replace(id))
}

func aliasOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func SidecarName(projectID, nodeID, role string) string {
	safe := DockerSafe(nodeID)
	if safe == "" {
		safe = role
	}
	return "deploy-" + projectID + "-" + safe + "-" + role
}

func PostgresName(projectID, nodeID string) string {
	return SidecarName(projectID, nodeID, "postgres")
}

func RedisName(projectID, nodeID string) string {
	return SidecarName(projectID, nodeID, "redis")
}

func LegacyPostgresName(projectID string) string { return "deploy-" + projectID + "-postgres" }
func LegacyRedisName(projectID string) string    { return "deploy-" + projectID + "-redis" }

func SidecarAlias(kind, nodeID string, index int) string {
	if index == 0 {
		if kind == "postgresql" {
			return "postgres"
		}
		if kind == "redis" {
			return "redis"
		}
	}
	return DockerSafe(nodeID)
}

func VolumeName(projectID, nodeID, role string) string {
	return SidecarName(projectID, nodeID, role+"-data")
}

func VolumeFor(nodeKind, projectID, nodeID string) string {
	switch nodeKind {
	case "postgresql":
		return VolumeName(projectID, nodeID, "pg")
	case "redis":
		return VolumeName(projectID, nodeID, "redis")
	default:
		return ""
	}
}

func (c *Client) EnsureVolume(ctx context.Context, name, projectID, nodeID, role string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := c.run(ctx, "volume", "inspect", name); err == nil {
		return nil
	}
	return c.run(ctx, "volume", "create",
		"--label", "deploy.platform=1",
		"--label", "deploy.project="+projectID,
		"--label", "deploy.node="+nodeID,
		"--label", "deploy.role="+role,
		name,
	)
}

func (c *Client) UpdateMemory(ctx context.Context, name, memory string) error {
	name = strings.TrimSpace(name)
	memory = strings.TrimSpace(memory)
	if name == "" || memory == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return c.run(ctx, "update", "--memory", memory, "--memory-swap", memory, name)
}

func (c *Client) MemoryLimitBytes(ctx context.Context, name string) int64 {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out, err := c.output(ctx, "inspect", "-f", "{{.HostConfig.Memory}}", name)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func (c *Client) RedisSetMaxMemory(ctx context.Context, name, password, maxmemory string) error {
	name = strings.TrimSpace(name)
	maxmemory = strings.TrimSpace(maxmemory)
	if name == "" || maxmemory == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	args := []string{"exec"}
	if strings.TrimSpace(password) != "" {
		args = append(args, "-e", "REDISCLI_AUTH="+password)
	}
	args = append(args, name, "redis-cli", "CONFIG", "SET", "maxmemory", maxmemory)
	return c.run(ctx, args...)
}

// VolumeExists reports whether a data volume is already present. A Postgres
// data directory keeps the credentials it was initialised with, so callers use
// this to tell a first boot from a container being recreated over real data.
func (c *Client) VolumeExists(ctx context.Context, name string) bool {
	if strings.TrimSpace(name) == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return c.run(ctx, "volume", "inspect", name) == nil
}

func (c *Client) RemoveVolume(ctx context.Context, name string) {
	if strings.TrimSpace(name) == "" {
		return
	}
	rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	_ = c.run(rmCtx, "volume", "rm", "-f", name)
}

func (c *Client) ContainerIPv4(ctx context.Context, name string) (string, error) {
	if entry, ok := c.cachedInspect(name); ok && entry.ip != "" {
		return entry.ip, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out, err := c.output(ctx, "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}", name)
	if err != nil {
		return "", err
	}
	for _, part := range strings.Fields(out) {
		if ip := strings.TrimSpace(part); ip != "" {
			c.storeInspect(name, func(entry *inspectEntry) { entry.ip = ip })
			return ip, nil
		}
	}
	return "", fmt.Errorf("container has no address")
}

func (c *Client) ContainerEnv(ctx context.Context, name string) map[string]string {
	if entry, ok := c.cachedInspect(name); ok && entry.env != nil {
		return entry.env
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out, err := c.output(ctx, "inspect", "-f", "{{range .Config.Env}}{{println .}}{{end}}", name)
	if err != nil {
		return nil
	}
	env := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && key != "" {
			env[key] = value
		}
	}
	c.storeInspect(name, func(entry *inspectEntry) { entry.env = env })
	return env
}

func (c *Client) PublishedPort(ctx context.Context, name string) int {
	info, err := c.InspectApp(ctx, name)
	if err != nil {
		return 0
	}
	return info.HostPort
}

func (c *Client) PublishedHostPorts(ctx context.Context) []int {
	out, err := c.output(ctx, "ps", "--format", "{{.Ports}}", "--filter", "label=deploy.platform=1")
	if err != nil {
		return nil
	}
	var ports []int
	for _, line := range strings.Split(out, "\n") {
		for _, part := range strings.Split(line, ",") {
			part = strings.TrimSpace(part)
			host, _, ok := strings.Cut(part, "->")
			if !ok {
				continue
			}
			if _, port, found := strings.Cut(host, ":"); found {
				if n, err := strconv.Atoi(strings.TrimSpace(port)); err == nil && n > 0 {
					ports = append(ports, n)
				}
			}
		}
	}
	return ports
}
