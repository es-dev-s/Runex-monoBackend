package deploy

import "testing"

func TestKeepDeployLog(t *testing.T) {
	keep := []struct{ level, text string }{
		{"info", "Detected FastAPI · Python"},
		{"info", "Cloning es-dev-s/Quality @ main"},
		{"cmd", "docker build"},
		{"info", "Step 4/9 : COPY . ."},
		{"info", "#5 [2/6] RUN npm ci"},
		{"error", "npm ERR! missing script"},
		{"info", "Published http://svc-abc.localhost:8080"},
		{"success", "Ready"},
		{"info", "GitHub push on main @ abc1234 — fetching a fresh copy of acme/api"},
	}
	for _, item := range keep {
		if !KeepDeployLog(item.level, item.text) {
			t.Fatalf("expected keep %q", item.text)
		}
	}
	drop := []string{
		"---> Running in abcdef123456",
		"sha256:0123456789abcdef",
		"Digest: sha256:abc",
		"Removing intermediate container abcdef123456",
		"abcdef123456",
		"#8 transferring context: 12.4MB",
		"   ",
	}
	for _, text := range drop {
		if KeepDeployLog("info", text) {
			t.Fatalf("expected drop %q", text)
		}
	}
}
