package store

import "context"

// AdminWorkload is one deployment row for the administrator directory.
// NodeID is empty when a project has no services yet.
type AdminWorkload struct {
	UserID          string
	ProjectID       string
	ProjectName     string
	NodeID          string
	Kind            string
	PlatformSlug    string
	PlatformRevoked bool
}

// AdminHost is a custom domain attached to a project.
type AdminHost struct {
	UserID    string
	ProjectID string
	Hostname  string
	Status    string
}

func (s *Store) ListAdminWorkloads(ctx context.Context) ([]AdminWorkload, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.user_id, p.id, p.name,
		       COALESCE(n.id, ''), COALESCE(n.kind, ''),
		       COALESCE(n.platform_slug, ''), COALESCE(n.platform_url_revoked, false)
		FROM projects p
		LEFT JOIN nodes n ON n.project_id = p.id
		WHERE p.user_id IS NOT NULL AND p.user_id <> ''
		ORDER BY lower(p.name), n.created_at
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminWorkload{}
	for rows.Next() {
		var item AdminWorkload
		if err := rows.Scan(&item.UserID, &item.ProjectID, &item.ProjectName, &item.NodeID, &item.Kind, &item.PlatformSlug, &item.PlatformRevoked); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) ListAdminHosts(ctx context.Context) ([]AdminHost, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT user_id, project_id, hostname, status
		FROM custom_domains
		ORDER BY hostname
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AdminHost{}
	for rows.Next() {
		var item AdminHost
		if err := rows.Scan(&item.UserID, &item.ProjectID, &item.Hostname, &item.Status); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
