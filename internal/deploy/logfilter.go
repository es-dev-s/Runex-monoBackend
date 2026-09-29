package deploy

import (
	"regexp"
	"strings"
)

var hexID = regexp.MustCompile(`^[0-9a-f]{12,}$`)

func KeepDeployLog(level, text string) bool {
	line := strings.TrimSpace(text)
	if line == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "error", "warn", "cmd", "success":
		return true
	}
	lower := strings.ToLower(line)
	if strings.HasPrefix(lower, "--->") ||
		strings.HasPrefix(lower, "sha256:") ||
		strings.HasPrefix(lower, "digest:") ||
		strings.Contains(lower, "removing intermediate") {
		return false
	}
	if hexID.MatchString(lower) {
		return false
	}
	if strings.HasPrefix(line, "#") && (strings.Contains(lower, "transferring") || strings.Contains(lower, "listing ")) {
		return false
	}
	return true
}
