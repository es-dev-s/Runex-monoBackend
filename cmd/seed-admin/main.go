// Seed the predefined administrator from the environment or from .admin-seed.
// The password is never written into source and is never printed.
package main

import (
	"bufio"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"deploy_platform/internal/api"
	"deploy_platform/internal/config"
	"deploy_platform/internal/db"
	"deploy_platform/internal/store"
)

func main() {
	loadAdminSeed()
	cfg := config.Load()
	username := strings.TrimSpace(os.Getenv("RUNEX_ADMIN_USERNAME"))
	email := strings.TrimSpace(os.Getenv("RUNEX_ADMIN_EMAIL"))
	password := os.Getenv("RUNEX_ADMIN_PASSWORD")
	if username == "" || email == "" || password == "" {
		log.Fatal("set RUNEX_ADMIN_USERNAME, RUNEX_ADMIN_EMAIL, and RUNEX_ADMIN_PASSWORD, or provide backend/.admin-seed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatal("postgres: ", err)
	}
	defer pool.Close()

	created, err := api.SeedAdmin(ctx, store.New(pool), username, email, password)
	if err != nil {
		log.Fatal(err)
	}
	action := "updated"
	if created {
		action = "created"
	}
	log.Printf("administrator %s username=%s email=%s", action, username, email)
}

// loadAdminSeed fills missing admin variables from a gitignored file.
// Values are taken after the first "=" so a password may contain "#".
func loadAdminSeed() {
	wd, _ := os.Getwd()
	candidates := []string{".admin-seed"}
	if wd != "" {
		candidates = append(candidates, filepath.Join(wd, ".admin-seed"), filepath.Join(wd, "backend", ".admin-seed"))
	}
	seen := map[string]struct{}{}
	for _, path := range candidates {
		abs, err := filepath.Abs(path)
		if err != nil {
			abs = path
		}
		if _, ok := seen[abs]; ok {
			continue
		}
		seen[abs] = struct{}{}
		file, err := os.Open(abs)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			key = strings.TrimSpace(key)
			if key == "" || os.Getenv(key) != "" {
				continue
			}
			value = strings.TrimSpace(value)
			if len(value) >= 2 {
				q := value[0]
				if (q == '"' || q == '\'') && value[len(value)-1] == q {
					value = value[1 : len(value)-1]
				}
			}
			_ = os.Setenv(key, value)
		}
		_ = file.Close()
	}
}
