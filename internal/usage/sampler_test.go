package usage

import (
	"context"
	"testing"

	"deploy_platform/internal/alloc"
	"deploy_platform/internal/store"
)

func TestEnforceLimitsNeedsTwoRAMStrikes(t *testing.T) {
	s := New(nil, nil)
	var fired []string
	s.SetOverLimit(func(_ context.Context, node store.Node, reason string) {
		fired = append(fired, node.ID+":"+reason)
	})
	nodes := []store.Node{{ID: "n1", Status: "running", Kind: "service", MemoryMB: 512, DiskMB: 1024}}
	s.live["n1"] = live{ram: alloc.BytesFromMB(512), disk: 10}

	s.enforceLimits(context.Background(), nodes)
	if len(fired) != 0 {
		t.Fatalf("first ram strike should wait, got %v", fired)
	}
	s.enforceLimits(context.Background(), nodes)
	if len(fired) != 1 {
		t.Fatalf("second ram strike should stop, got %v", fired)
	}
}

func TestEnforceLimitsStopsDiskImmediately(t *testing.T) {
	s := New(nil, nil)
	var fired int
	s.SetOverLimit(func(context.Context, store.Node, string) { fired++ })
	nodes := []store.Node{{ID: "n1", Status: "running", Kind: "postgresql", MemoryMB: 512, DiskMB: 2048}}
	s.live["n1"] = live{ram: 1, disk: alloc.BytesFromMB(2048) + 1}

	s.enforceLimits(context.Background(), nodes)
	if fired != 1 {
		t.Fatalf("disk over quota should stop on the first sample, got %d", fired)
	}
}

func TestEnforceLimitsIgnoresStopped(t *testing.T) {
	s := New(nil, nil)
	s.SetOverLimit(func(context.Context, store.Node, string) { t.Fatal("stopped node") })
	nodes := []store.Node{{ID: "n1", Status: "stopped", Kind: "redis", MemoryMB: 128, DiskMB: 512}}
	s.live["n1"] = live{ram: alloc.BytesFromMB(128), disk: alloc.BytesFromMB(512) + 4}
	s.enforceLimits(context.Background(), nodes)
}
