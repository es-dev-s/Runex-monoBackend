package api

import (
	"fmt"
	"net/http"
	"strings"

	"deploy_platform/internal/alloc"
	"deploy_platform/internal/store"
	"deploy_platform/internal/usage"
)

func (s *Server) workspaceUsage(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	projects, err := s.store.ListProjects(r.Context(), user.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not load usage")
		return
	}
	nodes, err := s.store.ListNodesForUser(r.Context(), user.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not load usage")
		return
	}
	if filter := strings.TrimSpace(r.URL.Query().Get("project")); filter != "" {
		var kept []store.Project
		for _, project := range projects {
			if project.ID == filter {
				kept = append(kept, project)
			}
		}
		if len(kept) == 0 {
			writeErr(w, http.StatusNotFound, "not_found", "Project not found")
			return
		}
		projects = kept
		var scoped []store.Node
		for _, node := range nodes {
			if node.ProjectID == filter {
				scoped = append(scoped, node)
			}
		}
		nodes = scoped
	}
	w.Header().Set("Cache-Control", "no-store")
	snap := s.usageSnapshot(projects, nodes)
	snap.Totals.RAMAllocBytes = alloc.BytesFromMB(alloc.AccountMemoryMB)
	snap.Totals.DiskAllocBytes = alloc.BytesFromMB(alloc.AccountDiskMB)
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) projectUsage(w http.ResponseWriter, r *http.Request) {
	project, err := s.loadProject(w, r)
	if err != nil {
		return
	}
	nodes, err := s.store.ListNodes(r.Context(), project.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not load usage")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.usageSnapshot([]store.Project{project}, nodes))
}

func (s *Server) usageSnapshot(projects []store.Project, nodes []store.Node) usage.Snapshot {
	if s.sampler == nil {
		return usage.EmptySnapshot(projects, nodes, s.cfg.MaxMemoryMB, s.cfg.MaxDiskMB)
	}
	return s.sampler.Snapshot(projects, nodes, s.cfg.MaxMemoryMB, s.cfg.MaxDiskMB)
}

func (s *Server) applyNodeAlloc(w http.ResponseWriter, r *http.Request, node store.Node, memoryMB, diskMB *int) (store.Node, bool) {
	if memoryMB == nil && diskMB == nil {
		return node, true
	}
	mem, disk := alloc.Resolve(node.Kind, node.MemoryMB, node.DiskMB, s.cfg.MaxMemoryMB, s.cfg.MaxDiskMB)
	if memoryMB != nil {
		mem = alloc.ClampMemory(*memoryMB, s.cfg.MaxMemoryMB)
	}
	if diskMB != nil {
		disk = alloc.ClampDisk(*diskMB, s.cfg.MaxDiskMB)
	}
	if disk < node.DiskMB {
		used := s.engine.DiskUsed(r.Context(), node)
		if used > alloc.BytesFromMB(disk) {
			writeErr(w, http.StatusConflict, "disk_in_use", "This node is already storing more than that disk size. Free data before lowering the quota.")
			return node, false
		}
	}
	node.MemoryMB = mem
	node.DiskMB = disk
	if err := s.engine.ApplyAlloc(r.Context(), node); err != nil {
		writeErr(w, http.StatusBadGateway, "runtime", "Could not apply that allocation to the running container.")
		return node, false
	}
	if err := s.store.SetNodeAlloc(r.Context(), node.ProjectID, node.ID, mem, disk); err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not save allocation")
		return node, false
	}
	if node.Status == "running" {
		ramUsed := s.engine.RAMUsed(r.Context(), node)
		diskUsed := s.engine.DiskUsed(r.Context(), node)
		if ramUsed > alloc.BytesFromMB(mem) {
			s.engine.StopOverLimit(r.Context(), node, fmt.Sprintf("Stopped: memory exceeded the %d MB allocation.", mem))
		} else if diskUsed > alloc.BytesFromMB(disk) {
			s.engine.StopOverLimit(r.Context(), node, fmt.Sprintf("Stopped: disk exceeded the %d MB allocation.", disk))
		}
		if latest, err := s.store.GetNode(r.Context(), node.ProjectID, node.ID); err == nil {
			return latest, true
		}
	}
	return node, true
}
