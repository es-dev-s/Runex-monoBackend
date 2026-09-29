package store

import (
	"context"
	"errors"
	"strings"

	"deploy_platform/internal/alloc"

	"github.com/jackc/pgx/v5/pgconn"
)

const (
	EnvProduction = "production"
	EnvStaging    = "staging"
)

func NormalizeEnv(env string) string {
	switch strings.ToLower(strings.TrimSpace(env)) {
	case EnvStaging, "preview":
		return EnvStaging
	default:
		return EnvProduction
	}
}

func FamilyOf(p Project) string {
	if id := strings.TrimSpace(p.FamilyID); id != "" {
		return id
	}
	return p.ID
}

func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// CreateProjectWithResources inserts a project and its nodes/vars in one
// transaction so a concurrent ensure cannot observe a half-cloned environment.
func (s *Store) CreateProjectWithResources(ctx context.Context, project Project, nodes []Node, vars []EnvVar) error {
	env := NormalizeEnv(project.Environment)
	family := strings.TrimSpace(project.FamilyID)
	if family == "" {
		family = project.ID
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO projects (id, name, subtitle, status, user_id, environment, family_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, project.ID, project.Name, project.Subtitle, project.Status, project.UserID, env, family); err != nil {
		return err
	}

	for _, n := range nodes {
		n.MemoryMB, n.DiskMB = alloc.Resolve(n.Kind, n.MemoryMB, n.DiskMB, alloc.MaxMemoryMB, alloc.MaxDiskMB)
		if _, err := tx.Exec(ctx, `
			INSERT INTO nodes (
			  id, project_id, kind, title, caption, x, y, status, container_name,
			  url, port, framework, source_type, source_ref, error, languages, stack,
			  github_installation_id, github_branch, auto_deploy, memory_mb, disk_mb
			)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)
		`, n.ID, n.ProjectID, n.Kind, n.Title, n.Caption, n.X, n.Y, n.Status, n.ContainerName,
			n.URL, n.Port, n.Framework, n.SourceType, n.SourceRef, n.Error, mustJSON(n.Languages), mustJSON(n.Stack),
			n.GitHubInstallationID, n.GitHubBranch, n.AutoDeploy, n.MemoryMB, n.DiskMB); err != nil {
			return err
		}
	}

	for _, v := range vars {
		if _, err := tx.Exec(ctx, `
			INSERT INTO project_vars (id, project_id, node_id, key, value, source)
			VALUES ($1,$2,$3,$4,$5,$6)
		`, v.ID, project.ID, v.NodeID, v.Key, v.Value, v.Source); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}
