// Package alloc is the single source of truth for per-node RAM and disk
// isolation. Every service, Postgres, and Redis node owns its own limits;
// changing them is a node-settings operation, not a workspace-wide toggle.
package alloc

import (
	"fmt"
	"strings"
)

const (
	MinMemoryMB = 64
	MinDiskMB   = 128
	MaxMemoryMB = 4096
	MaxDiskMB   = 20480
	// Every account starts with this much memory and disk.
	AccountMemoryMB = 1024
	AccountDiskMB   = 5 * 1024
)

func DefaultMemoryMB(kind string) int {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "postgresql":
		return 512
	case "redis":
		return 128
	default:
		return 512
	}
}

func DefaultDiskMB(kind string) int {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "postgresql":
		return 2048
	case "redis":
		return 512
	default:
		return 1024
	}
}

func ClampMemory(mb, max int) int {
	if max <= 0 {
		max = MaxMemoryMB
	}
	if mb < MinMemoryMB {
		return MinMemoryMB
	}
	if mb > max {
		return max
	}
	return mb
}

func ClampDisk(mb, max int) int {
	if max <= 0 {
		max = MaxDiskMB
	}
	if mb < MinDiskMB {
		return MinDiskMB
	}
	if mb > max {
		return max
	}
	return mb
}

// Resolve fills missing values with kind defaults and clamps to the platform max.
func Resolve(kind string, memoryMB, diskMB, maxMem, maxDisk int) (int, int) {
	if memoryMB <= 0 {
		memoryMB = DefaultMemoryMB(kind)
	}
	if diskMB <= 0 {
		diskMB = DefaultDiskMB(kind)
	}
	return ClampMemory(memoryMB, maxMem), ClampDisk(diskMB, maxDisk)
}

func DockerMemory(mb int) string {
	if mb <= 0 {
		mb = MinMemoryMB
	}
	return fmt.Sprintf("%dm", mb)
}

// RedisMaxMemory leaves ~10% of the cgroup for Redis overhead (AOF, clients).
func RedisMaxMemory(mb int) string {
	v := mb * 9 / 10
	if v < 16 {
		v = 16
	}
	return fmt.Sprintf("%dmb", v)
}

// PostgresTuning scales server settings to the memory a node was actually
// given. The stock image assumes it owns the machine: 128MB of shared_buffers,
// a 4GB effective_cache_size and a random_page_cost set for spinning disks. In
// a 512MB container those defaults make the planner pick sequential scans over
// index scans and leave most of the budget unused, which is felt directly as
// slow table and row reads.
func PostgresTuning(memoryMB int) []string {
	if memoryMB < MinMemoryMB {
		memoryMB = MinMemoryMB
	}
	sharedBuffers := clampInt(memoryMB/4, 32, 8192)
	effectiveCache := clampInt(memoryMB*3/4, 64, 24576)
	maintenance := clampInt(memoryMB/16, 32, 512)
	// work_mem is per sort, per connection, so it is sized against the
	// connection ceiling rather than the whole budget.
	maxConns := clampInt(memoryMB/8, 25, 200)
	workMem := clampInt(memoryMB/128, 2, 64)

	return []string{
		"postgres",
		"-c", fmt.Sprintf("shared_buffers=%dMB", sharedBuffers),
		"-c", fmt.Sprintf("effective_cache_size=%dMB", effectiveCache),
		"-c", fmt.Sprintf("maintenance_work_mem=%dMB", maintenance),
		"-c", fmt.Sprintf("work_mem=%dMB", workMem),
		"-c", fmt.Sprintf("max_connections=%d", maxConns),
		// NVMe, so an index read costs very nearly what a sequential read does
		// and many can be in flight at once.
		"-c", "random_page_cost=1.1",
		"-c", "effective_io_concurrency=200",
		// Spread checkpoints so a write burst cannot stall reads.
		"-c", "checkpoint_completion_target=0.9",
		"-c", "wal_compression=on",
		"-c", "max_wal_size=1GB",
		"-c", "min_wal_size=128MB",
		// Compiling a plan costs more than it saves on the short queries a
		// dashboard issues, and it allocates outside work_mem.
		"-c", "jit=off",
		// Keeps the planner's row estimates close enough that table listings
		// can trust them instead of counting rows.
		"-c", "autovacuum_analyze_scale_factor=0.05",
		"-c", "track_activity_query_size=4096",
	}
}

// CPUs is the Docker quota for a node. Databases get more than a plain service
// because a single query runs on one core and users judge a database by how
// fast one query comes back.
func CPUs(kind string, memoryMB int) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "postgresql":
		return formatCPUs(clampInt(memoryMB/256, 1, 4))
	case "redis":
		// Redis executes commands on one thread; the rest is AOF rewrite.
		return formatCPUs(clampInt(memoryMB/128, 1, 2))
	default:
		return "0.5"
	}
}

func formatCPUs(n int) string {
	return fmt.Sprintf("%d.0", n)
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// StorageOpt is the overlay writable-layer cap. Docker wants a size with a
// unit; we round up to whole gigabytes because overlay2 quota is coarse.
func StorageOpt(diskMB int) string {
	if diskMB <= 0 {
		return ""
	}
	g := (diskMB + 1023) / 1024
	if g < 1 {
		g = 1
	}
	return fmt.Sprintf("%dG", g)
}

func BytesFromMB(mb int) int64 {
	if mb <= 0 {
		return 0
	}
	return int64(mb) * 1024 * 1024
}
