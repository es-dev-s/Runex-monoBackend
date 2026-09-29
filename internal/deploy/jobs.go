package deploy

import (
	"context"
	"sync"
	"time"
)

// JobGate serializes deploys per service node and cancels the previous run
// for that node. Other services in the same project keep building in parallel.
type JobGate struct {
	mu    sync.Mutex
	slots map[string]*jobSlot
}

type jobSlot struct {
	cancel  context.CancelFunc
	done    chan struct{}
	started time.Time
}

func NewJobGate() *JobGate {
	return &JobGate{slots: map[string]*jobSlot{}}
}

func (g *JobGate) Takeover(slotID string) (ctx context.Context, waitPrev func(), finish func()) {
	ctx, cancel := context.WithCancel(context.Background())
	next := &jobSlot{cancel: cancel, done: make(chan struct{}), started: time.Now()}

	g.mu.Lock()
	prev := g.slots[slotID]
	g.slots[slotID] = next
	g.mu.Unlock()

	if prev != nil {
		prev.cancel()
	}

	waitPrev = func() {
		if prev == nil {
			return
		}
		// The previous run is already cancelled. Wait until it releases the
		// slot so two builds for the same service cannot overlap.
		<-prev.done
	}

	finish = func() {
		cancel()
		select {
		case <-next.done:
		default:
			close(next.done)
		}
		g.mu.Lock()
		if g.slots[slotID] == next {
			delete(g.slots, slotID)
		}
		g.mu.Unlock()
	}
	return ctx, waitPrev, finish
}

func (g *JobGate) Cancel(slotID string) {
	g.mu.Lock()
	prev := g.slots[slotID]
	g.mu.Unlock()
	if prev == nil {
		return
	}
	prev.cancel()
	select {
	case <-prev.done:
	case <-time.After(2 * time.Second):
	}
}

func (g *JobGate) Busy(slotID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.slots[slotID] != nil
}

// ActiveIDs returns node IDs with an in-flight deploy job.
func (g *JobGate) ActiveIDs() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	ids := make([]string, 0, len(g.slots))
	for id := range g.slots {
		ids = append(ids, id)
	}
	return ids
}

func (g *JobGate) Age(slotID string) (time.Duration, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	slot := g.slots[slotID]
	if slot == nil {
		return 0, false
	}
	return time.Since(slot.started), true
}
