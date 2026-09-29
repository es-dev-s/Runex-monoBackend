package cache

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMemoryRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if store.Backend() != "memory" {
		t.Fatalf("backend %s", store.Backend())
	}
	store.SetJSON(ctx, "aivex:v1:test", map[string]string{"ok": "yes"}, time.Minute)
	var out map[string]string
	if !store.GetJSON(ctx, "aivex:v1:test", &out) || out["ok"] != "yes" {
		t.Fatalf("missing value: %+v", out)
	}
	store.Delete(ctx, "aivex:v1:test")
	if store.GetJSON(ctx, "aivex:v1:test", &out) {
		t.Fatal("expected miss after delete")
	}
}

func TestBranchesKeyNormalizesRepo(t *testing.T) {
	if BranchesKey(12, "Acme/App") != "aivex:v1:gh:branches:12:acme/app" {
		t.Fatal(BranchesKey(12, "Acme/App"))
	}
}

func TestClaimIsExclusiveThenExpires(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	key := DeliveryKey("d-1")
	if !store.Claim(ctx, key, 50*time.Millisecond) {
		t.Fatal("first claim must win")
	}
	if store.Claim(ctx, key, 50*time.Millisecond) {
		t.Fatal("a redelivery of the same event must be rejected")
	}
	time.Sleep(70 * time.Millisecond)
	if !store.Claim(ctx, key, time.Minute) {
		t.Fatal("claim must be reclaimable after the TTL")
	}
}

// Only one of many simultaneous redeliveries may proceed, or a single push
// starts several competing builds of the same service.
func TestClaimIsRaceFree(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	const racers = 64
	var wins int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if store.Claim(ctx, DeliveryKey("same-delivery"), time.Minute) {
				atomic.AddInt64(&wins, 1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if wins != 1 {
		t.Fatalf("expected exactly one winner, got %d", wins)
	}
}

func TestDeliveryKeyNormalizes(t *testing.T) {
	if DeliveryKey("  AB-12  ") != DeliveryKey("ab-12") {
		t.Fatal("delivery ids must compare case- and space-insensitively")
	}
}

func TestClaimOnNilStoreAllows(t *testing.T) {
	var store *Store
	if !store.Claim(context.Background(), DeliveryKey("x"), time.Minute) {
		t.Fatal("a missing cache must never block webhook processing")
	}
}
