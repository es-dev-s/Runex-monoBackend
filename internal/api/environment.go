package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"deploy_platform/internal/ident"
	"deploy_platform/internal/store"

	"github.com/jackc/pgx/v5"
)

func (s *Server) writeProjectBundleCtx(ctx context.Context, w http.ResponseWriter, status int, project store.Project) {
	nodes, _ := s.store.ListNodes(ctx, project.ID)
	if nodes == nil {
		nodes = []store.Node{}
	}
	s.hydrateLegacyService(ctx, project, nodes)
	nodes, _ = s.store.ListNodes(ctx, project.ID)
	nodes = s.stampBuildClocks(ctx, project.ID, s.exposeNodeURLs(ctx, nodes))
	writeJSON(w, status, map[string]any{
		"project": s.exposeProjectURL(project, nodes),
		"nodes":   nodes,
	})
}

func (s *Server) ensureEnvironment(w http.ResponseWriter, r *http.Request) {
	source, err := s.loadProject(w, r)
	if err != nil {
		return
	}
	var body struct {
		Environment string `json:"environment"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
	env := store.NormalizeEnv(body.Environment)
	family := store.FamilyOf(source)
	userID := userFrom(r).ID
	ctx := r.Context()

	found, err := s.store.GetOwnedByFamilyEnv(ctx, userID, family, env)
	if err == nil {
		s.writeProjectBundleCtx(ctx, w, http.StatusOK, found)
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusInternalServerError, "db", "Could not load environment")
		return
	}
	if env == store.EnvProduction {
		writeErr(w, http.StatusNotFound, "not_found", "Production environment not found")
		return
	}

	created, err := s.cloneEnvironment(ctx, userID, source, family, env)
	if err != nil {
		if store.IsUniqueViolation(err) {
			found, retryErr := s.store.GetOwnedByFamilyEnv(ctx, userID, family, env)
			if retryErr == nil {
				s.writeProjectBundleCtx(ctx, w, http.StatusOK, found)
				return
			}
		}
		writeErr(w, http.StatusInternalServerError, "db", "Could not create environment")
		return
	}
	s.writeProjectBundleCtx(ctx, w, http.StatusCreated, created)
}

func (s *Server) cloneEnvironment(ctx context.Context, userID string, source store.Project, family, env string) (store.Project, error) {
	template := source
	if prod, err := s.store.GetOwnedByFamilyEnv(ctx, userID, family, store.EnvProduction); err == nil {
		template = prod
	}

	created := store.Project{
		ID:          ident.New("prj_"),
		Name:        template.Name,
		Subtitle:    "Staging",
		Status:      "idle",
		UserID:      userID,
		Environment: env,
		FamilyID:    family,
	}

	nodes, err := s.store.ListNodes(ctx, template.ID)
	if err != nil {
		return store.Project{}, err
	}
	remap := map[string]string{}
	cloned := make([]store.Node, 0, len(nodes))
	for _, node := range nodes {
		next := store.Node{
			ID:                   ident.New(nodePrefix(node.Kind)),
			ProjectID:            created.ID,
			Kind:                 node.Kind,
			Title:                node.Title,
			Caption:              node.Caption,
			X:                    node.X,
			Y:                    node.Y,
			Status:               "idle",
			Framework:            node.Framework,
			SourceType:           node.SourceType,
			SourceRef:            node.SourceRef,
			Languages:            node.Languages,
			Stack:                node.Stack,
			GitHubInstallationID: node.GitHubInstallationID,
			GitHubBranch:         node.GitHubBranch,
			AutoDeploy:           false,
		}
		remap[node.ID] = next.ID
		cloned = append(cloned, next)
	}

	vars, err := s.store.ListAllVars(ctx, template.ID)
	if err != nil {
		return store.Project{}, err
	}
	clonedVars := make([]store.EnvVar, 0, len(vars))
	for _, item := range vars {
		if item.Source == "platform" {
			continue
		}
		nodeID := item.NodeID
		if mapped, ok := remap[nodeID]; ok {
			nodeID = mapped
		} else if nodeID != "" {
			continue
		}
		clonedVars = append(clonedVars, store.EnvVar{
			ID:     ident.New("var_"),
			NodeID: nodeID,
			Key:    item.Key,
			Value:  item.Value,
			Source: item.Source,
		})
	}

	if err := s.store.CreateProjectWithResources(ctx, created, cloned, clonedVars); err != nil {
		return store.Project{}, err
	}

	loaded, err := s.store.GetProject(ctx, created.ID)
	if err != nil {
		return created, err
	}
	return loaded, nil
}

func nodePrefix(kind string) string {
	switch kind {
	case "postgresql":
		return "nod_"
	case "redis":
		return "nod_"
	default:
		return "svc_"
	}
}

func (s *Server) purgeFamilyOrProject(ctx context.Context, userID string, project store.Project) error {
	ids := []string{project.ID}
	if store.NormalizeEnv(project.Environment) == store.EnvProduction {
		members, err := s.store.ListFamily(ctx, userID, store.FamilyOf(project))
		if err != nil {
			return err
		}
		ids = ids[:0]
		for _, member := range members {
			if store.NormalizeEnv(member.Environment) == store.EnvStaging {
				ids = append(ids, member.ID)
			}
		}
		for _, member := range members {
			if store.NormalizeEnv(member.Environment) != store.EnvStaging {
				ids = append(ids, member.ID)
			}
		}
	}
	var first error
	seen := map[string]struct{}{}
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		if err := s.purgeProject(ctx, id); err != nil && first == nil {
			first = err
		}
	}
	return first
}
