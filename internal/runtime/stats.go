package runtime

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type ContainerStat struct {
	Name       string
	RAMUsed    int64
	RAMLimit   int64
	CPUPercent float64
}

// Stats is one docker stats pass across every Runex container. Callers must
// not fan this out per node — the daemon serializes stats collection.
func (c *Client) Stats(ctx context.Context) (map[string]ContainerStat, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	listed, err := c.output(ctx, "ps", "--format", "{{.Names}}", "--filter", "label=deploy.platform=1")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(listed, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return map[string]ContainerStat{}, nil
	}
	args := []string{"stats", "--no-stream", "--format", "{{.Name}}\t{{.MemUsage}}\t{{.CPUPerc}}"}
	args = append(args, names...)
	out, err := c.output(ctx, args...)
	if err != nil {
		return nil, err
	}
	result := map[string]ContainerStat{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 3 {
			continue
		}
		used, limit := parseMemUsage(parts[1])
		result[strings.TrimSpace(parts[0])] = ContainerStat{
			Name:       strings.TrimSpace(parts[0]),
			RAMUsed:    used,
			RAMLimit:   limit,
			CPUPercent: parsePercent(parts[2]),
		}
	}
	return result, nil
}

func parseMemUsage(raw string) (used, limit int64) {
	raw = strings.TrimSpace(raw)
	left, right, ok := strings.Cut(raw, "/")
	used = ParseSize(left)
	if ok {
		limit = ParseSize(right)
	}
	return used, limit
}

func parsePercent(raw string) float64 {
	raw = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(raw), "%"))
	n, _ := strconv.ParseFloat(raw, 64)
	if n < 0 {
		return 0
	}
	return n
}

func (c *Client) VolumeBytes(ctx context.Context, name string) int64 {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	out, err := c.output(ctx, "volume", "inspect", "-f", "{{.Mountpoint}}", name)
	if err != nil {
		return 0
	}
	return dirBytes(ctx, strings.TrimSpace(out))
}

func (c *Client) WritableLayerBytes(ctx context.Context, name string) int64 {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	out, err := c.output(ctx, "inspect", "-f", `{{index .GraphDriver.Data "UpperDir"}}`, name)
	if err != nil {
		return 0
	}
	return dirBytes(ctx, strings.TrimSpace(out))
}

func (c *Client) DiskUsed(ctx context.Context, kind, projectID, nodeID, containerName string) int64 {
	names := []string{}
	if strings.TrimSpace(containerName) != "" {
		names = append(names, containerName)
	}
	switch kind {
	case "postgresql":
		names = append(names, PostgresName(projectID, nodeID))
		for _, name := range names {
			if n := c.execDu(ctx, name, "/var/lib/postgresql/data"); n > 0 {
				return n
			}
		}
	case "redis":
		names = append(names, RedisName(projectID, nodeID))
		for _, name := range names {
			if n := c.execDu(ctx, name, "/data"); n > 0 {
				return n
			}
		}
	}
	if vol := VolumeFor(kind, projectID, nodeID); vol != "" {
		if n := c.VolumeBytes(ctx, vol); n > 0 {
			return n
		}
	}
	for _, name := range AppNames(projectID, nodeID, strPtr(containerName)) {
		if n := c.ContainerRWBytes(ctx, name); n > 0 {
			return n
		}
		if n := c.WritableLayerBytes(ctx, name); n > 0 {
			return n
		}
	}
	return 0
}

func (c *Client) ContainerRWBytes(ctx context.Context, name string) int64 {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	out, err := c.output(ctx, "inspect", "--size", "-f", "{{.SizeRw}}", name)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func (c *Client) execDu(ctx context.Context, name, path string) int64 {
	name = strings.TrimSpace(name)
	path = strings.TrimSpace(path)
	if name == "" || path == "" {
		return 0
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	out, err := c.output(ctx, "exec", name, "du", "-sb", path)
	if err != nil {
		return 0
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return 0
	}
	n, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func strPtr(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}

func dirBytes(ctx context.Context, path string) int64 {
	path = strings.TrimSpace(path)
	if path == "" || path == "<no value>" {
		return 0
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "du", "-sb", path)
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0
	}
	n, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// ParseSize reads Docker's human sizes: 23.54MiB, 1.2GiB, 512kB, 100B.
func ParseSize(raw string) int64 {
	raw = strings.TrimSpace(strings.ReplaceAll(raw, " ", ""))
	if raw == "" || raw == "0" {
		return 0
	}
	i := 0
	for i < len(raw) {
		ch := raw[i]
		if (ch >= '0' && ch <= '9') || ch == '.' {
			i++
			continue
		}
		break
	}
	if i == 0 {
		return 0
	}
	n, err := strconv.ParseFloat(raw[:i], 64)
	if err != nil {
		return 0
	}
	unit := strings.ToLower(raw[i:])
	mult := float64(1)
	switch unit {
	case "", "b":
		mult = 1
	case "kb", "k":
		mult = 1000
	case "kib", "ki":
		mult = 1024
	case "mb", "m":
		mult = 1000 * 1000
	case "mib", "mi":
		mult = 1024 * 1024
	case "gb", "g":
		mult = 1000 * 1000 * 1000
	case "gib", "gi":
		mult = 1024 * 1024 * 1024
	case "tb", "t":
		mult = 1000 * 1000 * 1000 * 1000
	case "tib", "ti":
		mult = 1024 * 1024 * 1024 * 1024
	}
	if n < 0 {
		return 0
	}
	return int64(n * mult)
}

var _ = time.Second
