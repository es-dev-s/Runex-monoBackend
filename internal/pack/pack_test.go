package pack

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPickAppPrefersWebOverGoServer(t *testing.T) {
	picked := pickApp([]DetectedApp{
		{
			Path:      "signaling-server",
			Language:  "go",
			Framework: &Tech{Name: "net/http"},
			Runtime:   &Tech{Name: "go"},
		},
		{
			Path:      "src-tauri",
			Language:  "rust",
			Framework: &Tech{Name: "tauri"},
			Runtime:   &Tech{Name: "rust"},
		},
		{
			Path:      ".",
			Language:  "javascript",
			Framework: &Tech{Name: "vite"},
			Runtime:   &Tech{Name: "nodejs"},
		},
	})
	if picked.Framework == nil || !strings.EqualFold(picked.Framework.Name, "vite") {
		t.Fatalf("wanted vite web app, got %+v", picked)
	}
}

func TestGoPlanUsesGoModVersion(t *testing.T) {
	dir := t.TempDir()
	mod := "module example.com/app\n\ngo 1.24.0\n\ntoolchain go1.24.6\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := goPlan(dir, hints{Version: "1.22"})
	if plan.RuntimeVersion != "1.24" {
		t.Fatalf("version=%q", plan.RuntimeVersion)
	}
	if plan.Image != "golang:1.24-alpine" {
		t.Fatalf("image=%q", plan.Image)
	}
	if !strings.Contains(plan.Dockerfile, "FROM golang:1.24-alpine") {
		t.Fatalf("dockerfile=%s", plan.Dockerfile)
	}
	if !strings.Contains(plan.Dockerfile, "GOTOOLCHAIN=auto") {
		t.Fatal("go image must allow newer toolchains when the tag is a bit behind")
	}
}

func TestGoPlanUsesDirectiveWhenNoToolchain(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n\ngo 1.26.5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := FromDetection(dir, []DetectedApp{{
		Language:  "go",
		Framework: &Tech{Name: "net/http"},
		Runtime:   &Tech{Name: "go", Version: "1.23"},
	}})
	if plan.Image != "golang:1.26-alpine" {
		t.Fatalf("image=%q", plan.Image)
	}
}

func TestNodePlanUsesNvmrc(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".nvmrc"), []byte("v20.18.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := nodePlan(dir, hints{Port: 3000, Start: "npm start", Version: "22"})
	if plan.Image != "node:20-bookworm-slim" {
		t.Fatalf("image=%q", plan.Image)
	}
}

func TestPythonPlanUsesPythonVersionFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".python-version"), []byte("3.11.9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := pythonPlan(dir, hints{Version: "3.12"})
	if plan.Image != "python:3.11-slim-bookworm" {
		t.Fatalf("image=%q", plan.Image)
	}
}

func TestStaticPlanReplacesDefaultNginxIndex(t *testing.T) {
	plan := staticPlan()
	for _, needle := range []string{
		"rm -rf /usr/share/nginx/html/*",
		"popup.html",
		"index.html",
	} {
		if !strings.Contains(plan.Dockerfile, needle) {
			t.Fatalf("static Dockerfile missing %q", needle)
		}
	}
	if !strings.Contains(plan.Dockerfile, b64inline(nginxSPAConf)) {
		t.Fatal("static Dockerfile missing SPA nginx config")
	}
}

func TestFromDetectionRustUsesCargo(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"Cargo.toml": "[package]\nname = \"demo\"\nversion = \"0.1.0\"\nedition = \"2021\"\n",
		"src/main.rs": "fn main() {}\n",
	})
	plan := FromDetection(dir, []DetectedApp{{
		Language: "rust",
		Runtime:  &Tech{Name: "rust"},
	}})
	if plan.Framework != "rust" {
		t.Fatalf("framework=%s", plan.Framework)
	}
	if !strings.Contains(plan.Dockerfile, "cargo build --release") {
		t.Fatalf("dockerfile=%s", plan.Dockerfile)
	}
}

func TestInferCargoTomlIsRust(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"Cargo.toml": "[package]\nname = \"demo\"\nversion = \"0.1.0\"\n",
	})
	plan := Infer(dir)
	if plan.Framework != "rust" {
		t.Fatalf("framework=%s", plan.Framework)
	}
}

func TestNextPlanSetsProductionBuildEnv(t *testing.T) {
	plan := nextPlan("", hints{})
	if !strings.Contains(plan.Dockerfile, "NODE_ENV=production") {
		t.Fatal("next.js image must run in production")
	}
	installAt := strings.Index(plan.Dockerfile, "npm install --include=dev")
	prodAt := strings.Index(plan.Dockerfile, "ENV NODE_ENV=production")
	if installAt < 0 || prodAt < 0 || prodAt < installAt {
		t.Fatal("devDependencies must install before NODE_ENV=production")
	}
	if !strings.Contains(plan.Dockerfile, "@tailwindcss/postcss") {
		t.Fatal("next.js image must install the Tailwind PostCSS plugin when the app needs it")
	}
	if !strings.Contains(plan.Dockerfile, "npx next start") {
		t.Fatal("next.js must start with npx so a hardcoded package.json port cannot steal the published port")
	}
}

// writeTree materialises a fixture repository so plan builders see the same
// filesystem shape a real clone would present.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func app(framework, runtime, start, build string, port int) DetectedApp {
	a := DetectedApp{
		Path:      ".",
		Framework: &Tech{Name: framework},
		Runtime:   &Tech{Name: runtime},
		Port:      port,
	}
	a.Start.Command = start
	a.Build.Command = build
	return a
}

// A FastAPI repo with main.py at the root used to get a hardcoded app.main:app
// CMD and crash-loop. The detected module must win.
func TestFastAPIRootModuleHonoursDetectedStart(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"main.py":          "from fastapi import FastAPI\napp = FastAPI()\n",
		"requirements.txt": "fastapi\nuvicorn\n",
	})
	plan := FromDetection(dir, []DetectedApp{
		app("fastapi", "python", "uvicorn main:app --host 0.0.0.0 --port 8000", "", 8000),
	})
	if strings.Contains(plan.Dockerfile, "app.main:app") {
		t.Fatalf("detected module discarded in favour of a guess:\n%s", plan.Dockerfile)
	}
	if !strings.Contains(plan.Dockerfile, "main:app") {
		t.Fatalf("dockerfile must start the detected module:\n%s", plan.Dockerfile)
	}
	if plan.ContainerPort != 8000 {
		t.Fatalf("port=%d", plan.ContainerPort)
	}
}

// Even with no detector result, the module is probed from disk rather than
// assumed, so a bare FastAPI repo still boots.
func TestFastAPIFallbackProbesModuleFromDisk(t *testing.T) {
	root := writeTree(t, map[string]string{"main.py": "app = 1\n", "requirements.txt": "fastapi\n"})
	if got := pythonPlan(root, hints{}).Dockerfile; !strings.Contains(got, "main:app") || strings.Contains(got, "app.main:app") {
		t.Fatalf("root layout should resolve to main:app:\n%s", got)
	}
	nested := writeTree(t, map[string]string{"app/main.py": "app = 1\n", "requirements.txt": "fastapi\n"})
	if got := pythonPlan(nested, hints{}).Dockerfile; !strings.Contains(got, "app.main:app") {
		t.Fatalf("package layout should resolve to app.main:app:\n%s", got)
	}
}

func TestDjangoBindsAllInterfaces(t *testing.T) {
	dir := writeTree(t, map[string]string{"manage.py": "", "requirements.txt": "django\n"})
	plan := FromDetection(dir, []DetectedApp{
		app("django", "python", "python manage.py runserver", "", 8000),
	})
	if !strings.Contains(plan.Dockerfile, "runserver 0.0.0.0:") {
		t.Fatalf("django must bind 0.0.0.0 or the health check can never pass:\n%s", plan.Dockerfile)
	}
}

func TestGunicornGetsBindFlag(t *testing.T) {
	dir := writeTree(t, map[string]string{"wsgi.py": "", "requirements.txt": "gunicorn\n"})
	plan := pythonPlan(dir, hints{Start: "gunicorn wsgi:app", Port: 8000})
	if !strings.Contains(plan.Dockerfile, "--bind 0.0.0.0:${PORT:-8000}") {
		t.Fatalf("gunicorn needs an explicit bind on the published port:\n%s", plan.Dockerfile)
	}
}

func TestStreamlitUsesServerAddressNotHostFlag(t *testing.T) {
	dir := writeTree(t, map[string]string{"app.py": "", "requirements.txt": "streamlit\n"})
	plan := pythonPlan(dir, hints{Start: "streamlit run app.py", Port: 8501})
	if strings.Contains(plan.Dockerfile, "--host") {
		t.Fatalf("streamlit does not accept --host:\n%s", plan.Dockerfile)
	}
	if !strings.Contains(plan.Dockerfile, "--server.address 0.0.0.0") {
		t.Fatalf("streamlit needs --server.address:\n%s", plan.Dockerfile)
	}
}

// A Go service whose entrypoint is not ./cmd/server must still build the right
// package rather than the module root.
func TestGoPlanBuildsDetectedPackage(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"go.mod":                "module x\n\ngo 1.24\n",
		"cmd/api/main.go":       "package main\nfunc main(){}\n",
		"internal/svc/other.go": "package svc\n",
	})
	plan := goPlan(dir, hints{Build: "go build -o app ./cmd/api", Port: 9000})
	if !strings.Contains(plan.Dockerfile, "go build -o /out/app ./cmd/api") {
		t.Fatalf("wanted ./cmd/api:\n%s", plan.Dockerfile)
	}
	if plan.ContainerPort != 9000 || !strings.Contains(plan.Dockerfile, "EXPOSE 9000") {
		t.Fatalf("detected port ignored:\n%s", plan.Dockerfile)
	}
	if !strings.Contains(plan.Dockerfile, "ADDR=:9000") {
		t.Fatalf("ADDR must match the published port:\n%s", plan.Dockerfile)
	}
}

func TestGoPlanFindsMainWhenNoCmdServer(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"go.mod":          "module x\n\ngo 1.24\n",
		"service/main.go": "package main\nfunc main(){}\n",
	})
	if got := goPlan(dir, hints{}).Dockerfile; !strings.Contains(got, "./service") {
		t.Fatalf("main.go location should drive the build package:\n%s", got)
	}
}

func TestNodePlanHonoursDetectedStartAndPort(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"package.json": `{"name":"x","main":"server.js"}`,
		"server.js":    "",
	})
	plan := FromDetection(dir, []DetectedApp{
		app("express", "nodejs", "node server.js", "", 4000),
	})
	if plan.ContainerPort != 4000 || !strings.Contains(plan.Dockerfile, "EXPOSE 4000") {
		t.Fatalf("detected port ignored:\n%s", plan.Dockerfile)
	}
	if !strings.Contains(plan.Dockerfile, "node server.js") {
		t.Fatalf("detected start ignored:\n%s", plan.Dockerfile)
	}
}

func TestNextPlanHonoursDetectedPort(t *testing.T) {
	plan := nextPlan("", hints{Port: 4321})
	for _, needle := range []string{"EXPOSE 4321", "PORT=4321", "${PORT:-4321}"} {
		if !strings.Contains(plan.Dockerfile, needle) {
			t.Fatalf("missing %q:\n%s", needle, plan.Dockerfile)
		}
	}
}

// Every generated Dockerfile must publish the port the platform will map, or
// the readiness probe fails for reasons unrelated to the user's code.
func TestEveryPlanExposesItsContainerPort(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"go.mod":           "module x\n\ngo 1.24\n",
		"main.go":          "package main\nfunc main(){}\n",
		"main.py":          "app = 1\n",
		"requirements.txt": "fastapi\n",
		"package.json":     `{"name":"x"}`,
	})
	cases := map[string]Plan{
		"fastapi":       pythonPlan(dir, hints{Port: 8000}),
		"python-worker": pythonWorkerPlan(dir, hints{Port: 8080}),
		"go":            goPlan(dir, hints{Port: 8080}),
		"nodejs":        nodePlan(dir, hints{Port: 3000}),
		"nextjs":        nextPlan(dir, hints{Port: 3000}),
		"vite":          vitePlan(dir, hints{}),
		"html":          staticPlan(),
	}
	for name, plan := range cases {
		if plan.ContainerPort <= 0 {
			t.Errorf("%s: no container port", name)
			continue
		}
		// The worker plan is deliberately headless and has no EXPOSE.
		if name == "python-worker" {
			continue
		}
		want := "EXPOSE " + strconv.Itoa(plan.ContainerPort)
		if !strings.Contains(plan.Dockerfile, want) {
			t.Errorf("%s: missing %q\n%s", name, want, plan.Dockerfile)
		}
	}
}

func TestTemplatePortOnlyRewritesStandaloneNumbers(t *testing.T) {
	cases := []struct{ in, want string }{
		{"uvicorn main:app --port 8000", "uvicorn main:app --port ${PORT:-8000}"},
		{"serve --bind 0.0.0.0:8000", "serve --bind 0.0.0.0:${PORT:-8000}"},
		{"python3 -m app --port 8000", "python3 -m app --port ${PORT:-8000}"},
		{"node app8000.js", "node app8000.js"},
		{"run --tag v1.8000", "run --tag v1.8000"},
	}
	for _, c := range cases {
		if got := templatePort(c.in, "8000"); got != c.want {
			t.Errorf("templatePort(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestInferFindsNestedPopupHTML(t *testing.T) {
	dir := t.TempDir()
	popup := filepath.Join(dir, "popup")
	if err := os.Mkdir(popup, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(popup, "popup.html"), []byte("<h1>exam</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := Infer(dir)
	if plan.Framework != "html" {
		t.Fatalf("framework=%s", plan.Framework)
	}
	if !strings.Contains(plan.Dockerfile, "popup.html") {
		t.Fatal("expected nested html to use static nginx plan")
	}
}
