package api

import "testing"

func TestValidateUserEnvValueDatabaseURL(t *testing.T) {
	cases := []struct {
		key, val string
		wantErr  bool
	}{
		{"DATABASE_URL", "postgres://u:p@postgres:5432/app", false},
		{"DATABASE_URL", "postgresql://u:p@h/db", false},
		{"DATABASE_URL", "Podghvvvhb hunch", true},
		{"DATABASE_URL", "mysql://x", true},
		{"DATABASE_URL", "", false},
		{"API_KEY", "not-a-url", false},
	}
	for _, c := range cases {
		err := validateUserEnvValue(c.key, c.val)
		if c.wantErr && err == "" {
			t.Fatalf("%s=%q expected error", c.key, c.val)
		}
		if !c.wantErr && err != "" {
			t.Fatalf("%s=%q unexpected error: %s", c.key, c.val, err)
		}
	}
}
