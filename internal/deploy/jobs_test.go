package deploy

import (
	"testing"
	"time"

	"deploy_platform/internal/detect"
	"deploy_platform/internal/pack"
)

func TestJobGateCancelsPrevious(t *testing.T) {
	gate := NewJobGate()
	ctx1, wait1, done1 := gate.Takeover("prj")
	started := make(chan struct{})
	go func() {
		wait1()
		close(started)
		<-ctx1.Done()
		done1()
	}()
	<-started

	ctx2, wait2, done2 := gate.Takeover("prj")
	defer done2()
	wait2()

	select {
	case <-ctx1.Done():
	case <-time.After(time.Second):
		t.Fatal("previous deploy context was not cancelled")
	}
	if ctx2.Err() != nil {
		t.Fatal("new deploy context should still be live")
	}
}

func TestJobGateCancelStopsActiveDeploy(t *testing.T) {
	gate := NewJobGate()
	ctx, wait, done := gate.Takeover("svc")
	started := make(chan struct{})
	go func() {
		wait()
		close(started)
		<-ctx.Done()
		done()
	}()
	<-started
	gate.Cancel("svc")
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("cancel did not stop the active deploy")
	}
}

func TestJobGateIsolatesProjects(t *testing.T) {
	gate := NewJobGate()
	ctxA, waitA, doneA := gate.Takeover("a")
	defer doneA()
	waitA()
	ctxB, waitB, doneB := gate.Takeover("b")
	defer doneB()
	waitB()
	if ctxA.Err() != nil || ctxB.Err() != nil {
		t.Fatal("independent projects should not cancel each other")
	}
}

func TestJobGateBusyAndAge(t *testing.T) {
	gate := NewJobGate()
	if gate.Busy("svc") {
		t.Fatal("empty gate should not be busy")
	}
	_, wait, done := gate.Takeover("svc")
	wait()
	if !gate.Busy("svc") {
		t.Fatal("active deploy should be busy")
	}
	age, ok := gate.Age("svc")
	if !ok || age < 0 {
		t.Fatal("expected age for active deploy")
	}
	done()
	if gate.Busy("svc") {
		t.Fatal("finished deploy should not stay busy")
	}
	if _, ok := gate.Age("svc"); ok {
		t.Fatal("finished deploy should not report age")
	}
}

func TestJobGateFinishIsIdempotent(t *testing.T) {
	gate := NewJobGate()
	ctx, wait, done := gate.Takeover("prj")
	wait()
	done()
	done()
	if ctx.Err() == nil {
		t.Fatal("expected cancelled context after finish")
	}
	_, wait2, done2 := gate.Takeover("prj")
	wait2()
	done2()
}

func TestJobGateTakeoverDoesNotBlockOnPrevious(t *testing.T) {
	gate := NewJobGate()
	_, wait1, done1 := gate.Takeover("svc")
	blocked := make(chan struct{})
	go func() {
		wait1()
		<-blocked
		done1()
	}()
	started := time.Now()
	_, wait2, done2 := gate.Takeover("svc")
	if time.Since(started) > 200*time.Millisecond {
		t.Fatal("takeover should return without waiting for the previous job")
	}
	close(blocked)
	wait2()
	done2()
}

func TestLanguagesFromFiltersAndMerges(t *testing.T) {
	got := languagesFrom(detect.Result{
		Languages: []detect.Language{
			{Name: "TSX", Percentage: 40, Bytes: 40},
			{Name: "Ignore List", Percentage: 10, Bytes: 10},
			{Name: "TypeScript", Percentage: 50, Bytes: 50},
			{Name: "Dockerfile", Percentage: 5, Bytes: 5},
		},
	})
	if len(got) != 1 || got[0].Name != "TypeScript" {
		t.Fatalf("merged languages: %+v", got)
	}
	if got[0].Percentage < 89 {
		t.Fatalf("expected merged percentage, got %v", got[0].Percentage)
	}
}

func TestStackFromDropsInfraAndNormalizes(t *testing.T) {
	got := stackFrom(detect.Result{
		Languages: []detect.Language{{Name: "Go", Percentage: 100, Bytes: 100}},
		Infra:     []pack.Tech{{Name: "Docker"}},
		Apps: []pack.DetectedApp{{
			Language:  "go",
			Framework: &pack.Tech{Name: "net/http"},
			Runtime:   &pack.Tech{Name: "go"},
		}},
	}, pack.Plan{Framework: "net/http", Runtime: "go"})
	for _, item := range got {
		if item.Kind == "infra" {
			t.Fatalf("infra leaked: %+v", got)
		}
	}
	if len(got) == 0 {
		t.Fatal("expected stack items")
	}
}

func TestLanguagesFromKeepsAppLanguages(t *testing.T) {
	got := languagesFrom(detect.Result{
		Languages: []detect.Language{
			{Name: "Rust", Percentage: 45, Bytes: 4500},
			{Name: "JavaScript", Percentage: 43, Bytes: 4300},
			{Name: "Go", Percentage: 4.3, Bytes: 430},
			{Name: "HTML", Percentage: 1, Bytes: 100},
			{Name: "SVG", Percentage: 6, Bytes: 600},
		},
	})
	names := map[string]struct{}{}
	for _, item := range got {
		names[item.Name] = struct{}{}
	}
	for _, want := range []string{"Rust", "JavaScript", "Go", "HTML"} {
		if _, ok := names[want]; !ok {
			t.Fatalf("missing %s in %+v", want, got)
		}
	}
	if _, ok := names["SVG"]; ok {
		t.Fatal("svg should be filtered")
	}
	if got[0].Name != "Rust" {
		t.Fatalf("expected rust first, got %+v", got)
	}
}

func TestJobGateActiveIDs(t *testing.T) {
	gate := NewJobGate()
	if len(gate.ActiveIDs()) != 0 {
		t.Fatal("empty gate should have no active ids")
	}
	_, waitA, doneA := gate.Takeover("a")
	waitA()
	_, waitB, doneB := gate.Takeover("b")
	waitB()
	ids := gate.ActiveIDs()
	if len(ids) != 2 {
		t.Fatalf("want 2 active ids, got %v", ids)
	}
	doneA()
	ids = gate.ActiveIDs()
	if len(ids) != 1 || ids[0] != "b" {
		t.Fatalf("want only b active, got %v", ids)
	}
	doneB()
	if len(gate.ActiveIDs()) != 0 {
		t.Fatal("finished jobs should clear active ids")
	}
}

