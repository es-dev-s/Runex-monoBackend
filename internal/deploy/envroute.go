package deploy

import (
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
)

type SidecarLink struct {
	Host     string
	Port     string
	User     string
	Password string
	Database string
}

func (s *SidecarLink) PostgresURL() string {
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(orDefault(s.User, "app"), orDefault(s.Password, "app")),
		Host:   net.JoinHostPort(s.Host, orDefault(s.Port, "5432")),
		Path:   "/" + orDefault(s.Database, "app"),
	}
	return u.String()
}

func (s *SidecarLink) RedisURL() string {
	u := &url.URL{
		Scheme: "redis",
		Host:   net.JoinHostPort(s.Host, orDefault(s.Port, "6379")),
		Path:   "/0",
	}
	if s.Password != "" {
		u.User = url.UserPassword("", s.Password)
	}
	return u.String()
}

func pinSidecarURLs(env map[string]string, postgres, redis *SidecarLink) []string {
	var changed []string
	if postgres != nil && postgres.Host != "" {
		pg := postgres.PostgresURL()
		for _, key := range []string{"DATABASE_URL", "POSTGRES_URL", "PLATFORM_DATABASE_URL", "DATABASE_PRIVATE_URL"} {
			if env[key] != pg {
				env[key] = pg
				changed = append(changed, key)
			}
		}
	}
	if redis != nil && redis.Host != "" {
		ru := redis.RedisURL()
		for _, key := range []string{"REDIS_URL", "PLATFORM_REDIS_URL", "REDIS_PRIVATE_URL"} {
			if env[key] != ru {
				env[key] = ru
				changed = append(changed, key)
			}
		}
	}
	changed = append(changed, rewritePrivateServiceURLs(env, postgres, redis)...)
	sort.Strings(changed)
	return uniqueStrings(changed)
}

func rewritePrivateServiceURLs(env map[string]string, postgres, redis *SidecarLink) []string {
	var changed []string
	for key, value := range env {
		next, ok := rewritePrivateServiceURL(value, postgres, redis)
		if !ok {
			continue
		}
		env[key] = next
		changed = append(changed, key)
	}
	return changed
}

func rewritePrivateServiceURL(value string, postgres, redis *SidecarLink) (string, bool) {
	raw := strings.TrimSpace(value)
	pgHost, rdHost := "", ""
	if postgres != nil {
		pgHost = postgres.Host
	}
	if redis != nil {
		rdHost = redis.Host
	}
	if raw == "" || !hasPrivateMeshHost(raw) {
		return value, false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return value, false
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case pgHost != "" && postgresURL(u.Scheme, host):
		return postgres.PostgresURL(), true
	case rdHost != "" && redisURL(u.Scheme, host):
		return redis.RedisURL(), true
	default:
		return value, false
	}
}

func unreachablePrivateKeys(env map[string]string) []string {
	var keys []string
	for key, value := range env {
		if hasPrivateMeshHost(value) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func privateMeshError(keys []string) error {
	return fmt.Errorf(
		"%s use Railway private DNS, which cannot resolve here. Attach PostgreSQL/Redis on the canvas, then Deploy again",
		strings.Join(keys, ", "),
	)
}

func hasPrivateMeshHost(value string) bool {
	v := strings.ToLower(value)
	return strings.Contains(v, ".railway.internal") || strings.Contains(v, ".rlwy.internal")
}

func postgresURL(scheme, host string) bool {
	s := strings.ToLower(scheme)
	if s == "postgres" || s == "postgresql" {
		return true
	}
	return strings.Contains(host, "postgres") || strings.HasPrefix(host, "pg.") || strings.Contains(host, ".pg.")
}

func redisURL(scheme, host string) bool {
	if strings.ToLower(scheme) == "redis" || strings.ToLower(scheme) == "rediss" {
		return true
	}
	return strings.Contains(host, "redis")
}

func uniqueStrings(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, item := range in {
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
