package usage

import (
	"time"

	"deploy_platform/internal/alloc"
	"deploy_platform/internal/store"
)

type Limits struct {
	MinMemoryMB int `json:"minMemoryMb"`
	MaxMemoryMB int `json:"maxMemoryMb"`
	MinDiskMB   int `json:"minDiskMb"`
	MaxDiskMB   int `json:"maxDiskMb"`
}

type Totals struct {
	RAMUsedBytes   int64   `json:"ramUsedBytes"`
	RAMAllocBytes  int64   `json:"ramAllocBytes"`
	DiskUsedBytes  int64   `json:"diskUsedBytes"`
	DiskAllocBytes int64   `json:"diskAllocBytes"`
	CPUPercent     float64 `json:"cpuPercent"`
}

type NodeUsage struct {
	ID            string  `json:"id"`
	Title         string  `json:"title"`
	Kind          string  `json:"kind"`
	Status        string  `json:"status"`
	MemoryMB      int     `json:"memoryMb"`
	DiskMB        int     `json:"diskMb"`
	RAMUsedBytes  int64   `json:"ramUsedBytes"`
	DiskUsedBytes int64   `json:"diskUsedBytes"`
	CPUPercent    float64 `json:"cpuPercent"`
	Series        []Point `json:"series"`
}

type ProjectUsage struct {
	ID             string      `json:"id"`
	Name           string      `json:"name"`
	Environment    string      `json:"environment"`
	FamilyID       string      `json:"familyId"`
	RAMUsedBytes   int64       `json:"ramUsedBytes"`
	RAMAllocBytes  int64       `json:"ramAllocBytes"`
	DiskUsedBytes  int64       `json:"diskUsedBytes"`
	DiskAllocBytes int64       `json:"diskAllocBytes"`
	CPUPercent     float64     `json:"cpuPercent"`
	Nodes          []NodeUsage `json:"nodes"`
}

type Snapshot struct {
	GeneratedAt     time.Time      `json:"generatedAt"`
	IntervalSeconds int            `json:"intervalSeconds"`
	Limits          Limits         `json:"limits"`
	Totals          Totals         `json:"totals"`
	Projects        []ProjectUsage `json:"projects"`
}

func (s *Sampler) Snapshot(projects []store.Project, nodes []store.Node, maxMem, maxDisk int) Snapshot {
	if maxMem <= 0 {
		maxMem = alloc.MaxMemoryMB
	}
	if maxDisk <= 0 {
		maxDisk = alloc.MaxDiskMB
	}
	byProject := map[string][]store.Node{}
	for _, node := range nodes {
		byProject[node.ProjectID] = append(byProject[node.ProjectID], node)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	out := Snapshot{
		GeneratedAt:     s.sampled,
		IntervalSeconds: s.IntervalSeconds(),
		Limits: Limits{
			MinMemoryMB: alloc.MinMemoryMB,
			MaxMemoryMB: maxMem,
			MinDiskMB:   alloc.MinDiskMB,
			MaxDiskMB:   maxDisk,
		},
		Projects: []ProjectUsage{},
	}
	if out.GeneratedAt.IsZero() {
		out.GeneratedAt = time.Now()
	}

	for _, project := range projects {
		item := ProjectUsage{
			ID:          project.ID,
			Name:        project.Name,
			Environment: project.Environment,
			FamilyID:    project.FamilyID,
			Nodes:       []NodeUsage{},
		}
		for _, node := range byProject[project.ID] {
			mem, disk := alloc.Resolve(node.Kind, node.MemoryMB, node.DiskMB, maxMem, maxDisk)
			cur := s.live[node.ID]
			hist := s.history[node.ID]
			if hist == nil {
				hist = []Point{}
			}
			item.Nodes = append(item.Nodes, NodeUsage{
				ID:            node.ID,
				Title:         node.Title,
				Kind:          node.Kind,
				Status:        node.Status,
				MemoryMB:      mem,
				DiskMB:        disk,
				RAMUsedBytes:  cur.ram,
				DiskUsedBytes: cur.disk,
				CPUPercent:    cur.cpu,
				Series:        hist,
			})
			item.RAMUsedBytes += cur.ram
			item.DiskUsedBytes += cur.disk
			item.CPUPercent += cur.cpu
			item.RAMAllocBytes += alloc.BytesFromMB(mem)
			item.DiskAllocBytes += alloc.BytesFromMB(disk)
		}
		out.Projects = append(out.Projects, item)
		out.Totals.RAMUsedBytes += item.RAMUsedBytes
		out.Totals.DiskUsedBytes += item.DiskUsedBytes
		out.Totals.CPUPercent += item.CPUPercent
		out.Totals.RAMAllocBytes += item.RAMAllocBytes
		out.Totals.DiskAllocBytes += item.DiskAllocBytes
	}
	return out
}

func EmptySnapshot(projects []store.Project, nodes []store.Node, maxMem, maxDisk int) Snapshot {
	dummy := &Sampler{live: map[string]live{}, history: map[string][]Point{}}
	return dummy.Snapshot(projects, nodes, maxMem, maxDisk)
}
