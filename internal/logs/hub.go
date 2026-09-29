package logs

import (
	"sync"
	"time"
)

type Language struct {
	Name       string  `json:"name"`
	Percentage float64 `json:"percentage"`
	Bytes      int64   `json:"bytes,omitempty"`
	Color      string  `json:"color,omitempty"`
}

type StackItem struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Color string `json:"color,omitempty"`
}

type Line struct {
	TS        time.Time   `json:"ts"`
	Phase     string      `json:"phase,omitempty"`
	Level     string      `json:"level,omitempty"`
	Text      string      `json:"text,omitempty"`
	Status    string      `json:"status,omitempty"`
	URL       string      `json:"url,omitempty"`
	Kind      string      `json:"kind"`
	Framework string      `json:"framework,omitempty"`
	NodeID    string      `json:"nodeId,omitempty"`
	StartedAt *time.Time  `json:"startedAt,omitempty"`
	Languages []Language  `json:"languages,omitempty"`
	Stack     []StackItem `json:"stack,omitempty"`
}

type Hub struct {
	mu   sync.Mutex
	subs map[string]map[chan Line]struct{}
}

func NewHub() *Hub {
	return &Hub{subs: map[string]map[chan Line]struct{}{}}
}

func (h *Hub) Subscribe(projectID string) chan Line {
	ch := make(chan Line, 2048)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[projectID] == nil {
		h.subs[projectID] = map[chan Line]struct{}{}
	}
	h.subs[projectID][ch] = struct{}{}
	return ch
}

func (h *Hub) Unsubscribe(projectID string, ch chan Line) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if set, ok := h.subs[projectID]; ok {
		delete(set, ch)
		if len(set) == 0 {
			delete(h.subs, projectID)
		}
	}
}

func (h *Hub) Publish(projectID string, line Line) {
	if line.Kind == "" {
		line.Kind = "log"
	}
	if line.TS.IsZero() {
		line.TS = time.Now()
	}
	h.mu.Lock()
	chans := make([]chan Line, 0, len(h.subs[projectID]))
	for ch := range h.subs[projectID] {
		chans = append(chans, ch)
	}
	h.mu.Unlock()

	wait := time.Duration(0)
	if line.Kind != "log" {
		wait = 1500 * time.Millisecond
	}
	for _, ch := range chans {
		sendLine(ch, line, wait)
	}
}

func sendLine(ch chan Line, line Line, wait time.Duration) {
	if wait <= 0 {
		select {
		case ch <- line:
		default:
		}
		return
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case ch <- line:
	case <-timer.C:
	}
}
