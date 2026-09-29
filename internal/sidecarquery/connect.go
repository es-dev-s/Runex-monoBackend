package sidecarquery

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

type Creds struct {
	User     string
	Password string
	Database string
}

func PostgresCreds(env map[string]string) Creds {
	user := strings.TrimSpace(env["POSTGRES_USER"])
	if user == "" {
		user = "app"
	}
	pass := env["POSTGRES_PASSWORD"]
	if pass == "" {
		pass = "app"
	}
	db := strings.TrimSpace(env["POSTGRES_DB"])
	if db == "" {
		db = "app"
	}
	return Creds{User: user, Password: pass, Database: db}
}

func RedisPassword(env map[string]string) string {
	if env == nil {
		return ""
	}
	if p := env["REDIS_PASSWORD"]; p != "" {
		return p
	}
	return env["REQUIREPASS"]
}

func (c Creds) PostgresURL(host string, port int) string {
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(c.User, c.Password),
		Host:   net.JoinHostPort(host, fmt.Sprintf("%d", port)),
		Path:   "/" + c.Database,
	}
	return u.String()
}

func RedisURL(host string, port int, password string) string {
	u := &url.URL{
		Scheme: "redis",
		Host:   net.JoinHostPort(host, fmt.Sprintf("%d", port)),
		Path:   "/0",
	}
	if password != "" {
		u.User = url.UserPassword("", password)
	}
	return u.String()
}

type Table struct {
	Schema        string `json:"schema"`
	Name          string `json:"name"`
	EstimatedRows int64  `json:"estimatedRows"`
	Exact         bool   `json:"exact"`
	SizeBytes     int64  `json:"sizeBytes"`
	Size          string `json:"size"`
}

type Column struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
	PK       bool   `json:"pk"`
}

type RowPage struct {
	Columns       []Column         `json:"columns"`
	Rows          []map[string]any `json:"rows"`
	EstimatedRows int64            `json:"estimatedRows"`
	Exact         bool             `json:"exact"`
	Offset        int              `json:"offset"`
	Limit         int              `json:"limit"`
	HasMore       bool             `json:"hasMore"`
	Truncated     bool             `json:"truncated"`
	Keyset        bool             `json:"keyset"`
	NextAfter     string           `json:"nextAfter,omitempty"`
}

type KeyItem struct {
	Name string `json:"name"`
	Type string `json:"type"`
	TTL  int64  `json:"ttl"`
}

type KeyPage struct {
	Keys    []KeyItem `json:"keys"`
	Cursor  uint64    `json:"cursor"`
	HasMore bool      `json:"hasMore"`
}

type KeyValue struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	TTL       int64  `json:"ttl"`
	Value     any    `json:"value"`
	Truncated bool   `json:"truncated"`
}
