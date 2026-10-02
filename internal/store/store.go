package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"deploy_platform/internal/alloc"
	"deploy_platform/internal/ident"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Project struct {
	ID            string      `json:"id"`
	Name          string      `json:"name"`
	Subtitle      string      `json:"subtitle"`
	Status        string      `json:"status"`
	Framework     *string     `json:"framework,omitempty"`
	URL           *string     `json:"url,omitempty"`
	Port          *int        `json:"port,omitempty"`
	ContainerName *string     `json:"containerName,omitempty"`
	SourceType    *string     `json:"sourceType,omitempty"`
	SourceRef     *string     `json:"sourceRef,omitempty"`
	Error         *string     `json:"error,omitempty"`
	Languages     []Language  `json:"languages,omitempty"`
	Stack         []StackItem `json:"stack,omitempty"`
	Environment   string      `json:"environment"`
	FamilyID      string      `json:"familyId"`
	CreatedAt     time.Time   `json:"createdAt"`
	UpdatedAt     time.Time   `json:"updatedAt"`
	UserID        string      `json:"-"`
}

type Language struct {
	Name       string  `json:"name"`
	Percentage float64 `json:"percentage"`
	Color      string  `json:"color,omitempty"`
	Bytes      int64   `json:"bytes,omitempty"`
}

type StackItem struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Color string `json:"color,omitempty"`
}

type EnvVar struct {
	ID     string `json:"id"`
	NodeID string `json:"nodeId,omitempty"`
	Key    string `json:"key"`
	Value  string `json:"value"`
	Source string `json:"source"`
}

type Node struct {
	ID                   string      `json:"id"`
	ProjectID            string      `json:"projectId"`
	Kind                 string      `json:"kind"`
	Title                string      `json:"title"`
	Caption              string      `json:"caption"`
	X                    float64     `json:"x"`
	Y                    float64     `json:"y"`
	Status               string      `json:"status"`
	ContainerName        *string     `json:"containerName,omitempty"`
	URL                  *string     `json:"url,omitempty"`
	Port                 *int        `json:"port,omitempty"`
	Framework            *string     `json:"framework,omitempty"`
	SourceType           *string     `json:"sourceType,omitempty"`
	SourceRef            *string     `json:"sourceRef,omitempty"`
	Error                *string     `json:"error,omitempty"`
	Languages            []Language  `json:"languages,omitempty"`
	Stack                []StackItem `json:"stack,omitempty"`
	GitHubInstallationID *int64      `json:"githubInstallationId,omitempty"`
	GitHubBranch         *string     `json:"githubBranch,omitempty"`
	AutoDeploy           bool        `json:"autoDeploy,omitempty"`
	MemoryMB             int         `json:"memoryMb"`
	DiskMB               int         `json:"diskMb"`
	PlatformURLRevoked   bool        `json:"-"`
	PlatformURLEnabled   bool        `json:"platformUrlEnabled"`
	PlatformURLUserSet   bool        `json:"-"`
	PlatformURL          string      `json:"platformUrl,omitempty"`
	PlatformSlug         string      `json:"platformSlug,omitempty"`
	PrimaryKind          string      `json:"primaryKind,omitempty"`
	StartedAt            *time.Time  `json:"startedAt,omitempty"`
	UserID               string      `json:"-"`
}

type Installation struct {
	ID        int64     `json:"id"`
	Account   string    `json:"account"`
	Type      string    `json:"type"`
	UserID    string    `json:"-"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type Deployment struct {
	ID           string     `json:"id"`
	ProjectID    string     `json:"projectId"`
	NodeID       string     `json:"nodeId,omitempty"`
	ServiceTitle string     `json:"serviceTitle,omitempty"`
	ProjectName  string     `json:"projectName,omitempty"`
	Status       string     `json:"status"`
	Phase        string     `json:"phase"`
	Trigger      string     `json:"trigger,omitempty"`
	SourceType   string     `json:"sourceType"`
	SourceRef    *string    `json:"sourceRef,omitempty"`
	Framework    *string    `json:"framework,omitempty"`
	URL          *string    `json:"url,omitempty"`
	Port         *int       `json:"port,omitempty"`
	Error        *string    `json:"error,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
	FinishedAt   *time.Time `json:"finishedAt,omitempty"`
}

type LogLine struct {
	ID           int64     `json:"id"`
	ProjectID    string    `json:"projectId"`
	DeploymentID string    `json:"deploymentId"`
	NodeID       string    `json:"nodeId,omitempty"`
	TS           time.Time `json:"ts"`
	Phase        string    `json:"phase"`
	Level        string    `json:"level"`
	Text         string    `json:"text"`
}

type Store struct {
	pool *pgxpool.Pool
}

func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) CreateProject(ctx context.Context, p Project) error {
	env := NormalizeEnv(p.Environment)
	family := strings.TrimSpace(p.FamilyID)
	if family == "" {
		family = p.ID
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO projects (id, name, subtitle, status, user_id, environment, family_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, p.ID, p.Name, p.Subtitle, p.Status, p.UserID, env, family)
	return err
}

const projectCols = `id, name, subtitle, status, framework, url, port, container_name,
		       source_type, source_ref, error, languages, stack, created_at, updated_at,
		       COALESCE(NULLIF(environment, ''), 'production'),
		       COALESCE(NULLIF(family_id, ''), id)`

func (s *Store) ListProjects(ctx context.Context, userID string) ([]Project, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+projectCols+`
		FROM projects
		WHERE user_id=$1
		ORDER BY created_at DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if out == nil {
		out = []Project{}
	}
	return out, rows.Err()
}

func (s *Store) GetProject(ctx context.Context, id string) (Project, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+projectCols+` FROM projects WHERE id = $1`, id)
	return scanProject(row)
}

func (s *Store) GetOwnedProject(ctx context.Context, userID, id string) (Project, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+projectCols+` FROM projects WHERE id=$1 AND user_id=$2`, id, userID)
	return scanProject(row)
}

func (s *Store) GetOwnedByFamilyEnv(ctx context.Context, userID, familyID, env string) (Project, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+projectCols+`
		FROM projects
		WHERE user_id=$1
		  AND COALESCE(NULLIF(family_id, ''), id)=$2
		  AND COALESCE(NULLIF(environment, ''), 'production')=$3
	`, userID, familyID, NormalizeEnv(env))
	return scanProject(row)
}

func (s *Store) ListFamily(ctx context.Context, userID, familyID string) ([]Project, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+projectCols+`
		FROM projects
		WHERE user_id=$1 AND COALESCE(NULLIF(family_id, ''), id)=$2
		ORDER BY environment ASC
	`, userID, familyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if out == nil {
		out = []Project{}
	}
	return out, rows.Err()
}

func (s *Store) UpdateProject(ctx context.Context, p Project) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE projects SET
		  name=$2, subtitle=$3, status=$4, framework=$5, url=$6, port=$7,
		  container_name=$8, source_type=$9, source_ref=$10, error=$11,
		  languages=$12, stack=$13, updated_at=now()
		WHERE id=$1
	`, p.ID, p.Name, p.Subtitle, p.Status, p.Framework, p.URL, p.Port, p.ContainerName, p.SourceType, p.SourceRef, p.Error, mustJSON(p.Languages), mustJSON(p.Stack))
	return err
}

func (s *Store) SetStatus(ctx context.Context, id, status string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE projects SET status=$2, error=NULL, updated_at=now() WHERE id=$1
	`, id, status)
	return err
}

func (s *Store) SetFailed(ctx context.Context, id, message string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE projects SET status='failed', error=$2, updated_at=now() WHERE id=$1
	`, id, message)
	return err
}

func (s *Store) SetPort(ctx context.Context, id string, port int) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE projects SET port=$2, updated_at=now() WHERE id=$1
	`, id, port)
	return err
}

func (s *Store) SetLive(ctx context.Context, id, url, container string, port int, framework *string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE projects SET
		  status='running', url=$2, port=$3, container_name=$4, framework=$5,
		  error=NULL, updated_at=now()
		WHERE id=$1
	`, id, url, port, container, framework)
	return err
}

func (s *Store) SetSource(ctx context.Context, id, sourceType, sourceRef string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE projects SET source_type=$2, source_ref=$3, status='building', error=NULL, updated_at=now()
		WHERE id=$1
	`, id, sourceType, sourceRef)
	return err
}

func (s *Store) SetStack(ctx context.Context, id string, languages []Language, stack []StackItem) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE projects SET languages=$2, stack=$3, updated_at=now() WHERE id=$1
	`, id, mustJSON(languages), mustJSON(stack))
	return err
}

func (s *Store) DeleteProject(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM projects WHERE id=$1`, id)
	return err
}

func (s *Store) UsedPorts(ctx context.Context) ([]int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT port FROM projects WHERE port IS NOT NULL
		UNION
		SELECT port FROM nodes WHERE port IS NOT NULL
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ports []int
	for rows.Next() {
		var port int
		if err := rows.Scan(&port); err != nil {
			return nil, err
		}
		ports = append(ports, port)
	}
	return ports, rows.Err()
}

const nodeCols = `id, project_id, kind, title, caption, x, y, status, container_name,
	url, port, framework, source_type, source_ref, error, languages, stack,
	github_installation_id, github_branch, auto_deploy,
	COALESCE(memory_mb, 0), COALESCE(disk_mb, 0),
	COALESCE(platform_url_revoked, false), COALESCE(platform_url_user_set, false),
	COALESCE(platform_slug, '')`

func (s *Store) ListNodes(ctx context.Context, projectID string) ([]Node, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+nodeCols+` FROM nodes WHERE project_id=$1 ORDER BY created_at`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if out == nil {
		out = []Node{}
	}
	return out, rows.Err()
}

func (s *Store) ListNodesForUser(ctx context.Context, userID string) ([]Node, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+nodeCols+` FROM nodes
		WHERE project_id IN (SELECT id FROM projects WHERE user_id=$1)
		ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if out == nil {
		out = []Node{}
	}
	return out, rows.Err()
}

func (s *Store) ListServiceNodes(ctx context.Context) ([]Node, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+nodeCols+` FROM nodes WHERE kind='service' ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if out == nil {
		out = []Node{}
	}
	return out, rows.Err()
}

func (s *Store) ListServicesByStatus(ctx context.Context, status string) ([]Node, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+nodeCols+` FROM nodes WHERE kind='service' AND status=$1 ORDER BY created_at`, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if out == nil {
		out = []Node{}
	}
	return out, rows.Err()
}

func (s *Store) ListAllSidecars(ctx context.Context) ([]Node, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+nodeCols+` FROM nodes WHERE kind IN ('postgresql','redis') ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if out == nil {
		out = []Node{}
	}
	return out, rows.Err()
}

func (s *Store) UpsertNode(ctx context.Context, n Node) error {
	n.MemoryMB, n.DiskMB = alloc.Resolve(n.Kind, n.MemoryMB, n.DiskMB, alloc.MaxMemoryMB, alloc.MaxDiskMB)
	_, err := s.pool.Exec(ctx, `
		INSERT INTO nodes (
		  id, project_id, kind, title, caption, x, y, status, container_name,
		  url, port, framework, source_type, source_ref, error, languages, stack,
		  github_installation_id, github_branch, auto_deploy, memory_mb, disk_mb
		)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)
		ON CONFLICT (id) DO UPDATE SET
		  title=EXCLUDED.title, caption=EXCLUDED.caption,
		  status=EXCLUDED.status, container_name=EXCLUDED.container_name,
		  url=COALESCE(EXCLUDED.url, nodes.url),
		  port=COALESCE(EXCLUDED.port, nodes.port),
		  framework=COALESCE(EXCLUDED.framework, nodes.framework),
		  source_type=COALESCE(EXCLUDED.source_type, nodes.source_type),
		  source_ref=COALESCE(EXCLUDED.source_ref, nodes.source_ref),
		  error=EXCLUDED.error,
		  languages=CASE WHEN EXCLUDED.languages = '[]'::jsonb THEN nodes.languages ELSE EXCLUDED.languages END,
		  stack=CASE WHEN EXCLUDED.stack = '[]'::jsonb THEN nodes.stack ELSE EXCLUDED.stack END,
		  github_installation_id=COALESCE(EXCLUDED.github_installation_id, nodes.github_installation_id),
		  github_branch=COALESCE(EXCLUDED.github_branch, nodes.github_branch),
		  auto_deploy=nodes.auto_deploy OR EXCLUDED.auto_deploy
	`, n.ID, n.ProjectID, n.Kind, n.Title, n.Caption, n.X, n.Y, n.Status, n.ContainerName,
		n.URL, n.Port, n.Framework, n.SourceType, n.SourceRef, n.Error, mustJSON(n.Languages), mustJSON(n.Stack),
		n.GitHubInstallationID, n.GitHubBranch, n.AutoDeploy, n.MemoryMB, n.DiskMB)
	return err
}

func (s *Store) SetNodeAlloc(ctx context.Context, projectID, nodeID string, memoryMB, diskMB int) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE nodes SET memory_mb=$3, disk_mb=$4 WHERE project_id=$1 AND id=$2
	`, projectID, nodeID, memoryMB, diskMB)
	return err
}

func (s *Store) ListAllNodes(ctx context.Context) ([]Node, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+nodeCols+` FROM nodes ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if out == nil {
		out = []Node{}
	}
	return out, rows.Err()
}

func (s *Store) MoveNode(ctx context.Context, projectID, nodeID string, x, y float64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE nodes SET x=$3, y=$4 WHERE project_id=$1 AND id=$2
	`, projectID, nodeID, x, y)
	return err
}

func (s *Store) SetAutoDeploy(ctx context.Context, projectID, nodeID string, enabled bool) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE nodes SET auto_deploy=$3 WHERE project_id=$1 AND id=$2
	`, projectID, nodeID, enabled)
	return err
}

func (s *Store) SetGitHubBranch(ctx context.Context, projectID, nodeID, branch string) error {
	branch = strings.TrimSpace(branch)
	var value any
	if branch != "" {
		value = branch
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE nodes SET github_branch=$3 WHERE project_id=$1 AND id=$2
	`, projectID, nodeID, value)
	return err
}

func (s *Store) ResetService(ctx context.Context, projectID, nodeID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE nodes SET
		  title='Service',
		  caption='Service',
		  status='idle',
		  container_name=NULL,
		  url=NULL,
		  port=NULL,
		  framework=NULL,
		  source_type=NULL,
		  source_ref=NULL,
		  error=NULL,
		  languages='[]'::jsonb,
		  stack='[]'::jsonb,
		  github_installation_id=NULL,
		  github_branch=NULL,
		  auto_deploy=false,
		  platform_url_revoked=false,
		  platform_url_user_set=false
		WHERE project_id=$1 AND id=$2
	`, projectID, nodeID)
	return err
}

func (s *Store) DeleteNode(ctx context.Context, projectID, nodeID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM nodes WHERE project_id=$1 AND id=$2`, projectID, nodeID)
	return err
}

func (s *Store) GetNode(ctx context.Context, projectID, nodeID string) (Node, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+nodeCols+` FROM nodes WHERE project_id=$1 AND id=$2`, projectID, nodeID)
	return scanNode(row)
}

func (s *Store) GetServiceBySlug(ctx context.Context, slug string) (Node, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if slug == "" {
		return Node{}, pgx.ErrNoRows
	}
	// Explicit platform subdomain wins over derived title/id aliases.
	row := s.pool.QueryRow(ctx, `
		SELECT `+nodeCols+`
		FROM nodes
		WHERE kind='service' AND lower(platform_slug) = $1
		LIMIT 1
	`, slug)
	if n, err := scanNode(row); err == nil {
		return n, nil
	}
	row = s.pool.QueryRow(ctx, `
		SELECT `+nodeCols+`
		FROM nodes
		WHERE kind='service'
		  AND lower(replace(replace(replace(id, ':', '-'), '/', '-'), '_', '-')) = $1
		LIMIT 1
	`, slug)
	return scanNode(row)
}

func (s *Store) SetPlatformSlug(ctx context.Context, projectID, nodeID, slug string) error {
	slug = strings.ToLower(strings.TrimSpace(slug))
	_, err := s.pool.Exec(ctx, `
		UPDATE nodes SET platform_slug=$3
		WHERE project_id=$1 AND id=$2 AND kind='service'
	`, projectID, nodeID, slug)
	return err
}

func (s *Store) PlatformSlugTaken(ctx context.Context, slug, exceptNodeID string) (bool, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if slug == "" {
		return false, nil
	}
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM nodes
		WHERE kind='service' AND lower(platform_slug)=$1 AND id<>$2
	`, slug, exceptNodeID).Scan(&n)
	return n > 0, err
}

func (s *Store) CountServices(ctx context.Context, projectID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM nodes WHERE project_id=$1 AND kind='service'`, projectID).Scan(&n)
	return n, err
}

func (s *Store) SetNodePort(ctx context.Context, projectID, nodeID string, port int) error {
	_, err := s.pool.Exec(ctx, `UPDATE nodes SET port=$3 WHERE project_id=$1 AND id=$2`, projectID, nodeID, port)
	return err
}

func (s *Store) SetNodeStack(ctx context.Context, projectID, nodeID string, languages []Language, stack []StackItem) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE nodes SET languages=$3, stack=$4 WHERE project_id=$1 AND id=$2
	`, projectID, nodeID, mustJSON(languages), mustJSON(stack))
	return err
}

func (s *Store) RollupProject(ctx context.Context, projectID string) error {
	nodes, err := s.ListNodes(ctx, projectID)
	if err != nil {
		return err
	}
	var services []Node
	for _, node := range nodes {
		if node.Kind == "service" {
			services = append(services, node)
		}
	}
	status := "idle"
	var primary *Node
	for i := range services {
		svc := &services[i]
		switch svc.Status {
		case "building":
			if status != "running" {
				status = "building"
			}
		case "running":
			status = "running"
			primary = svc
		case "failed":
			if status == "idle" || status == "stopped" || status == "ready" {
				status = "failed"
			}
		case "ready":
			if status == "idle" || status == "stopped" {
				status = "ready"
			}
		case "stopped":
			if status == "idle" {
				status = "stopped"
			}
		}
		if primary == nil && (svc.URL != nil || svc.SourceType != nil) {
			primary = svc
		}
	}
	if primary == nil && len(services) > 0 {
		primary = &services[len(services)-1]
	}
	p := Project{ID: projectID, Status: status}
	if primary != nil {
		p.Framework = primary.Framework
		p.URL = primary.URL
		p.Port = primary.Port
		p.ContainerName = primary.ContainerName
		p.SourceType = primary.SourceType
		p.SourceRef = primary.SourceRef
		p.Error = primary.Error
		p.Languages = primary.Languages
		p.Stack = primary.Stack
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE projects SET
		  status=$2, framework=$3, url=$4, port=$5, container_name=$6,
		  source_type=$7, source_ref=$8, error=$9, languages=$10, stack=$11, updated_at=now()
		WHERE id=$1
	`, projectID, p.Status, p.Framework, p.URL, p.Port, p.ContainerName, p.SourceType, p.SourceRef, p.Error, mustJSON(p.Languages), mustJSON(p.Stack))
	return err
}

func (s *Store) CreateDeployment(ctx context.Context, d Deployment) error {
	if strings.TrimSpace(d.Trigger) == "" {
		d.Trigger = "manual"
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO deployments (id, project_id, node_id, status, phase, source_type, source_ref, trigger)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
	`, d.ID, d.ProjectID, d.NodeID, d.Status, d.Phase, d.SourceType, d.SourceRef, d.Trigger)
	return err
}

func (s *Store) FinishDeployment(ctx context.Context, d Deployment) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE deployments SET status=$2, phase=$3, framework=$4, url=$5, port=$6, error=$7, finished_at=now()
		WHERE id=$1
	`, d.ID, d.Status, d.Phase, d.Framework, d.URL, d.Port, d.Error)
	return err
}

func (s *Store) LatestDeployment(ctx context.Context, projectID string) (Deployment, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, project_id, node_id, status, phase, source_type, source_ref, framework, url, port, error, created_at, finished_at
		FROM deployments WHERE project_id=$1 ORDER BY created_at DESC LIMIT 1
	`, projectID)
	var d Deployment
	err := row.Scan(&d.ID, &d.ProjectID, &d.NodeID, &d.Status, &d.Phase, &d.SourceType, &d.SourceRef, &d.Framework, &d.URL, &d.Port, &d.Error, &d.CreatedAt, &d.FinishedAt)
	return d, err
}

// OpenBuildStarts is the created_at of each node's unfinished deployment, used
// as the live build clock when REST reloads wipe SSE startedAt.
func (s *Store) OpenBuildStartsForUser(ctx context.Context, userID string) (map[string]time.Time, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (d.node_id) d.node_id, d.created_at
		FROM deployments d
		INNER JOIN projects p ON p.id = d.project_id
		WHERE p.user_id=$1 AND d.finished_at IS NULL AND COALESCE(d.node_id, '') <> ''
		ORDER BY d.node_id, d.created_at DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var id string
		var at time.Time
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		out[id] = at
	}
	return out, rows.Err()
}

func (s *Store) OpenBuildStarts(ctx context.Context, projectID string) (map[string]time.Time, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (node_id) node_id, created_at
		FROM deployments
		WHERE project_id=$1 AND finished_at IS NULL AND COALESCE(node_id, '') <> ''
		ORDER BY node_id, created_at DESC
	`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var id string
		var at time.Time
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		out[id] = at
	}
	return out, rows.Err()
}

func (s *Store) FinishOpenDeployments(ctx context.Context, projectID, nodeID, status, message string) error {
	var errPtr *string
	if strings.TrimSpace(message) != "" {
		msg := message
		errPtr = &msg
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE deployments SET status=$3, error=$4, finished_at=now()
		WHERE project_id=$1 AND node_id=$2 AND finished_at IS NULL
	`, projectID, nodeID, status, errPtr)
	return err
}

type ActivityStats struct {
	ProjectID string `json:"projectId"`
	Name      string `json:"name"`
	Deploys   int    `json:"deploys"`
	Failed    int    `json:"failed"`
	GitHub    int    `json:"github"`
	Live      int    `json:"live"`
}

func (s *Store) ListActivityStats(ctx context.Context, userID string) ([]ActivityStats, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.id, p.name,
		  COUNT(d.id) FILTER (WHERE d.status IN ('success', 'running', 'failed'))::int AS deploys,
		  COUNT(d.id) FILTER (WHERE d.status = 'failed')::int AS failed,
		  COUNT(d.id) FILTER (WHERE d.trigger = 'github')::int AS github,
		  COUNT(d.id) FILTER (WHERE d.status = 'success')::int AS live
		FROM projects p
		LEFT JOIN deployments d ON d.project_id = p.id
		WHERE p.user_id=$1
		GROUP BY p.id, p.name, p.created_at
		ORDER BY p.created_at DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActivityStats
	for rows.Next() {
		var item ActivityStats
		if err := rows.Scan(&item.ProjectID, &item.Name, &item.Deploys, &item.Failed, &item.GitHub, &item.Live); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if out == nil {
		out = []ActivityStats{}
	}
	return out, rows.Err()
}

func (s *Store) ListActivity(ctx context.Context, userID, projectID string, limit int, hour bool) ([]Deployment, error) {
	if limit <= 0 || limit > 300 {
		limit = 120
	}
	query := `
		SELECT d.id, d.project_id, COALESCE(d.node_id, ''), COALESCE(n.title, ''), p.name,
		       d.status, d.phase, COALESCE(d.trigger, 'manual'), d.source_type, d.source_ref,
		       d.framework, d.url, d.port, d.error, d.created_at, d.finished_at
		FROM deployments d
		JOIN projects p ON p.id = d.project_id
		LEFT JOIN nodes n ON n.id = d.node_id
		WHERE p.user_id=$1
	`
	args := []any{userID}
	if strings.TrimSpace(projectID) != "" {
		args = append(args, projectID)
		query += fmt.Sprintf(` AND d.project_id=$%d`, len(args))
	}
	// The activity timeline only needs the last hour, and it skips detect-only
	// stage rows. Project history keeps the unbounded query.
	if hour {
		query += ` AND d.status IN ('success', 'failed', 'running', 'building', 'cancelled')
			AND (d.created_at >= now() - interval '1 hour' OR d.finished_at >= now() - interval '1 hour')`
	}
	args = append(args, limit)
	query += fmt.Sprintf(` ORDER BY d.created_at DESC LIMIT $%d`, len(args))
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Deployment
	for rows.Next() {
		var d Deployment
		if err := rows.Scan(
			&d.ID, &d.ProjectID, &d.NodeID, &d.ServiceTitle, &d.ProjectName,
			&d.Status, &d.Phase, &d.Trigger, &d.SourceType, &d.SourceRef,
			&d.Framework, &d.URL, &d.Port, &d.Error, &d.CreatedAt, &d.FinishedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if out == nil {
		out = []Deployment{}
	}
	return out, rows.Err()
}

func (s *Store) OwnedDeployment(ctx context.Context, userID, deploymentID string) (Deployment, error) {
	var d Deployment
	err := s.pool.QueryRow(ctx, `
		SELECT d.id, d.project_id, COALESCE(d.node_id, ''), COALESCE(n.title, ''), p.name,
		       d.status, d.phase, COALESCE(d.trigger, 'manual'), d.source_type, d.source_ref,
		       d.framework, d.url, d.port, d.error, d.created_at, d.finished_at
		FROM deployments d
		JOIN projects p ON p.id = d.project_id
		LEFT JOIN nodes n ON n.id = d.node_id
		WHERE d.id=$1 AND p.user_id=$2
	`, deploymentID, userID).Scan(
		&d.ID, &d.ProjectID, &d.NodeID, &d.ServiceTitle, &d.ProjectName,
		&d.Status, &d.Phase, &d.Trigger, &d.SourceType, &d.SourceRef,
		&d.Framework, &d.URL, &d.Port, &d.Error, &d.CreatedAt, &d.FinishedAt,
	)
	return d, err
}

// ListRecentDeploymentLogs returns the latest lines for one deployment.
// The read is bounded by deployment_id so Activity never scans the log table.
func (s *Store) ListRecentDeploymentLogs(ctx context.Context, projectID, deploymentID string, limit int) ([]LogLine, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, project_id, deployment_id, node_id, ts, phase, level, text
		FROM deploy_logs
		WHERE project_id=$1 AND deployment_id=$2
		ORDER BY id DESC
		LIMIT $3
	`, projectID, deploymentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rev []LogLine
	for rows.Next() {
		var line LogLine
		if err := rows.Scan(&line.ID, &line.ProjectID, &line.DeploymentID, &line.NodeID, &line.TS, &line.Phase, &line.Level, &line.Text); err != nil {
			return nil, err
		}
		rev = append(rev, line)
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	if rev == nil {
		rev = []LogLine{}
	}
	return rev, rows.Err()
}

func (s *Store) OpenDeploymentID(ctx context.Context, projectID, nodeID string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		SELECT id FROM deployments
		WHERE project_id=$1 AND node_id=$2 AND finished_at IS NULL
		ORDER BY created_at DESC
		LIMIT 1
	`, projectID, nodeID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func (s *Store) FailOpenDeployments(ctx context.Context, projectID, msg string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE deployments
		SET status='failed', phase='start', error=$2, finished_at=COALESCE(finished_at, now())
		WHERE project_id=$1 AND finished_at IS NULL AND status='running'
	`, projectID, msg)
	return err
}

func (s *Store) AppendLog(ctx context.Context, line LogLine) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO deploy_logs (project_id, deployment_id, node_id, ts, phase, level, text)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
	`, line.ProjectID, line.DeploymentID, line.NodeID, line.TS, line.Phase, line.Level, line.Text)
	return err
}

type LogQuery struct {
	ProjectID    string
	NodeID       string
	DeploymentID string
	Since        time.Time
	Limit        int
}

func (s *Store) ListLogs(ctx context.Context, projectID string, limit int) ([]LogLine, error) {
	return s.QueryLogs(ctx, LogQuery{ProjectID: projectID, Limit: limit})
}

func (s *Store) QueryLogs(ctx context.Context, q LogQuery) ([]LogLine, error) {
	if q.Limit <= 0 || q.Limit > 2000 {
		q.Limit = 500
	}
	query := `
		SELECT id, project_id, deployment_id, node_id, ts, phase, level, text
		FROM deploy_logs
		WHERE project_id=$1
	`
	args := []any{q.ProjectID}
	if strings.TrimSpace(q.DeploymentID) != "" {
		args = append(args, q.DeploymentID)
		query += fmt.Sprintf(` AND deployment_id=$%d`, len(args))
	}
	if strings.TrimSpace(q.NodeID) != "" {
		args = append(args, q.NodeID)
		query += fmt.Sprintf(` AND (node_id=$%d OR node_id='')`, len(args))
	}
	if !q.Since.IsZero() {
		args = append(args, q.Since)
		query += fmt.Sprintf(` AND ts >= $%d`, len(args))
	}
	args = append(args, q.Limit)
	query += fmt.Sprintf(` ORDER BY id DESC LIMIT $%d`, len(args))
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rev []LogLine
	for rows.Next() {
		var line LogLine
		if err := rows.Scan(&line.ID, &line.ProjectID, &line.DeploymentID, &line.NodeID, &line.TS, &line.Phase, &line.Level, &line.Text); err != nil {
			return nil, err
		}
		rev = append(rev, line)
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	if rev == nil {
		rev = []LogLine{}
	}
	return rev, rows.Err()
}

func scanNode(row interface{ Scan(dest ...any) error }) (Node, error) {
	var n Node
	var languages, stack []byte
	err := row.Scan(
		&n.ID, &n.ProjectID, &n.Kind, &n.Title, &n.Caption, &n.X, &n.Y, &n.Status, &n.ContainerName,
		&n.URL, &n.Port, &n.Framework, &n.SourceType, &n.SourceRef, &n.Error, &languages, &stack,
		&n.GitHubInstallationID, &n.GitHubBranch, &n.AutoDeploy, &n.MemoryMB, &n.DiskMB,
		&n.PlatformURLRevoked, &n.PlatformURLUserSet, &n.PlatformSlug,
	)
	if err != nil {
		return n, err
	}
	n.PlatformURLEnabled = !n.PlatformURLRevoked
	n.MemoryMB, n.DiskMB = alloc.Resolve(n.Kind, n.MemoryMB, n.DiskMB, alloc.MaxMemoryMB, alloc.MaxDiskMB)
	if len(languages) > 0 {
		_ = json.Unmarshal(languages, &n.Languages)
	}
	if len(stack) > 0 {
		_ = json.Unmarshal(stack, &n.Stack)
	}
	return n, nil
}

func scanProject(row interface{ Scan(dest ...any) error }) (Project, error) {
	var p Project
	var languages, stack []byte
	err := row.Scan(
		&p.ID, &p.Name, &p.Subtitle, &p.Status, &p.Framework, &p.URL, &p.Port, &p.ContainerName,
		&p.SourceType, &p.SourceRef, &p.Error, &languages, &stack, &p.CreatedAt, &p.UpdatedAt,
		&p.Environment, &p.FamilyID,
	)
	if err != nil {
		return p, err
	}
	p.Environment = NormalizeEnv(p.Environment)
	if strings.TrimSpace(p.FamilyID) == "" {
		p.FamilyID = p.ID
	}
	if len(languages) > 0 {
		_ = json.Unmarshal(languages, &p.Languages)
	}
	if len(stack) > 0 {
		_ = json.Unmarshal(stack, &p.Stack)
	}
	return p, nil
}

func mustJSON(v any) []byte {
	if v == nil {
		return []byte("[]")
	}
	raw, err := json.Marshal(v)
	if err != nil || string(raw) == "null" {
		return []byte("[]")
	}
	return raw
}

func (s *Store) ListAllVars(ctx context.Context, projectID string) ([]EnvVar, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, node_id, key, value, source FROM project_vars
		WHERE project_id=$1
		ORDER BY node_id, key
	`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EnvVar
	for rows.Next() {
		var v EnvVar
		if err := rows.Scan(&v.ID, &v.NodeID, &v.Key, &v.Value, &v.Source); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if out == nil {
		out = []EnvVar{}
	}
	return out, rows.Err()
}

func (s *Store) ListVars(ctx context.Context, projectID, nodeID string) ([]EnvVar, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, node_id, key, value, source FROM project_vars
		WHERE project_id=$1 AND node_id=$2
		ORDER BY key
	`, projectID, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]EnvVar{}
	var order []string
	for rows.Next() {
		var v EnvVar
		if err := rows.Scan(&v.ID, &v.NodeID, &v.Key, &v.Value, &v.Source); err != nil {
			return nil, err
		}
		prev, exists := seen[v.Key]
		if !exists {
			order = append(order, v.Key)
			seen[v.Key] = v
			continue
		}
		if prev.NodeID != nodeID && v.NodeID == nodeID {
			seen[v.Key] = v
		}
	}
	out := make([]EnvVar, 0, len(order))
	for _, key := range order {
		out = append(out, seen[key])
	}
	return out, rows.Err()
}

func (s *Store) UpsertVar(ctx context.Context, projectID string, v EnvVar) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO project_vars (id, project_id, node_id, key, value, source)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (project_id, node_id, key) DO UPDATE SET
		  value=CASE
		    WHEN project_vars.source IN ('user', 'platform') AND EXCLUDED.source='detected' THEN project_vars.value
		    WHEN EXCLUDED.source='detected' AND COALESCE(project_vars.value, '') <> '' THEN project_vars.value
		    ELSE EXCLUDED.value
		  END,
		  source=CASE
		    WHEN project_vars.source IN ('user', 'platform') AND EXCLUDED.source='detected' THEN project_vars.source
		    WHEN EXCLUDED.source='detected' AND COALESCE(project_vars.value, '') <> '' THEN project_vars.source
		    ELSE EXCLUDED.source
		  END
	`, v.ID, projectID, v.NodeID, v.Key, v.Value, v.Source)
	return err
}

// UpsertVars writes a whole set in one statement. Writing platform connection
// keys one at a time cost one network round trip per key, which is what made
// attaching a database feel slow.
func (s *Store) UpsertVars(ctx context.Context, projectID string, vars []EnvVar) error {
	if len(vars) == 0 {
		return nil
	}
	ids := make([]string, len(vars))
	nodeIDs := make([]string, len(vars))
	keys := make([]string, len(vars))
	values := make([]string, len(vars))
	sources := make([]string, len(vars))
	for i, v := range vars {
		ids[i] = v.ID
		nodeIDs[i] = v.NodeID
		keys[i] = v.Key
		values[i] = v.Value
		sources[i] = v.Source
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO project_vars (id, project_id, node_id, key, value, source)
		SELECT u.id, $2, u.node_id, u.key, u.value, u.source
		FROM unnest($1::text[], $3::text[], $4::text[], $5::text[], $6::text[])
		  AS u(id, node_id, key, value, source)
		ON CONFLICT (project_id, node_id, key) DO UPDATE SET
		  value=CASE
		    WHEN project_vars.source IN ('user', 'platform') AND EXCLUDED.source='detected' THEN project_vars.value
		    WHEN EXCLUDED.source='detected' AND COALESCE(project_vars.value, '') <> '' THEN project_vars.value
		    ELSE EXCLUDED.value
		  END,
		  source=CASE
		    WHEN project_vars.source IN ('user', 'platform') AND EXCLUDED.source='detected' THEN project_vars.source
		    WHEN EXCLUDED.source='detected' AND COALESCE(project_vars.value, '') <> '' THEN project_vars.source
		    ELSE EXCLUDED.source
		  END
	`, ids, projectID, nodeIDs, keys, values, sources)
	return err
}

func (s *Store) ReplaceVars(ctx context.Context, projectID, nodeID string, vars []EnvVar) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM project_vars WHERE project_id=$1 AND node_id=$2`, projectID, nodeID); err != nil {
		return err
	}
	for _, v := range vars {
		if v.NodeID == "" {
			v.NodeID = nodeID
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO project_vars (id, project_id, node_id, key, value, source)
			VALUES ($1,$2,$3,$4,$5,$6)
		`, v.ID, projectID, v.NodeID, v.Key, v.Value, v.Source); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) MergeUserVars(ctx context.Context, projectID, nodeID string, vars []EnvVar) (added, updated, skipped int, err error) {
	existing, err := s.ListVars(ctx, projectID, nodeID)
	if err != nil {
		return 0, 0, 0, err
	}
	byKey := map[string]EnvVar{}
	for _, item := range existing {
		byKey[item.Key] = item
	}
	for _, item := range vars {
		prev, ok := byKey[item.Key]
		if ok && prev.Source == "platform" {
			skipped++
			continue
		}
		next := item
		next.NodeID = nodeID
		next.Source = "user"
		if next.ID == "" {
			next.ID = ident.New("var_")
		}
		if err := s.UpsertVar(ctx, projectID, next); err != nil {
			return added, updated, skipped, err
		}
		if ok {
			updated++
		} else {
			added++
		}
	}
	return added, updated, skipped, nil
}

func (s *Store) DeleteVar(ctx context.Context, projectID, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM project_vars WHERE project_id=$1 AND id=$2`, projectID, id)
	return err
}

func (s *Store) UpsertInstallation(ctx context.Context, inst Installation) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO github_installations (id, account, account_type, user_id, updated_at)
		VALUES ($1,$2,$3,NULLIF($4,''),now())
		ON CONFLICT (id) DO UPDATE SET
		  account=EXCLUDED.account,
		  account_type=EXCLUDED.account_type,
		  user_id=COALESCE(github_installations.user_id, EXCLUDED.user_id),
		  updated_at=now()
	`, inst.ID, inst.Account, inst.Type, inst.UserID)
	return err
}

func (s *Store) ListInstallations(ctx context.Context, userID string) ([]Installation, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.id, i.account, i.account_type, i.created_at, i.updated_at
		FROM github_user_installations u
		JOIN github_installations i ON i.id = u.installation_id
		WHERE u.user_id=$1
		ORDER BY i.updated_at DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Installation
	for rows.Next() {
		var item Installation
		if err := rows.Scan(&item.ID, &item.Account, &item.Type, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if out == nil {
		out = []Installation{}
	}
	return out, rows.Err()
}

func (s *Store) DeleteInstallation(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM github_installations WHERE id=$1`, id)
	return err
}

func (s *Store) ListAutoDeployNodes(ctx context.Context, installationID int64, fullName, branch string) ([]Node, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+nodeCols+` FROM nodes
		WHERE kind='service'
		  AND auto_deploy = true
		  AND source_type = 'github'
		  AND lower(source_ref) = lower($1)
		  AND github_installation_id = $2
		  AND (
		    github_branch IS NULL OR github_branch = '' OR github_branch = $3
		    OR github_branch = 'refs/heads/' || $3
		  )
	`, fullName, installationID, branch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if out == nil {
		out = []Node{}
	}
	return out, rows.Err()
}

func (s *Store) ListGitHubNodes(ctx context.Context, installationID int64, fullName string) ([]Node, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+nodeCols+` FROM nodes
		WHERE kind='service'
		  AND source_type = 'github'
		  AND lower(source_ref) = lower($1)
		  AND github_installation_id = $2
	`, fullName, installationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if out == nil {
		out = []Node{}
	}
	return out, rows.Err()
}

func Encode(v any) []byte {
	raw, _ := json.Marshal(v)
	return raw
}

var ErrNotFound = pgx.ErrNoRows
