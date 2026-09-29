package runtime

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Live check that Runex's isolated Postgres and Redis sidecars actually
// accept queries on the project Docker network — the same path StartSidecar uses.
func TestPostgresAndRedisSidecarsAcceptQueries(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	c := New()
	if _, err := c.output(ctx, "info"); err != nil {
		t.Skip("docker is not available")
	}

	projectID := "prj_sidecarcheck"
	pgNode := "nod_pgcheck"
	rdNode := "nod_rdcheck"
	defer c.PurgeProject(context.Background(), projectID)

	network := NetworkName(projectID)
	if err := c.EnsureNetwork(ctx, network); err != nil {
		t.Fatalf("network: %v", err)
	}

	pgName := PostgresName(projectID, pgNode)
	if err := c.RunSidecar(ctx, SidecarOpts{
		Name:      pgName,
		Image:     "postgres:16-alpine",
		Network:   network,
		Alias:     SidecarAlias("postgresql", pgNode, 0),
		ProjectID: projectID,
		NodeID:    pgNode,
		Role:      "postgresql",
		Memory:    "256m",
		Env: map[string]string{
			"POSTGRES_USER":     "app",
			"POSTGRES_PASSWORD": "app",
			"POSTGRES_DB":       "app",
		},
	}); err != nil {
		t.Fatalf("postgres start: %v", err)
	}

	rdName := RedisName(projectID, rdNode)
	if err := c.RunSidecar(ctx, SidecarOpts{
		Name:      rdName,
		Image:     "redis:7-alpine",
		Network:   network,
		Alias:     SidecarAlias("redis", rdNode, 0),
		ProjectID: projectID,
		NodeID:    rdNode,
		Role:      "redis",
		Memory:    "128m",
	}); err != nil {
		t.Fatalf("redis start: %v", err)
	}

	waitUntil(t, ctx, "postgres ready", func() error {
		out, err := c.output(ctx, "exec", "-e", "PGPASSWORD=app", pgName,
			"psql", "-h", "127.0.0.1", "-U", "app", "-d", "app", "-tAc", "select 1")
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) != "1" {
			return errString("postgres select 1 => " + out)
		}
		return nil
	})
	sql, err := c.output(ctx, "exec", "-e", "PGPASSWORD=app", pgName,
		"psql", "-h", "127.0.0.1", "-U", "app", "-d", "app", "-tAc", "select 1")
	if err != nil {
		t.Fatalf("postgres query: %v", err)
	}
	if strings.TrimSpace(sql) != "1" {
		t.Fatalf("postgres select 1 => %q", sql)
	}

	waitUntil(t, ctx, "redis ready", func() error {
		out, err := c.output(ctx, "exec", rdName, "redis-cli", "ping")
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) != "PONG" {
			return errString("redis ping => " + out)
		}
		return nil
	})

	probe := "deploy-" + projectID + "-aliasprobe"
	c.RemoveContainer(ctx, probe)
	if err := c.run(ctx, "run", "--rm", "--name", probe, "--network", network,
		"-e", "PGPASSWORD=app",
		"postgres:16-alpine",
		"psql", "-h", "postgres", "-U", "app", "-d", "app", "-tAc", "select 1",
	); err != nil {
		t.Fatalf("postgres alias on the project network: %v", err)
	}
	if err := c.run(ctx, "run", "--rm", "--name", probe, "--network", network,
		"redis:7-alpine", "redis-cli", "-h", "redis", "ping",
	); err != nil {
		t.Fatalf("redis alias on the project network: %v", err)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func waitUntil(t *testing.T, ctx context.Context, label string, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(40 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			t.Fatalf("%s: %v", label, ctx.Err())
		}
		last = fn()
		if last == nil {
			return
		}
		time.Sleep(400 * time.Millisecond)
	}
	t.Fatalf("%s: %v", label, last)
}
