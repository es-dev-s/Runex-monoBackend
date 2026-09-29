package envfile

import (
	"crypto/rand"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

type Pair struct {
	Key   string
	Value string
}

var codeEnvPatterns = []*regexp.Regexp{
	regexp.MustCompile(`process\.env\.([A-Z][A-Z0-9_]{1,64})`),
	regexp.MustCompile(`process\.env\[['"]([A-Z][A-Z0-9_]{1,64})['"]\]`),
	regexp.MustCompile(`os\.Getenv\(['"]([A-Z][A-Z0-9_]{1,64})['"]\)`),
	regexp.MustCompile(`os\.LookupEnv\(['"]([A-Z][A-Z0-9_]{1,64})['"]\)`),
	regexp.MustCompile(`os\.getenv\(['"]([A-Z][A-Z0-9_]{1,64})['"]\)`),
	regexp.MustCompile(`os\.environ(?:\.get)?\(['"]([A-Z][A-Z0-9_]{1,64})['"]\)`),
	regexp.MustCompile(`env\(['"]([A-Z][A-Z0-9_]{1,64})['"]`),
	regexp.MustCompile(`std::env::var(?:_os)?\(['"]([A-Z][A-Z0-9_]{1,64})['"]\)`),
	regexp.MustCompile(`ENV\[['"]([A-Z][A-Z0-9_]{1,64})['"]\]`),
}

var skipCodeKeys = map[string]struct{}{
	"PATH": {}, "HOME": {}, "USER": {}, "PWD": {}, "SHELL": {}, "TERM": {},
	"TMPDIR": {}, "LANG": {}, "EDITOR": {}, "HOSTNAME": {}, "OLDPWD": {},
	"NODE_ENV": {}, "CI": {}, "COLORTERM": {}, "XDG_CONFIG_HOME": {},
	"APPDATA": {}, "LOCALAPPDATA": {}, "USERPROFILE": {}, "COMPUTERNAME": {},
	"PROGRAMFILES": {}, "WINDIR": {}, "TEMP": {}, "TMP": {}, "OS": {},
	"PATHEXT": {}, "SYSTEMROOT": {}, "ALLUSERSPROFILE": {}, "PROGRAMDATA": {},
	"PUBLIC": {}, "COMSPEC": {}, "HOMEDRIVE": {}, "HOMEPATH": {},
	"LOGNAME": {}, "USERNAME": {}, "USERDOMAIN": {},
}

func IsNoiseKey(key string) bool {
	_, ok := skipCodeKeys[key]
	return ok
}

func Discover(root string) []Pair {
	seen := map[string]Pair{}
	root = filepath.Clean(root)
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipWalkDir(d.Name()) && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !isDummyEnvFile(d.Name()) {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil || len(data) > 256<<10 {
			return nil
		}
		for _, pair := range Parse(string(data)) {
			if _, ok := seen[pair.Key]; ok {
				continue
			}
			seen[pair.Key] = pair
		}
		return nil
	})
	for _, key := range scanCodeKeys(root) {
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = Pair{Key: key}
	}
	out := make([]Pair, 0, len(seen))
	for _, pair := range seen {
		out = append(out, pair)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func Parse(raw string) []Pair {
	raw = strings.TrimPrefix(raw, "\ufeff")
	var out []Pair
	index := map[string]int{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		if strings.HasPrefix(trim, "export ") {
			trim = strings.TrimSpace(strings.TrimPrefix(trim, "export "))
		}
		key, value, ok := splitKV(trim)
		if !ok || !validKey(key) {
			continue
		}
		if i, exists := index[key]; exists {
			out[i].Value = value
			continue
		}
		index[key] = len(out)
		out = append(out, Pair{Key: key, Value: value})
	}
	return out
}

func Format(pairs []Pair) string {
	var b strings.Builder
	for _, pair := range pairs {
		if !validKey(pair.Key) {
			continue
		}
		b.WriteString(pair.Key)
		b.WriteByte('=')
		b.WriteString(quoteIfNeeded(pair.Value))
		b.WriteByte('\n')
	}
	return b.String()
}

func Write(path string, pairs []Pair) error {
	return os.WriteFile(path, []byte(Format(pairs)), 0o600)
}

func RandomSecret() string {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		fallback := make([]byte, 32)
		return hex.EncodeToString(fallback)
	}
	return hex.EncodeToString(raw)
}

func IsAuthSecretKey(key string) bool {
	switch strings.ToUpper(strings.TrimSpace(key)) {
	case "AUTH_SECRET", "NEXTAUTH_SECRET":
		return true
	default:
		return false
	}
}

func scanCodeKeys(root string) []string {
	used := map[string]struct{}{}
	var keys []string
	var files int
	type found struct {
		path     string
		priority bool
	}
	var list []found
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipWalkDir(d.Name()) && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !isCodeFile(d.Name()) {
			return nil
		}
		name := strings.ToLower(d.Name())
		priority := strings.Contains(name, "env") || strings.Contains(name, "config")
		list = append(list, found{path: path, priority: priority})
		return nil
	})
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].priority == list[j].priority {
			return false
		}
		return list[i].priority
	})
	for _, item := range list {
		if files >= 800 {
			break
		}
		data, readErr := os.ReadFile(item.path)
		if readErr != nil || len(data) > 256<<10 {
			continue
		}
		files++
		body := string(data)
		for _, pattern := range codeEnvPatterns {
			for _, match := range pattern.FindAllStringSubmatch(body, 40) {
				if len(match) < 2 {
					continue
				}
				key := match[1]
				if _, skip := skipCodeKeys[key]; skip || !validKey(key) {
					continue
				}
				if _, exists := used[key]; exists {
					continue
				}
				used[key] = struct{}{}
				keys = append(keys, key)
			}
		}
	}
	sort.Strings(keys)
	return keys
}

func isDummyEnvFile(name string) bool {
	lower := strings.ToLower(name)
	if lower == "env.example" {
		return true
	}
	if !strings.HasPrefix(lower, ".env") {
		return false
	}
	switch lower {
	case ".env", ".env.local", ".env.development", ".env.production", ".env.test", ".env.staging":
		return false
	}
	return strings.Contains(lower, "example") ||
		strings.Contains(lower, "sample") ||
		strings.Contains(lower, "template") ||
		strings.Contains(lower, "dummy")
}

func isCodeFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs", ".py", ".go", ".rs", ".php", ".rb", ".java", ".kt":
		return true
	default:
		return false
	}
}

func skipWalkDir(name string) bool {
	switch name {
	case "node_modules", ".git", "dist", "build", "out", "vendor", "target", ".next", ".turbo", "coverage", "__pycache__":
		return true
	default:
		return strings.HasPrefix(name, ".") && name != ".env.example" && !strings.HasPrefix(name, ".env")
	}
}

func splitKV(line string) (string, string, bool) {
	idx := strings.IndexByte(line, '=')
	if idx <= 0 {
		return "", "", false
	}
	key := strings.TrimSpace(line[:idx])
	value := strings.TrimSpace(line[idx+1:])
	value = unquote(value)
	return key, value, true
}

func unquote(value string) string {
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
			inner := value[1 : len(value)-1]
			return strings.ReplaceAll(inner, `\"`, `"`)
		}
	}
	if i := strings.Index(value, " #"); i >= 0 {
		return strings.TrimSpace(value[:i])
	}
	return value
}

func quoteIfNeeded(value string) string {
	if value == "" {
		return ""
	}
	if strings.ContainsAny(value, " \t#\"'") {
		escaped := strings.ReplaceAll(value, `"`, `\"`)
		return `"` + escaped + `"`
	}
	return value
}

func validKey(key string) bool {
	if key == "" {
		return false
	}
	for i, r := range key {
		if i == 0 {
			if r != '_' && !unicode.IsLetter(r) {
				return false
			}
			continue
		}
		if r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}
