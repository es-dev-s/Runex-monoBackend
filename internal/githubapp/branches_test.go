package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"deploy_platform/internal/config"
)

func TestValidGitHubName(t *testing.T) {
	if !ValidGitHubName("acme") || !ValidGitHubName("app.js") || !ValidGitHubName("SD-Bakend") {
		t.Fatal("expected valid names")
	}
	if ValidGitHubName("") || ValidGitHubName("acme/app") || ValidGitHubName("..") || ValidGitHubName("has space") {
		t.Fatal("expected invalid names")
	}
}

func TestSortBranchesPutsDefaultFirst(t *testing.T) {
	got := SortBranches("main", []Branch{
		{Name: "feat/login"},
		{Name: "main"},
		{Name: "develop"},
	})
	if len(got) != 3 || got[0].Name != "main" || !got[0].Default {
		t.Fatalf("%+v", got)
	}
	if got[1].Name != "develop" || got[2].Name != "feat/login" {
		t.Fatalf("order %+v", got)
	}
}

func TestSortBranchesEmpty(t *testing.T) {
	got := SortBranches("main", nil)
	if got == nil || len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestListBranchesPagesMarksDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/app/branches" {
			t.Fatalf("path %s", r.URL.Path)
		}
		if r.URL.Query().Get("page") == "2" {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"name": "main", "protected": true, "commit": map[string]string{"sha": "bbb"}},
			})
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/acme/app/branches?page=2>; rel="next"`, "http://"+r.Host))
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"name": "develop", "commit": map[string]string{"sha": "aaa"}},
		})
	}))
	t.Cleanup(srv.Close)

	client := New(config.Config{})
	client.apiBase = srv.URL
	client.http = srv.Client()

	def, branches, err := client.ListBranches(context.Background(), 0, "acme", "app", "main")
	if err != nil {
		t.Fatal(err)
	}
	if def != "main" || len(branches) != 2 {
		t.Fatalf("def=%s branches=%+v", def, branches)
	}
	if branches[0].Name != "main" || !branches[0].Default || !branches[0].Protected {
		t.Fatalf("%+v", branches[0])
	}
	if branches[1].Name != "develop" || branches[1].Default {
		t.Fatalf("%+v", branches[1])
	}
}

func TestListBranchesEmptyRepo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	t.Cleanup(srv.Close)
	client := New(config.Config{})
	client.apiBase = srv.URL
	client.http = srv.Client()
	def, branches, err := client.ListBranches(context.Background(), 0, "acme", "empty", "main")
	if err != nil {
		t.Fatal(err)
	}
	if def != "main" || branches == nil || len(branches) != 0 {
		t.Fatalf("def=%s branches=%+v", def, branches)
	}
}

func TestListBranchesRejectsInvalidName(t *testing.T) {
	client := New(config.Config{})
	if _, _, err := client.ListBranches(context.Background(), 0, "acme/app", "x", ""); err == nil {
		t.Fatal("expected invalid repository")
	}
}
