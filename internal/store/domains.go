package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrDomainTaken = errors.New("domain taken")
	ErrDomainLimit = errors.New("domain limit")
)

type CustomDomain struct {
	ID          string     `json:"id"`
	UserID      string     `json:"-"`
	ProjectID   string     `json:"projectId"`
	NodeID      string     `json:"nodeId"`
	Hostname    string     `json:"hostname"`
	Status      string     `json:"status"`
	Token       string     `json:"-"`
	LastError   string     `json:"error,omitempty"`
	EdgeStatus  string     `json:"edgeStatus,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	VerifiedAt  *time.Time `json:"verifiedAt,omitempty"`
	LastCheckAt *time.Time `json:"lastCheckAt,omitempty"`
}

const domainCols = `
	id, user_id, project_id, node_id, hostname, status, token, last_error, edge_status,
	created_at, verified_at, last_check_at`

func scanDomain(row interface{ Scan(dest ...any) error }) (CustomDomain, error) {
	var d CustomDomain
	err := row.Scan(
		&d.ID, &d.UserID, &d.ProjectID, &d.NodeID, &d.Hostname, &d.Status, &d.Token,
		&d.LastError, &d.EdgeStatus, &d.CreatedAt, &d.VerifiedAt, &d.LastCheckAt,
	)
	return d, err
}

func (s *Store) CreateCustomDomain(ctx context.Context, d CustomDomain, maxUser, maxNode int) (CustomDomain, error) {
	if maxUser <= 0 {
		maxUser = 20
	}
	if maxNode <= 0 {
		maxNode = 8
	}
	var userCount, nodeCount int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM custom_domains WHERE user_id=$1`, d.UserID).Scan(&userCount); err != nil {
		return CustomDomain{}, err
	}
	if userCount >= maxUser {
		return CustomDomain{}, ErrDomainLimit
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM custom_domains WHERE project_id=$1 AND node_id=$2`, d.ProjectID, d.NodeID).Scan(&nodeCount); err != nil {
		return CustomDomain{}, err
	}
	if nodeCount >= maxNode {
		return CustomDomain{}, ErrDomainLimit
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO custom_domains (
		  id, user_id, project_id, node_id, hostname, status, token, last_error, edge_status, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,now())
	`, d.ID, d.UserID, d.ProjectID, d.NodeID, d.Hostname, d.Status, d.Token, d.LastError, d.EdgeStatus)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return CustomDomain{}, ErrDomainTaken
		}
		return CustomDomain{}, err
	}
	return s.GetCustomDomain(ctx, d.UserID, d.ID)
}

func (s *Store) GetCustomDomain(ctx context.Context, userID, id string) (CustomDomain, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+domainCols+` FROM custom_domains WHERE id=$1 AND user_id=$2`, id, userID)
	return scanDomain(row)
}

func (s *Store) GetOwnedCustomDomain(ctx context.Context, userID, projectID, nodeID, id string) (CustomDomain, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+domainCols+`
		FROM custom_domains
		WHERE id=$1 AND user_id=$2 AND project_id=$3 AND node_id=$4
	`, id, userID, projectID, nodeID)
	return scanDomain(row)
}

func (s *Store) GetCustomDomainByHost(ctx context.Context, hostname string) (CustomDomain, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+domainCols+` FROM custom_domains WHERE hostname=$1`, strings.ToLower(strings.TrimSpace(hostname)))
	return scanDomain(row)
}

func (s *Store) GetActiveDomainByHost(ctx context.Context, hostname string) (CustomDomain, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+domainCols+`
		FROM custom_domains
		WHERE hostname=$1 AND status='active'
	`, strings.ToLower(strings.TrimSpace(hostname)))
	return scanDomain(row)
}

func (s *Store) ListCustomDomains(ctx context.Context, userID, projectID, nodeID string) ([]CustomDomain, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+domainCols+`
		FROM custom_domains
		WHERE user_id=$1 AND project_id=$2 AND node_id=$3
		ORDER BY created_at
	`, userID, projectID, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CustomDomain
	for rows.Next() {
		d, err := scanDomain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if out == nil {
		out = []CustomDomain{}
	}
	return out, rows.Err()
}

func (s *Store) ListCustomDomainsByProject(ctx context.Context, projectID string) ([]CustomDomain, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+domainCols+` FROM custom_domains WHERE project_id=$1 ORDER BY created_at
	`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CustomDomain
	for rows.Next() {
		d, err := scanDomain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) ListPendingCustomDomains(ctx context.Context, limit int) ([]CustomDomain, error) {
	if limit <= 0 {
		limit = 40
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+domainCols+`
		FROM custom_domains
		WHERE status IN ('pending', 'verifying', 'error')
		ORDER BY COALESCE(last_check_at, created_at)
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CustomDomain
	for rows.Next() {
		d, err := scanDomain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) SetCustomDomainCheck(ctx context.Context, id, status, lastError, edgeStatus string, verified bool) error {
	if status == "" {
		status = "pending"
	}
	if lastError == "" && status == "active" {
		lastError = ""
	}
	var verifiedAt any
	if verified {
		verifiedAt = time.Now()
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE custom_domains
		SET status=$2,
		    last_error=$3,
		    edge_status=CASE WHEN $4 = '' THEN edge_status ELSE $4 END,
		    last_check_at=now(),
		    verified_at=COALESCE($5, verified_at)
		WHERE id=$1
	`, id, status, lastError, edgeStatus, verifiedAt)
	return err
}

func (s *Store) SetCustomDomainEdge(ctx context.Context, id, edgeStatus string) error {
	_, err := s.pool.Exec(ctx, `UPDATE custom_domains SET edge_status=$2 WHERE id=$1`, id, edgeStatus)
	return err
}

func (s *Store) PrimaryCustomHosts(ctx context.Context, nodeIDs []string) (map[string]string, error) {
	out := map[string]string{}
	if len(nodeIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (node_id) node_id, hostname
		FROM custom_domains
		WHERE status='active' AND node_id = ANY($1)
		ORDER BY node_id, created_at ASC
	`, nodeIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var nodeID, host string
		if err := rows.Scan(&nodeID, &host); err != nil {
			return nil, err
		}
		out[nodeID] = host
	}
	return out, rows.Err()
}

func (s *Store) CountActiveCustomDomains(ctx context.Context, nodeID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM custom_domains WHERE node_id=$1 AND status='active'
	`, nodeID).Scan(&n)
	return n, err
}

func (s *Store) SetPlatformURL(ctx context.Context, nodeID string, enabled, userSet bool) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE nodes
		SET platform_url_revoked=$2, platform_url_user_set=$3
		WHERE id=$1
	`, nodeID, !enabled, userSet)
	return err
}

func (s *Store) AutoRevokePlatformURL(ctx context.Context, nodeID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE nodes
		SET platform_url_revoked=true
		WHERE id=$1 AND COALESCE(platform_url_user_set, false)=false
	`, nodeID)
	return err
}

func (s *Store) AutoRevokePlatformURLsForActiveCustom(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE nodes n
		SET platform_url_revoked=true
		WHERE COALESCE(n.platform_url_user_set, false)=false
		  AND EXISTS (
		    SELECT 1 FROM custom_domains d
		    WHERE d.node_id = n.id AND d.status = 'active'
		  )
	`)
	return err
}

func (s *Store) RestorePlatformURLIfUnused(ctx context.Context, nodeID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE nodes
		SET platform_url_revoked=false, platform_url_user_set=false
		WHERE id=$1
		  AND NOT EXISTS (
		    SELECT 1 FROM custom_domains WHERE node_id=$1 AND status='active'
		  )
	`, nodeID)
	return err
}

func (s *Store) DeleteCustomDomain(ctx context.Context, userID, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM custom_domains WHERE id=$1 AND user_id=$2`, id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
