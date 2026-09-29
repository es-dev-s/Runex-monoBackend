package logs

import (
	"sync"
	"testing"
	"time"
)

func TestHubDropsWhenSlow(t *testing.T) {
	hub := NewHub()
	ch := hub.Subscribe("p")
	defer hub.Unsubscribe("p", ch)

	for i := 0; i < 3000; i++ {
		hub.Publish("p", Line{Text: "x", Kind: "log"})
	}

	got := 0
drain:
	for {
		select {
		case <-ch:
			got++
		default:
			break drain
		}
	}
	if got == 0 || got > 2048 {
		t.Fatalf("buffered=%d", got)
	}
}

func TestHubConcurrentPublish(t *testing.T) {
	hub := NewHub()
	ch := hub.Subscribe("p")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hub.Publish("p", Line{Kind: "status", Status: "building"})
		}()
	}
	wg.Wait()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("expected at least one event")
	}
	hub.Unsubscribe("p", ch)
}

func TestHubDeliversStatusWhenLogBufferIsFull(t *testing.T) {
	hub := NewHub()
	ch := hub.Subscribe("p")
	defer hub.Unsubscribe("p", ch)
	got := make(chan Line, 8)
	go func() {
		for line := range ch {
			if line.Kind == "status" {
				got <- line
				return
			}
		}
	}()
	for i := 0; i < 2048; i++ {
		hub.Publish("p", Line{Text: "x", Kind: "log"})
	}
	hub.Publish("p", Line{Kind: "status", Status: "running", URL: "http://svc-a.localhost:8080"})
	select {
	case line := <-got:
		if line.Status != "running" {
			t.Fatalf("status=%s", line.Status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("status event must not be dropped behind build logs")
	}
}
