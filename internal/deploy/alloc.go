package deploy

import (
	"context"
	"fmt"
	"strings"

	"deploy_platform/internal/alloc"
	"deploy_platform/internal/logs"
	"deploy_platform/internal/runtime"
	"deploy_platform/internal/store"
)

func (e *Engine) resolveAlloc(node store.Node) (memoryMB, diskMB int) {
	return alloc.Resolve(node.Kind, node.MemoryMB, node.DiskMB, e.cfg.MaxMemoryMB, e.cfg.MaxDiskMB)
}

func (e *Engine) dockerMemory(node store.Node) string {
	mem, _ := e.resolveAlloc(node)
	return alloc.DockerMemory(mem)
}

func (e *Engine) dockerStorage(node store.Node) string {
	_, disk := e.resolveAlloc(node)
	return alloc.StorageOpt(disk)
}

func (e *Engine) allocContainers(node store.Node) []string {
	if node.Kind == "postgresql" || node.Kind == "redis" {
		name := e.sidecarName(node)
		if node.ContainerName != nil && strings.TrimSpace(*node.ContainerName) != "" && *node.ContainerName != name {
			return []string{name, *node.ContainerName}
		}
		return []string{name}
	}
	return runtime.AppNames(node.ProjectID, node.ID, node.ContainerName)
}

func (e *Engine) DiskUsed(ctx context.Context, node store.Node) int64 {
	container := ""
	if node.ContainerName != nil {
		container = *node.ContainerName
	}
	return e.docker.DiskUsed(ctx, node.Kind, node.ProjectID, node.ID, container)
}

func (e *Engine) RAMUsed(ctx context.Context, node store.Node) int64 {
	stats, err := e.docker.Stats(ctx)
	if err != nil || stats == nil {
		return 0
	}
	var best int64
	for _, name := range e.allocContainers(node) {
		if st, ok := stats[name]; ok && st.RAMUsed > best {
			best = st.RAMUsed
		}
	}
	return best
}

func (e *Engine) ApplyAlloc(ctx context.Context, node store.Node) error {
	memMB, _ := e.resolveAlloc(node)
	mem := alloc.DockerMemory(memMB)
	want := alloc.BytesFromMB(memMB)
	var last error
	touched := false
	for _, name := range e.allocContainers(node) {
		if e.docker.ContainerStatus(ctx, name) == "" {
			continue
		}
		touched = true
		if err := e.docker.UpdateMemory(ctx, name, mem); err != nil {
			last = err
			continue
		}
		got := e.docker.MemoryLimitBytes(ctx, name)
		if got > 0 && abs64(got-want) > 2*1024*1024 {
			last = fmt.Errorf("container memory is %s, wanted %s", alloc.DockerMemory(int(got/1024/1024)), mem)
		}
	}
	if node.Kind == "redis" {
		if err := e.applyRedisMaxMemory(ctx, node); err != nil && last == nil {
			last = err
		}
	}
	if !touched {
		return nil
	}
	return last
}

func (e *Engine) applyRedisMaxMemory(ctx context.Context, node store.Node) error {
	name := e.sidecarName(node)
	if !e.docker.Running(ctx, name) {
		return nil
	}
	mem, _ := e.resolveAlloc(node)
	password := e.docker.ContainerEnv(ctx, name)["REDIS_PASSWORD"]
	return e.docker.RedisSetMaxMemory(ctx, name, password, alloc.RedisMaxMemory(mem))
}

func (e *Engine) StopOverLimit(ctx context.Context, node store.Node, reason string) {
	if node.Status != "running" {
		return
	}
	if err := e.StopService(ctx, node.ProjectID, node.ID); err != nil {
		e.hub.Publish(node.ProjectID, logs.Line{
			Kind:   "log",
			Phase:  "start",
			Level:  "error",
			Text:   "Could not stop this node after it exceeded its allocation.",
			NodeID: node.ID,
		})
		return
	}
	updated, err := e.store.GetNode(ctx, node.ProjectID, node.ID)
	if err != nil {
		updated = node
	}
	updated.Status = "stopped"
	updated.Error = &reason
	_ = e.store.UpsertNode(ctx, updated)
	_ = e.store.RollupProject(ctx, node.ProjectID)
	e.hub.Publish(node.ProjectID, logs.Line{
		Kind:   "log",
		Phase:  "start",
		Level:  "error",
		Text:   reason,
		NodeID: node.ID,
	})
	e.hub.Publish(node.ProjectID, logs.Line{
		Kind:   "status",
		Status: "stopped",
		Phase:  "start",
		Text:   reason,
		NodeID: node.ID,
	})
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
