package usage

import (
	"context"
	"strings"
	"sync"
	"time"

	"deploy_platform/internal/alloc"
	"deploy_platform/internal/runtime"
	"deploy_platform/internal/store"
)

const (
	interval    = 12 * time.Second
	historyKeep = 90
	diskEvery   = 4
)

type Point struct {
	T    int64   `json:"t"`
	RAM  int64   `json:"ram"`
	Disk int64   `json:"disk"`
	CPU  float64 `json:"cpu"`
}

type live struct {
	ram  int64
	cpu  float64
	disk int64
}

type Sampler struct {
	docker *runtime.Client
	store  *store.Store

	mu      sync.RWMutex
	live    map[string]live
	history map[string][]Point
	ticks   int
	sampled time.Time
	strikes map[string]int
	onOver  func(ctx context.Context, node store.Node, reason string)
}

func New(docker *runtime.Client, st *store.Store) *Sampler {
	return &Sampler{
		docker:  docker,
		store:   st,
		live:    map[string]live{},
		history: map[string][]Point{},
		strikes: map[string]int{},
	}
}

func (s *Sampler) SetOverLimit(fn func(ctx context.Context, node store.Node, reason string)) {
	s.onOver = fn
}

func (s *Sampler) Run(ctx context.Context) {
	s.tick(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

func (s *Sampler) IntervalSeconds() int {
	return int(interval / time.Second)
}

// Read returns the latest RAM and CPU sample for one deployment.
func (s *Sampler) Read(nodeID string) (ram int64, cpu float64) {
	if s == nil || nodeID == "" {
		return 0, 0
	}
	s.mu.RLock()
	cur := s.live[nodeID]
	s.mu.RUnlock()
	return cur.ram, cur.cpu
}

func (s *Sampler) tick(ctx context.Context) {
	nodes, err := s.store.ListAllNodes(ctx)
	if err != nil {
		return
	}
	stats, _ := s.docker.Stats(ctx)
	if stats == nil {
		stats = map[string]runtime.ContainerStat{}
	}

	s.mu.Lock()
	s.ticks++
	wantDisk := s.ticks%diskEvery == 1
	s.mu.Unlock()

	disks := map[string]int64{}
	if wantDisk {
		for _, node := range nodes {
			container := ""
			if node.ContainerName != nil {
				container = *node.ContainerName
			}
			disks[node.ID] = s.docker.DiskUsed(ctx, node.Kind, node.ProjectID, node.ID, container)
		}
	}

	now := time.Now()
	ts := now.Unix()
	s.mu.Lock()
	s.sampled = now
	seen := map[string]struct{}{}
	for _, node := range nodes {
		st := pickStat(node, stats)
		cur := s.live[node.ID]
		cur.ram = st.RAMUsed
		cur.cpu = st.CPUPercent
		if used, ok := disks[node.ID]; ok && (used > 0 || cur.disk == 0) {
			cur.disk = used
		}
		s.live[node.ID] = cur
		seen[node.ID] = struct{}{}
		hist := append(s.history[node.ID], Point{T: ts, RAM: cur.ram, Disk: cur.disk, CPU: cur.cpu})
		if len(hist) > historyKeep {
			hist = hist[len(hist)-historyKeep:]
		}
		s.history[node.ID] = hist
	}
	for id := range s.live {
		if _, ok := seen[id]; !ok {
			delete(s.live, id)
			delete(s.history, id)
		}
	}
	s.mu.Unlock()

	s.enforceLimits(ctx, nodes)
}

func (s *Sampler) enforceLimits(ctx context.Context, nodes []store.Node) {
	if s.onOver == nil {
		return
	}
	s.mu.RLock()
	type hit struct {
		node   store.Node
		reason string
	}
	var hits []hit
	for _, node := range nodes {
		if node.Status != "running" {
			continue
		}
		mem, disk := alloc.Resolve(node.Kind, node.MemoryMB, node.DiskMB, alloc.MaxMemoryMB, alloc.MaxDiskMB)
		cur := s.live[node.ID]
		ramCap := int64(mem) * 1024 * 1024
		diskCap := int64(disk) * 1024 * 1024
		overRAM := mem > 0 && ramCap > 0 && cur.ram >= ramCap*97/100
		overDisk := disk > 0 && diskCap > 0 && cur.disk > diskCap
		if overDisk {
			hits = append(hits, hit{node: node, reason: "Stopped: disk exceeded the allocated quota."})
			continue
		}
		if overRAM {
			hits = append(hits, hit{node: node, reason: "Stopped: memory exceeded the allocated RAM."})
		}
	}
	s.mu.RUnlock()

	s.mu.Lock()
	var fire []hit
	for _, item := range hits {
		if strings.HasPrefix(item.reason, "Stopped: disk") {
			s.strikes[item.node.ID] = 0
			fire = append(fire, item)
			continue
		}
		s.strikes[item.node.ID]++
		if s.strikes[item.node.ID] >= 2 {
			fire = append(fire, item)
			delete(s.strikes, item.node.ID)
		}
	}
	for _, node := range nodes {
		over := false
		for _, item := range hits {
			if item.node.ID == node.ID {
				over = true
				break
			}
		}
		if !over {
			delete(s.strikes, node.ID)
		}
	}
	s.mu.Unlock()

	for _, item := range fire {
		s.onOver(ctx, item.node, item.reason)
	}
}

func pickStat(node store.Node, stats map[string]runtime.ContainerStat) runtime.ContainerStat {
	var best runtime.ContainerStat
	for _, name := range containerNames(node) {
		if st, ok := stats[name]; ok && st.RAMUsed >= best.RAMUsed {
			best = st
		}
	}
	return best
}

func containerNames(node store.Node) []string {
	if node.Kind == "postgresql" || node.Kind == "redis" {
		name := runtime.SidecarName(node.ProjectID, node.ID, sidecarRole(node.Kind))
		if node.ContainerName != nil && *node.ContainerName != "" && *node.ContainerName != name {
			return []string{name, *node.ContainerName}
		}
		return []string{name}
	}
	return runtime.AppNames(node.ProjectID, node.ID, node.ContainerName)
}

func sidecarRole(kind string) string {
	if kind == "postgresql" {
		return "postgres"
	}
	return "redis"
}
