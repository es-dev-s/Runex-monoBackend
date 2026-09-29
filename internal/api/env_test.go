package api

import (
	"testing"

	"deploy_platform/internal/store"
)

func TestValidEnvKey(t *testing.T) {
	ok := []string{"PORT", "_PRIVATE", "A1", "foo_bar"}
	bad := []string{"", "1PORT", "FOO-BAR", "FOO.BAR", "FOO BAR"}
	for _, key := range ok {
		if !validEnvKey(key) {
			t.Fatalf("expected valid %q", key)
		}
	}
	for _, key := range bad {
		if validEnvKey(key) {
			t.Fatalf("expected invalid %q", key)
		}
	}
}

func TestServiceOccupiedTreatsLiveAndBuildingAsTaken(t *testing.T) {
	idle := store.Node{Kind: "service", Status: "idle"}
	if serviceOccupied(idle) {
		t.Fatal("blank idle service should be reusable for the first deploy")
	}
	src := "github"
	if !serviceOccupied(store.Node{Kind: "service", Status: "idle", SourceType: &src}) {
		t.Fatal("a sourced service must not be reused for another codebase")
	}
	if !serviceOccupied(store.Node{Kind: "service", Status: "ready"}) {
		t.Fatal("a staged service must not be reused")
	}
	if !serviceOccupied(store.Node{Kind: "service", Status: "running"}) {
		t.Fatal("a running service must not be reused")
	}
}

func TestPickIdleServiceReusesEmptyAmongMany(t *testing.T) {
	src := "github"
	occupied := store.Node{ID: "svc_a", Kind: "service", Status: "running", SourceType: &src}
	idle := store.Node{ID: "svc_b", Kind: "service", Status: "idle"}
	got, ok := pickIdleService([]store.Node{occupied, idle}, "API")
	if !ok || got.ID != "svc_b" || got.Title != "API" {
		t.Fatalf("expected idle slot, got %+v ok=%v", got, ok)
	}
	if _, ok := pickIdleService([]store.Node{occupied}, "API"); ok {
		t.Fatal("occupied services must not be reused")
	}
}

func TestRemainingNodesKeepsSidecarsWhenServiceIsRemoved(t *testing.T) {
	running := store.Node{ID: "svc_a", Kind: "service", Status: "running"}
	postgres := store.Node{ID: "pg", Kind: "postgresql"}
	if remainingNodes([]store.Node{running, postgres}, running.ID) != 1 {
		t.Fatal("removing a service must leave postgres on the canvas")
	}
	if remainingNodes([]store.Node{running}, running.ID) != 0 {
		t.Fatal("removing the last node should empty the project")
	}
	if remainingNodes([]store.Node{running, postgres}, postgres.ID) != 1 {
		t.Fatal("removing postgres must leave sibling services running")
	}
}

func TestUniqueProjectIDsDropsBlanksAndCaps(t *testing.T) {
	got := uniqueProjectIDs([]string{" a ", "", "a", "b", "c"}, 2)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("unique ids: %v", got)
	}
}
