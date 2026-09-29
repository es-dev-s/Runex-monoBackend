package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

func loadDotEnv() {
	wd, _ := os.Getwd()
	candidates := []string{".env"}
	if wd != "" {
		candidates = append(candidates, filepath.Join(wd, ".env"), filepath.Join(wd, "backend", ".env"))
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
			_ = os.Setenv(key, strings.ReplaceAll(value, `\n`, "\n"))
		}
		_ = file.Close()
	}
}
