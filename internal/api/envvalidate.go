package api

import (
	"net/url"
	"strings"
)

// validateUserEnvValue rejects values that will crash a deploy when applied.
// Runex-managed DATABASE_URL from linked Postgres is injected separately;
// a user override must still be a real postgres URL if the key is database-ish.
func validateUserEnvValue(key, value string) string {
	k := strings.ToUpper(strings.TrimSpace(key))
	v := strings.TrimSpace(value)
	switch k {
	case "DATABASE_URL", "POSTGRES_URL", "POSTGRESQL_URL", "PLATFORM_DATABASE_URL", "DATABASE_PRIVATE_URL", "DATABASE_PUBLIC_URL":
		if v == "" {
			return ""
		}
		if strings.ContainsAny(v, " \t\n\r") {
			return "DATABASE_URL cannot contain spaces. Use a full postgres:// URL from Runex Postgres or your provider."
		}
		u, err := url.Parse(v)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return "DATABASE_URL must be a valid postgres:// or postgresql:// URL."
		}
		switch strings.ToLower(u.Scheme) {
		case "postgres", "postgresql":
			return ""
		default:
			return "DATABASE_URL must use the postgres:// or postgresql:// scheme."
		}
	default:
		return ""
	}
}
