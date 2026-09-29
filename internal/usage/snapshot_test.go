package usage

import (
	"testing"
	"time"

	"deploy_platform/internal/store"
)

func TestSnapshotSumsPerProject(t *testing.T) {
	s := New(nil, nil)
	s.sampled = time.Unix(1_700_000_000, 0)
	s.live["n1"] = live{ram: 100, disk: 200, cpu: 1.5}
	s.live["n2"] = live{ram: 50, disk: 25, cpu: 0.5}
	s.history["n1"] = []Point{{T: 1, RAM: 100, Disk: 200}}

	snap := s.Snapshot(
		[]store.Project{{ID: "p1", Name: "App", Environment: "production", FamilyID: "p1"}},
		[]store.Node{
			{ID: "n1", ProjectID: "p1", Kind: "service", Title: "Web", MemoryMB: 512, DiskMB: 1024},
			{ID: "n2", ProjectID: "p1", Kind: "redis", Title: "Cache", MemoryMB: 128, DiskMB: 512},
		},
		4096,
		20480,
	)
	if len(snap.Projects) != 1 {
		t.Fatalf("projects %d", len(snap.Projects))
	}
	if snap.Totals.RAMUsedBytes != 150 || snap.Totals.DiskUsedBytes != 225 {
		t.Fatalf("totals %+v", snap.Totals)
	}
	if snap.Projects[0].Nodes[0].RAMUsedBytes != 100 {
		t.Fatalf("node ram")
	}
	if snap.IntervalSeconds != 12 {
		t.Fatalf("interval")
	}
}
