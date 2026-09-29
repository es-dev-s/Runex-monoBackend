package pack

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	goDirective  = regexp.MustCompile(`(?m)^go\s+v?(\d+\.\d+(?:\.\d+)?)`)
	goToolchain  = regexp.MustCompile(`(?m)^toolchain\s+go(\d+\.\d+(?:\.\d+)?)`)
	pythonPin    = regexp.MustCompile(`(?i)(?:python-?)?(\d+\.\d+(?:\.\d+)?)`)
	requiresPy   = regexp.MustCompile(`(?i)requires-python\s*=\s*["']([^"']+)["']`)
	versionToken = regexp.MustCompile(`\d+(?:\.\d+){0,2}`)
)

func resolveGoVersion(dir, detected string) string {
	body := readFile(filepath.Join(dir, "go.mod"))
	if m := goToolchain.FindStringSubmatch(body); len(m) > 1 {
		return m[1]
	}
	if m := goDirective.FindStringSubmatch(body); len(m) > 1 {
		return m[1]
	}
	return stripGoPrefix(detected)
}

func resolveNodeVersion(dir, detected string) string {
	for _, name := range []string{".nvmrc", ".node-version"} {
		if v := parseNodeVersionLine(readFile(filepath.Join(dir, name))); v != "" {
			return v
		}
	}
	if v := packageNodeVersion(dir); v != "" {
		return v
	}
	return strings.TrimSpace(detected)
}

func resolvePythonVersion(dir, detected string) string {
	if v := parsePythonVersionLine(readFile(filepath.Join(dir, ".python-version"))); v != "" {
		return v
	}
	if v := parsePythonVersionLine(readFile(filepath.Join(dir, "runtime.txt"))); v != "" {
		return v
	}
	body := readFile(filepath.Join(dir, "pyproject.toml"))
	if m := requiresPy.FindStringSubmatch(body); len(m) > 1 {
		if v := majorMinor(m[1], ""); v != "" {
			return v
		}
	}
	return strings.TrimSpace(detected)
}

func packageNodeVersion(dir string) string {
	var pkg struct {
		Engines map[string]string `json:"engines"`
		Volta   struct {
			Node string `json:"node"`
		} `json:"volta"`
	}
	_ = json.Unmarshal([]byte(readFile(filepath.Join(dir, "package.json"))), &pkg)
	if v := normalizeVersion(pkg.Engines["node"]); v != "" {
		return v
	}
	return normalizeVersion(pkg.Volta.Node)
}

func goImage(version string) string {
	return "golang:" + majorMinor(version, "1.24") + "-alpine"
}

func nodeImage(version, suffix string) string {
	tag := major(version, "22")
	return "node:" + tag + suffix
}

func pythonImage(version, suffix string) string {
	mm := majorMinor(version, "3.12")
	if !strings.Contains(mm, ".") {
		mm = "3.12"
	}
	return "python:" + mm + suffix
}

func parseNodeVersionLine(raw string) string {
	raw = firstLine(raw)
	raw = strings.TrimPrefix(raw, "v")
	switch strings.ToLower(raw) {
	case "lts", "lts/*", "lts/jod", "jod":
		return "22"
	case "lts/iron", "iron":
		return "20"
	case "lts/hydrogen", "hydrogen":
		return "18"
	case "lts/krypton", "krypton":
		return "24"
	}
	return normalizeVersion(raw)
}

func parsePythonVersionLine(raw string) string {
	raw = firstLine(raw)
	if m := pythonPin.FindStringSubmatch(raw); len(m) > 1 {
		return m[1]
	}
	return ""
}

func majorMinor(raw, fallback string) string {
	v := normalizeVersion(raw)
	if v == "" {
		return fallback
	}
	parts := strings.Split(v, ".")
	if len(parts) >= 2 && isDigits(parts[0]) && isDigits(parts[1]) {
		return parts[0] + "." + parts[1]
	}
	if len(parts) == 1 && isDigits(parts[0]) {
		return parts[0]
	}
	return fallback
}

func major(raw, fallback string) string {
	mm := majorMinor(raw, fallback)
	if i := strings.Index(mm, "."); i > 0 {
		return mm[:i]
	}
	if mm != "" {
		return mm
	}
	return fallback
}

func normalizeVersion(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "v")
	raw = strings.TrimPrefix(raw, "go")
	for _, prefix := range []string{"workspace:", "^", "~", ">=", "<=", ">", "<", "="} {
		raw = strings.TrimSpace(strings.TrimPrefix(raw, prefix))
	}
	if i := strings.IndexAny(raw, " ,|&"); i >= 0 {
		raw = raw[:i]
	}
	if !versionToken.MatchString(raw) {
		return ""
	}
	return versionToken.FindString(raw)
}

func stripGoPrefix(raw string) string {
	return strings.TrimPrefix(strings.TrimSpace(raw), "go")
}

func firstLine(raw string) string {
	raw = strings.TrimSpace(raw)
	if i := strings.IndexAny(raw, "\r\n"); i >= 0 {
		raw = raw[:i]
	}
	if i := strings.Index(raw, "#"); i >= 0 {
		raw = raw[:i]
	}
	return strings.TrimSpace(raw)
}

func firstFromImage(dockerfile string) string {
	for _, line := range strings.Split(dockerfile, "\n") {
		trim := strings.TrimSpace(line)
		if len(trim) < 5 || !strings.EqualFold(trim[:5], "from ") {
			continue
		}
		fields := strings.Fields(trim)
		if len(fields) < 2 {
			continue
		}
		return fields[1]
	}
	return ""
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
