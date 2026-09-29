package pack

import (
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Plan struct {
	Framework      string
	Runtime        string
	RuntimeVersion string
	Image          string
	ContainerPort  int
	Dockerfile     string
}

type DetectedApp struct {
	Path      string `json:"path"`
	Language  string `json:"language"`
	Framework *Tech  `json:"framework"`
	Runtime   *Tech  `json:"runtime"`
	Build     struct {
		Command   string `json:"command"`
		OutputDir string `json:"outputDir"`
	} `json:"build"`
	Start struct {
		Command string `json:"command"`
	} `json:"start"`
	Port      int    `json:"port"`
	Databases []Tech `json:"databases"`
}

type Tech struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// hints carries what the detection engine actually observed in the repository.
// Every plan builder prefers these over its own built-in guess, so a repo whose
// layout differs from the common case still gets a working Dockerfile.
type hints struct {
	Start   string
	Build   string
	Port    int
	Version string
}

func hintsFrom(app DetectedApp) hints {
	h := hints{
		Start: strings.TrimSpace(app.Start.Command),
		Build: strings.TrimSpace(app.Build.Command),
		Port:  app.Port,
	}
	if app.Runtime != nil {
		h.Version = app.Runtime.Version
	}
	return h
}

func (h hints) portOr(fallback int) int {
	if h.Port > 0 {
		return h.Port
	}
	return fallback
}

func FromDetection(dir string, apps []DetectedApp) Plan {
	if plan, ok := fromDockerfile(dir); ok {
		return plan
	}
	if has(dir, "server/package.json") && has(dir, "client/package.json") {
		return nodeMonorepoPlan(dir)
	}
	if len(apps) > 0 {
		picked := pickApp(apps)
		if !isDesktopApp(picked) {
			return fromApp(dir, picked)
		}
	}
	return Infer(dir)
}

func WriteDockerfile(dir string, plan Plan) error {
	ignore := []byte(".git\nnode_modules\n.DS_Store\n")
	_ = os.WriteFile(filepath.Join(dir, ".dockerignore"), ignore, 0o644)
	if plan.Dockerfile == "" {
		return nil
	}
	return os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(plan.Dockerfile), 0o644)
}

func Infer(dir string) Plan {
	if plan, ok := fromDockerfile(dir); ok {
		return plan
	}
	if has(dir, "server/package.json") && has(dir, "client/package.json") {
		return nodeMonorepoPlan(dir)
	}
	if has(dir, "package.json") {
		return inferPackageJSON(dir)
	}
	if has(dir, "index.html") || has(dir, "index.htm") || findHTML(dir) != "" {
		return staticPlan()
	}
	if has(dir, "Cargo.toml") {
		return rustPlan(dir, hints{})
	}
	if has(dir, "requirements.txt") || has(dir, "pyproject.toml") {
		return pythonPlan(dir, hints{})
	}
	if has(dir, "go.mod") {
		return goPlan(dir, hints{})
	}
	return staticPlan()
}

func inferPackageJSON(dir string) Plan {
	pkg := readFile(filepath.Join(dir, "package.json"))
	if strings.Contains(pkg, `"next"`) {
		return nextPlan(dir, hints{})
	}
	if strings.Contains(pkg, `"vite"`) {
		return vitePlan(dir, hints{})
	}
	return nodePlan(dir, hints{})
}

func fromApp(dir string, app DetectedApp) Plan {
	name := ""
	if app.Framework != nil {
		name = strings.ToLower(app.Framework.Name)
	}
	runtime := ""
	if app.Runtime != nil {
		runtime = strings.ToLower(app.Runtime.Name)
	}
	h := hintsFrom(app)
	switch name {
	case "html", "static":
		return staticPlan()
	case "nextjs":
		return nextPlan(dir, h)
	case "vite", "react", "vue", "svelte", "angular", "astro", "gatsby":
		return vitePlan(dir, h)
	case "express", "nodejs", "javascript", "fastify", "hono", "koa", "nestjs":
		return nodePlan(dir, h)
	case "fastapi", "django", "flask", "starlette", "streamlit":
		return pythonPlan(dir, h)
	case "python-worker":
		return pythonWorkerPlan(dir, h)
	case "tauri", "electron":
		return Infer(dir)
	case "net/http", "chi", "gin", "echo", "fiber", "gorilla-mux":
		return goPlan(dir, h)
	case "axum", "actix-web", "rocket", "rust":
		return rustPlan(dir, h)
	}
	switch runtime {
	case "static":
		return staticPlan()
	case "nodejs":
		return nodePlan(dir, h)
	case "python":
		return pythonPlan(dir, h)
	case "go":
		return goPlan(dir, h)
	case "rust":
		return rustPlan(dir, h)
	}
	return Infer(dir)
}

func fromDockerfile(dir string) (Plan, bool) {
	body := ""
	if has(dir, "Dockerfile") {
		body = readFile(filepath.Join(dir, "Dockerfile"))
	} else if has(dir, "dockerfile") {
		body = readFile(filepath.Join(dir, "dockerfile"))
	} else {
		return Plan{}, false
	}
	port := parseExpose(body)
	if port == 0 {
		port = 8080
	}
	return Plan{
		Framework:     "dockerfile",
		Runtime:       "container",
		Image:         firstFromImage(body),
		ContainerPort: port,
	}, true
}

func staticPlan() Plan {
	return Plan{
		Framework:     "html",
		Runtime:       "static",
		Image:         "nginxinc/nginx-unprivileged:1.27-alpine",
		ContainerPort: 8080,
		Dockerfile: `FROM nginxinc/nginx-unprivileged:1.27-alpine
USER root
COPY . /tmp/site
RUN rm -rf /usr/share/nginx/html/* \
 && cp -a /tmp/site/. /usr/share/nginx/html/ \
 && rm -rf /tmp/site \
 && if [ ! -f /usr/share/nginx/html/index.html ] && [ ! -f /usr/share/nginx/html/index.htm ]; then \
      found=$(find /usr/share/nginx/html -type f \( -name 'index.html' -o -name 'index.htm' -o -name 'popup.html' \) | head -n 1); \
      if [ -n "$found" ]; then \
        rel=${found#/usr/share/nginx/html/}; \
        printf '%s\n' "<!DOCTYPE html><html><head><meta charset=\"utf-8\"><meta http-equiv=\"refresh\" content=\"0;url=/${rel}\"></head><body><a href=\"/${rel}\">Open site</a></body></html>" > /usr/share/nginx/html/index.html; \
      else \
        printf '%s' '` + b64inline(staticFallbackIndex) + `' | base64 -d > /usr/share/nginx/html/index.html; \
      fi; \
    fi \
 && printf '%s' '` + b64inline(nginxSPAConf) + `' | base64 -d > /etc/nginx/conf.d/default.conf \
 && chown -R nginx:nginx /usr/share/nginx/html
USER nginx
EXPOSE 8080
`,
	}
}

func nextPlan(dir string, h hints) Plan {
	version := resolveNodeVersion(dir, h.Version)
	image := nodeImage(version, "-bookworm-slim")
	port := h.portOr(3000)
	return Plan{
		Framework:      "nextjs",
		Runtime:        "nodejs",
		RuntimeVersion: major(version, "22"),
		Image:          image,
		ContainerPort:  port,
		Dockerfile: `FROM ` + image + `
WORKDIR /app
ENV NEXT_TELEMETRY_DISABLED=1
COPY . .
RUN if [ -f package.json ]; then \
      npm install --include=dev; \
      if grep -Rqs --include='postcss.config.*' '@tailwindcss/postcss' . \
         || grep -Rqs --include='*.css' '@tailwindcss/postcss' . \
         || grep -Rqs --include='*.css' '@import "tailwindcss"' .; then \
        npm install --include=dev @tailwindcss/postcss tailwindcss postcss; \
      fi; \
    fi
RUN if [ -f package.json ]; then npm run build; fi
ENV NODE_ENV=production PORT=` + strconv.Itoa(port) + ` HOSTNAME=0.0.0.0 HOST=0.0.0.0
EXPOSE ` + strconv.Itoa(port) + `
CMD ["sh","-c","npx next start -H 0.0.0.0 -p ${PORT:-` + strconv.Itoa(port) + `}"]
`,
	}
}

func vitePlan(dir string, h hints) Plan {
	version := resolveNodeVersion(dir, h.Version)
	image := nodeImage(version, "-alpine")
	return Plan{
		Framework:      "vite",
		Runtime:        "static",
		RuntimeVersion: major(version, "22"),
		Image:          image,
		ContainerPort:  8080,
		Dockerfile: `FROM ` + image + ` AS build
WORKDIR /app
COPY . .
RUN npm install && npm run build \
 && mkdir -p /out \
 && if [ -d dist ]; then cp -a dist/. /out/; \
    elif [ -d build ]; then cp -a build/. /out/; \
    elif [ -d out ]; then cp -a out/. /out/; \
    fi
FROM nginxinc/nginx-unprivileged:1.27-alpine
USER root
RUN rm -rf /usr/share/nginx/html/*
COPY --from=build /out/ /usr/share/nginx/html/
RUN printf '%s' '` + b64inline(nginxSPAConf) + `' | base64 -d > /etc/nginx/conf.d/default.conf \
 && if [ ! -f /usr/share/nginx/html/index.html ] && [ ! -f /usr/share/nginx/html/index.htm ]; then \
      printf '%s' '` + b64inline(staticFallbackIndex) + `' | base64 -d > /usr/share/nginx/html/index.html; \
    fi \
 && chown -R nginx:nginx /usr/share/nginx/html
USER nginx
EXPOSE 8080
`,
	}
}

func nodePlan(dir string, h hints) Plan {
	port := h.portOr(3000)
	version := resolveNodeVersion(dir, h.Version)
	image := nodeImage(version, "-bookworm-slim")
	start := startOr(h.Start, "npm start")
	build := ""
	if strings.TrimSpace(h.Build) != "" {
		build = "RUN if [ -f package.json ]; then npm run build --if-present; fi\n"
	}
	return Plan{
		Framework:      "nodejs",
		Runtime:        "nodejs",
		RuntimeVersion: major(version, "22"),
		Image:          image,
		ContainerPort:  port,
		Dockerfile: `FROM ` + image + `
WORKDIR /app
COPY . .
RUN if [ -f package.json ]; then npm install; fi
` + build + `ENV PORT=` + strconv.Itoa(port) + ` HOST=0.0.0.0 NODE_ENV=production
EXPOSE ` + strconv.Itoa(port) + `
CMD ` + shellCMD(start, port) + `
`,
	}
}

func nodeMonorepoPlan(dir string) Plan {
	version := resolveNodeVersion(dir, "")
	image := nodeImage(version, "-bookworm-slim")
	return Plan{
		Framework:      "node-monorepo",
		Runtime:        "nodejs",
		RuntimeVersion: major(version, "22"),
		Image:          image,
		ContainerPort:  3010,
		Dockerfile: `FROM ` + image + `
WORKDIR /app
RUN apt-get update && apt-get install -y --no-install-recommends python3 make g++ \
  && rm -rf /var/lib/apt/lists/*
COPY . .
RUN npm install \
  && if [ -f server/package.json ]; then npm install --prefix server; fi \
  && if [ -f client/package.json ]; then npm install --prefix client; fi
RUN if grep -q '"build"' package.json; then npm run build; fi
ENV NODE_ENV=production PORT=3010 HOST=0.0.0.0
EXPOSE 3010
CMD ["npm","start"]
`,
	}
}

func pythonPlan(dir string, h hints) Plan {
	version := resolvePythonVersion(dir, h.Version)
	image := pythonImage(version, "-slim-bookworm")
	port := h.portOr(8000)
	start := pythonStart(dir, h.Start, port)
	return Plan{
		Framework:      "fastapi",
		Runtime:        "python",
		RuntimeVersion: majorMinor(version, "3.12"),
		Image:          image,
		ContainerPort:  port,
		Dockerfile: `FROM ` + image + `
WORKDIR /app
ENV PYTHONUNBUFFERED=1
COPY . .
RUN if [ -f requirements.txt ]; then pip install --no-cache-dir -r requirements.txt; \
    elif [ -f pyproject.toml ]; then pip install --no-cache-dir .; fi
ENV PORT=` + strconv.Itoa(port) + ` HOST=0.0.0.0
EXPOSE ` + strconv.Itoa(port) + `
CMD ` + shellCMD(start, port) + `
`,
	}
}

func pythonWorkerPlan(dir string, h hints) Plan {
	version := resolvePythonVersion(dir, h.Version)
	image := pythonImage(version, "-alpine")
	port := h.portOr(8080)
	start := pythonModule(startOr(h.Start, "python -u main.py"))
	return Plan{
		Framework:      "python-worker",
		Runtime:        "python",
		RuntimeVersion: majorMinor(version, "3.12"),
		Image:          image,
		ContainerPort:  port,
		Dockerfile: `FROM ` + image + `
WORKDIR /app
ENV PYTHONUNBUFFERED=1
COPY . .
RUN if [ -f requirements.txt ]; then pip install --no-cache-dir -r requirements.txt; fi
ENV PORT=` + strconv.Itoa(port) + ` HOST=0.0.0.0
CMD ` + shellCMD(start, port) + `
`,
	}
}

func rustPlan(dir string, h hints) Plan {
	port := h.portOr(8080)
	start := strings.TrimSpace(h.Start)
	if start == "" || strings.Contains(strings.ToLower(start), "tauri") {
		start = `BIN=$(find target/release -maxdepth 1 -type f -executable ! -name '*.d' | head -n 1); if [ -z "$BIN" ]; then echo 'cargo build produced no binary' >&2; exit 1; fi; exec "$BIN"`
	}
	_ = dir
	return Plan{
		Framework:     "rust",
		Runtime:       "rust",
		Image:         "rust:1-bookworm",
		ContainerPort: port,
		Dockerfile: `FROM rust:1-bookworm
WORKDIR /app
COPY . .
RUN cargo build --release
ENV PORT=` + strconv.Itoa(port) + ` HOST=0.0.0.0 RUST_LOG=info
EXPOSE ` + strconv.Itoa(port) + `
CMD ` + shellCMD(start, port) + `
`,
	}
}

func goPlan(dir string, h hints) Plan {
	pkg := goPackage(dir, h.Build)
	version := resolveGoVersion(dir, h.Version)
	image := goImage(version)
	port := h.portOr(8080)
	return Plan{
		Framework:      "go",
		Runtime:        "go",
		RuntimeVersion: majorMinor(version, "1.24"),
		Image:          image,
		ContainerPort:  port,
		Dockerfile: `FROM ` + image + ` AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=auto
COPY . .
RUN go build -o /out/app ` + pkg + `
FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/app /app
ENV ADDR=:` + strconv.Itoa(port) + ` PORT=` + strconv.Itoa(port) + ` HOST=0.0.0.0
EXPOSE ` + strconv.Itoa(port) + `
CMD ["/app"]
`,
	}
}

func pickApp(apps []DetectedApp) DetectedApp {
	best := apps[0]
	bestScore := -1
	for _, app := range apps {
		if s := scoreApp(app); s > bestScore {
			best = app
			bestScore = s
		}
	}
	return best
}

func isDesktopApp(app DetectedApp) bool {
	name := ""
	if app.Framework != nil {
		name = strings.ToLower(app.Framework.Name)
	}
	path := strings.ToLower(app.Path)
	return name == "tauri" || name == "electron" || strings.Contains(path, "src-tauri")
}

func scoreApp(app DetectedApp) int {
	name := ""
	if app.Framework != nil {
		name = strings.ToLower(app.Framework.Name)
	}
	runtime := ""
	if app.Runtime != nil {
		runtime = strings.ToLower(app.Runtime.Name)
	}
	path := strings.ToLower(app.Path)
	if isDesktopApp(app) {
		return -100
	}
	score := 0
	if path == "." || path == "" {
		score += 10
	}
	switch name {
	case "nextjs", "vite", "react", "vue", "svelte", "html", "static":
		score += 24
	case "express", "fastapi", "net/http", "chi", "gin", "echo", "axum", "actix-web", "rocket":
		score += 8
	}
	if strings.Contains(path, "server") || strings.Contains(path, "backend") || strings.Contains(path, "api") || strings.Contains(path, "engine") || strings.Contains(path, "signaling") {
		score -= 6
	}
	if runtime == "nodejs" || runtime == "static" {
		score += 4
	}
	if runtime == "go" || runtime == "python" || runtime == "rust" {
		score += 2
	}
	if strings.TrimSpace(app.Start.Command) != "" {
		score += 3
	}
	return score
}

func findNamed(root, name string, maxDepth int) string {
	var found string
	root = filepath.Clean(root)
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		if d.IsDir() {
			if skipWalkDir(d.Name()) && path != root {
				return filepath.SkipDir
			}
			depth := 0
			if rel != "." {
				depth = strings.Count(rel, string(os.PathSeparator)) + 1
			}
			if depth > maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(d.Name(), name) {
			return nil
		}
		found = path
		return fs.SkipAll
	})
	return found
}

func findHTML(root string) string {
	for _, name := range []string{"index.html", "index.htm", "popup.html"} {
		if path := findNamed(root, name, 3); path != "" {
			return path
		}
	}
	return ""
}

func skipWalkDir(name string) bool {
	switch name {
	case "node_modules", ".git", "dist", "build", "out", "vendor", "target", ".next", ".turbo":
		return true
	default:
		return strings.HasPrefix(name, ".")
	}
}

func parseExpose(dockerfile string) int {
	port := 0
	for _, line := range strings.Split(dockerfile, "\n") {
		trim := strings.TrimSpace(line)
		if !strings.HasPrefix(strings.ToUpper(trim), "EXPOSE ") {
			continue
		}
		fields := strings.Fields(trim)
		if len(fields) < 2 {
			continue
		}
		raw := strings.Split(fields[1], "/")[0]
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			port = n
		}
	}
	return port
}

const nginxSPAConf = `server {
    listen 8080;
    server_name _;
    root /usr/share/nginx/html;
    index index.html index.htm;
    location / {
        try_files $uri $uri/ /index.html;
    }
}
`

const staticFallbackIndex = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Deployment live</title>
<style>
:root { color-scheme: dark; }
body { margin: 0; min-height: 100vh; display: grid; place-items: center; font-family: ui-sans-serif, system-ui, sans-serif; background: #09090b; color: #fafafa; }
main { text-align: center; padding: 24px; }
h1 { font-size: 1.25rem; font-weight: 600; margin: 0 0 8px; }
p { margin: 0; color: #a1a1aa; font-size: 14px; }
code { color: #93c5fd; }
</style>
</head>
<body>
<main>
<h1>Deployment is live</h1>
<p><code id="host"></code></p>
</main>
<script>document.getElementById("host").textContent = location.origin;</script>
</body>
</html>
`

func b64inline(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func has(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func readFile(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}

func startOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func jsonArray(cmd string) string {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		parts = []string{"sleep", "infinity"}
	}
	raw, _ := json.Marshal(parts)
	return string(raw)
}

// shellCMD renders a start command as a Dockerfile CMD executed through sh so
// ${PORT} expands at runtime. A literal port inside the command is rewritten to
// ${PORT:-n}, which keeps the process listening on whatever port the platform
// publishes even if the detected command pinned a different one.
func shellCMD(cmd string, port int) string {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return ""
	}
	if port > 0 {
		cmd = templatePort(cmd, strconv.Itoa(port))
	}
	raw, _ := json.Marshal([]string{"sh", "-c", cmd})
	return string(raw)
}

// templatePort replaces standalone occurrences of literal with ${PORT:-literal}.
// Digit runs that are part of a longer identifier (python3, main:app2, 1.2.3)
// are left alone so only real port arguments are rewritten.
func templatePort(cmd, literal string) string {
	placeholder := "${PORT:-" + literal + "}"
	var out strings.Builder
	for i := 0; i < len(cmd); {
		if !isDigit(cmd[i]) {
			out.WriteByte(cmd[i])
			i++
			continue
		}
		j := i
		for j < len(cmd) && isDigit(cmd[j]) {
			j++
		}
		run := cmd[i:j]
		boundedLeft := i == 0 || !isIdentByte(cmd[i-1])
		boundedRight := j == len(cmd) || !isIdentByte(cmd[j])
		if run == literal && boundedLeft && boundedRight {
			out.WriteString(placeholder)
		} else {
			out.WriteString(run)
		}
		i = j
	}
	return out.String()
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// isIdentByte reports whether b would make an adjacent digit run part of a
// larger token rather than a standalone number. '.' is included so version
// strings such as 3.12 are never treated as ports.
func isIdentByte(b byte) bool {
	switch {
	case b >= '0' && b <= '9':
		return true
	case b >= 'a' && b <= 'z':
		return true
	case b >= 'A' && b <= 'Z':
		return true
	case b == '_' || b == '-' || b == '.':
		return true
	default:
		return false
	}
}

// pythonModule normalises a bare console-script invocation to `python -m x`,
// which works regardless of whether pip placed the script on PATH.
func pythonModule(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	for _, tool := range []string{"uvicorn", "gunicorn", "hypercorn", "streamlit", "flask", "celery"} {
		if cmd == tool || strings.HasPrefix(cmd, tool+" ") {
			return "python -m " + cmd
		}
	}
	return cmd
}

// pythonStart turns a detected Python start command into one that binds to all
// interfaces on the published port. Each framework spells that differently, so
// the rewrite is per-family rather than a blanket flag append.
func pythonStart(dir, detected string, port int) string {
	start := pythonModule(detected)
	if start == "" {
		return "python -m uvicorn " + uvicornTarget(dir) + " --host 0.0.0.0 --port " + strconv.Itoa(port)
	}
	switch {
	case strings.Contains(start, "manage.py runserver"):
		if !strings.Contains(start, "0.0.0.0") {
			start = strings.Replace(start, "runserver", "runserver 0.0.0.0:"+strconv.Itoa(port), 1)
		}
	case strings.Contains(start, "streamlit"):
		if !strings.Contains(start, "--server.address") {
			start += " --server.address 0.0.0.0 --server.port " + strconv.Itoa(port)
		}
	case strings.Contains(start, "uvicorn"), strings.Contains(start, "hypercorn"):
		if !strings.Contains(start, "--host") {
			start += " --host 0.0.0.0"
		}
		if !strings.Contains(start, "--port") {
			start += " --port " + strconv.Itoa(port)
		}
	case strings.Contains(start, "gunicorn"):
		if !strings.Contains(start, "--bind") && !strings.Contains(start, "-b ") {
			start += " --bind 0.0.0.0:" + strconv.Itoa(port)
		}
	case strings.Contains(start, "flask run"):
		if !strings.Contains(start, "--host") {
			start += " --host 0.0.0.0"
		}
		if !strings.Contains(start, "--port") {
			start += " --port " + strconv.Itoa(port)
		}
	}
	return start
}

// uvicornTarget picks the ASGI module path by looking at what the repository
// actually contains, instead of assuming a fixed layout.
func uvicornTarget(dir string) string {
	candidates := []struct{ file, target string }{
		{"main.py", "main:app"},
		{"app.py", "app:app"},
		{"asgi.py", "asgi:app"},
		{"app/main.py", "app.main:app"},
		{"src/main.py", "src.main:app"},
		{"api/main.py", "api.main:app"},
		{"backend/main.py", "backend.main:app"},
	}
	for _, c := range candidates {
		if has(dir, c.file) {
			return c.target
		}
	}
	return "main:app"
}

// goPackage extracts the package path a detected `go build` command targets so
// multi-module and cmd/-style layouts build the right binary.
func goPackage(dir, build string) string {
	for _, field := range strings.Fields(build) {
		if strings.HasPrefix(field, "./") {
			return field
		}
	}
	if has(dir, "cmd/server") {
		return "./cmd/server"
	}
	if main := findNamed(dir, "main.go", 3); main != "" {
		if rel, err := filepath.Rel(dir, filepath.Dir(main)); err == nil && rel != "." {
			return "./" + filepath.ToSlash(rel)
		}
	}
	return "."
}
