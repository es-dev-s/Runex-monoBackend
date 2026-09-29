package deploy

import (
	"context"
	"fmt"
	"net"
	"time"

	"deploy_platform/internal/logs"
	"deploy_platform/internal/runtime"
	"deploy_platform/internal/store"
)

func (e *Engine) Recover(ctx context.Context) {
	e.RestoreSidecarProxies(ctx)
	e.syncRunningAlloc(ctx)
	e.reclaimLive(ctx)
	nodes, err := e.store.ListServicesByStatus(ctx, "building")
	if err != nil {
		return
	}
	for _, node := range nodes {
		e.reclaimBuilding(ctx, node, "Deploy was interrupted. Click Deploy to retry.")
	}
}

func (e *Engine) syncRunningAlloc(ctx context.Context) {
	nodes, err := e.store.ListAllNodes(ctx)
	if err != nil {
		return
	}
	for _, node := range nodes {
		if node.Status != "running" {
			continue
		}
		_ = e.ApplyAlloc(ctx, node)
	}
}

func (e *Engine) Watch(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	e.reapStale(ctx)
	e.reclaimLive(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.reapStale(ctx)
			e.reclaimLive(ctx)
		}
	}
}

func (e *Engine) CancelDeploy(node store.Node) {
	e.Jobs.Cancel(node.ID)
	e.markInterrupted(node, "Deploy cancelled")
}

func (e *Engine) jobLimit() time.Duration {
	return e.cfg.BuildTimeout + e.cfg.StartTimeout + 2*time.Minute
}

func (e *Engine) reapStale(ctx context.Context) {
	nodes, err := e.store.ListServicesByStatus(ctx, "building")
	if err != nil {
		return
	}
	limit := e.jobLimit()
	for _, node := range nodes {
		if !e.Jobs.Busy(node.ID) {
			e.reclaimBuilding(ctx, node, "Deploy stalled with no active job. Click Deploy to retry.")
			continue
		}
		age, ok := e.Jobs.Age(node.ID)
		if ok && age > limit {
			e.Jobs.Cancel(node.ID)
			e.markInterrupted(node, "Deploy timed out. Click Deploy to retry.")
		}
	}
}

func (e *Engine) reclaimLive(ctx context.Context) {
	e.healBusyNodes(ctx)
	for _, status := range []string{"failed", "stopped", "building"} {
		nodes, err := e.store.ListServicesByStatus(ctx, status)
		if err != nil {
			continue
		}
		for _, node := range nodes {
			// Prior container often stays up during a rebuild; never paint green mid-job.
			if e.Jobs != nil && e.Jobs.Busy(node.ID) {
				continue
			}
			e.promoteIfContainerLive(ctx, node)
		}
	}
}

// healBusyNodes restores status=building when an active job drifted to running/failed
// because the previous container was still healthy (blue/green style redeploy).
func (e *Engine) healBusyNodes(ctx context.Context) {
	if e.Jobs == nil {
		return
	}
	active := e.Jobs.ActiveIDs()
	if len(active) == 0 {
		return
	}
	want := make(map[string]struct{}, len(active))
	for _, id := range active {
		want[id] = struct{}{}
	}
	nodes, err := e.store.ListAllNodes(ctx)
	if err != nil {
		return
	}
	for _, node := range nodes {
		if _, ok := want[node.ID]; !ok {
			continue
		}
		if node.Kind != "service" || node.Status == "building" {
			continue
		}
		e.forceBuilding(ctx, node)
	}
}

func (e *Engine) forceBuilding(ctx context.Context, node store.Node) {
	started := time.Now().UTC()
	if age, ok := e.Jobs.Age(node.ID); ok {
		started = started.Add(-age)
	}
	node.Status = "building"
	node.Error = nil
	_ = e.store.UpsertNode(ctx, node)
	_ = e.store.RollupProject(ctx, node.ProjectID)
	line := logs.Line{
		Kind:   "status",
		Status: "building",
		Phase:  "build",
		NodeID: node.ID,
	}
	t := started
	line.StartedAt = &t
	e.hub.Publish(node.ProjectID, line)
}

func (e *Engine) promoteIfContainerLive(ctx context.Context, node store.Node) {
	if e.Jobs != nil && e.Jobs.Busy(node.ID) {
		return
	}
	for _, name := range runtime.AppNames(node.ProjectID, node.ID, node.ContainerName) {
		if !e.docker.Running(ctx, name) {
			continue
		}
		info, err := e.docker.InspectApp(ctx, name)
		if err != nil || info.HostPort <= 0 || !portOpen(info.HostPort) {
			continue
		}
		node.Status = "running"
		node.Error = nil
		node.ContainerName = &name
		url := e.appURLFor(ctx, node, info.HostPort)
		node.URL = &url
		port := info.HostPort
		node.Port = &port
		_ = e.store.UpsertNode(ctx, node)
		_ = e.store.RollupProject(ctx, node.ProjectID)
		e.hub.Publish(node.ProjectID, logs.Line{
			Kind:   "status",
			Status: "running",
			Phase:  "start",
			URL:    url,
			NodeID: node.ID,
		})
		return
	}
}

func (e *Engine) reclaimBuilding(ctx context.Context, node store.Node, msg string) {
	if e.Jobs != nil && e.Jobs.Busy(node.ID) {
		return
	}
	for _, name := range runtime.AppNames(node.ProjectID, node.ID, node.ContainerName) {
		if !e.docker.Running(ctx, name) {
			continue
		}
		info, err := e.docker.InspectApp(ctx, name)
		if err != nil {
			continue
		}
		if info.HostPort > 0 && !portOpen(info.HostPort) {
			e.docker.RemoveContainer(ctx, name)
			e.markInterrupted(node, "Deploy stalled: the process started but never became reachable. Click Deploy to retry.")
			return
		}
		node.Status = "running"
		node.Error = nil
		node.ContainerName = &name
		if info.HostPort > 0 {
			url := e.appURLFor(ctx, node, info.HostPort)
			node.URL = &url
			port := info.HostPort
			node.Port = &port
		}
		_ = e.store.UpsertNode(ctx, node)
		_ = e.store.RollupProject(ctx, node.ProjectID)
		e.hub.Publish(node.ProjectID, logs.Line{
			Kind:   "status",
			Status: "running",
			Phase:  "start",
			URL:    deref(node.URL),
			NodeID: node.ID,
		})
		return
	}
	e.markInterrupted(node, msg)
}

func portOpen(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 800*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func (e *Engine) markInterrupted(node store.Node, msg string) {
	bg := context.Background()
	node.Status = "failed"
	node.Error = &msg
	_ = e.store.UpsertNode(bg, node)
	_ = e.store.RollupProject(bg, node.ProjectID)
	if id, err := e.store.OpenDeploymentID(bg, node.ProjectID, node.ID); err == nil && id != "" {
		_ = e.store.AppendLog(bg, store.LogLine{
			ProjectID:    node.ProjectID,
			DeploymentID: id,
			NodeID:       node.ID,
			TS:           time.Now().UTC(),
			Phase:        "start",
			Level:        "error",
			Text:         msg,
		})
	}
	_ = e.store.FailOpenDeployments(bg, node.ProjectID, msg)
	e.hub.Publish(node.ProjectID, logs.Line{
		Kind:   "log",
		Phase:  "start",
		Level:  "error",
		Text:   msg,
		NodeID: node.ID,
	})
	e.hub.Publish(node.ProjectID, logs.Line{
		Kind:   "status",
		Status: "failed",
		Phase:  "start",
		Text:   msg,
		NodeID: node.ID,
	})
}
