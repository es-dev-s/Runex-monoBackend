package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"deploy_platform/internal/deploy"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
)

func (s *Server) loadDatabaseNode(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	project, err := s.loadProject(w, r)
	if err != nil {
		return "", "", false
	}
	node, err := s.store.GetNode(r.Context(), project.ID, r.PathValue("nodeId"))
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "Node not found")
		return "", "", false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", "Could not load node")
		return "", "", false
	}
	if node.Kind != "postgresql" && node.Kind != "redis" {
		writeErr(w, http.StatusBadRequest, "invalid_request", "This service is not a database")
		return "", "", false
	}
	return project.ID, node.ID, true
}

func (s *Server) writeDatabaseErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, deploy.ErrNotDatabase):
		writeErr(w, http.StatusBadRequest, "invalid_request", "This service is not a database")
	case errors.Is(err, deploy.ErrDatabaseStopped):
		writeErr(w, http.StatusConflict, "database_stopped", "Start the database to browse its data")
	case errors.Is(err, redis.Nil):
		writeErr(w, http.StatusNotFound, "not_found", "Key not found")
	case err != nil && strings.Contains(strings.ToLower(err.Error()), "invalid identifier"):
		writeErr(w, http.StatusBadRequest, "invalid_request", "That table or column name is not allowed")
	case err != nil && strings.Contains(strings.ToLower(err.Error()), "unknown sort"):
		writeErr(w, http.StatusBadRequest, "invalid_request", "Cannot sort by that column")
	default:
		writeErr(w, http.StatusBadGateway, "database", "Could not query the isolated database")
	}
}

func (s *Server) databaseOverview(w http.ResponseWriter, r *http.Request) {
	projectID, nodeID, ok := s.loadDatabaseNode(w, r)
	if !ok {
		return
	}
	out, err := s.engine.DatabaseOverview(r.Context(), projectID, nodeID)
	if err != nil {
		s.writeDatabaseErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) databaseTables(w http.ResponseWriter, r *http.Request) {
	projectID, nodeID, ok := s.loadDatabaseNode(w, r)
	if !ok {
		return
	}
	tables, err := s.engine.DatabaseTables(r.Context(), projectID, nodeID)
	if err != nil {
		s.writeDatabaseErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tables": tables})
}

func (s *Server) databaseRows(w http.ResponseWriter, r *http.Request) {
	projectID, nodeID, ok := s.loadDatabaseNode(w, r)
	if !ok {
		return
	}
	table := strings.TrimSpace(r.URL.Query().Get("table"))
	if table == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request", "table is required")
		return
	}
	schema := strings.TrimSpace(r.URL.Query().Get("schema"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	page, err := s.engine.DatabaseRows(
		r.Context(), projectID, nodeID, schema, table, limit, offset,
		r.URL.Query().Get("sort"), r.URL.Query().Get("q"), r.URL.Query().Get("after"),
	)
	if err != nil {
		s.writeDatabaseErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) databaseKeys(w http.ResponseWriter, r *http.Request) {
	projectID, nodeID, ok := s.loadDatabaseNode(w, r)
	if !ok {
		return
	}
	cursor, _ := strconv.ParseUint(r.URL.Query().Get("cursor"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	page, err := s.engine.DatabaseKeys(r.Context(), projectID, nodeID, r.URL.Query().Get("match"), cursor, limit)
	if err != nil {
		s.writeDatabaseErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) databaseKey(w http.ResponseWriter, r *http.Request) {
	projectID, nodeID, ok := s.loadDatabaseNode(w, r)
	if !ok {
		return
	}
	key := r.URL.Query().Get("name")
	if strings.TrimSpace(key) == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request", "name is required")
		return
	}
	value, err := s.engine.DatabaseKey(r.Context(), projectID, nodeID, key)
	if err != nil {
		s.writeDatabaseErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}
