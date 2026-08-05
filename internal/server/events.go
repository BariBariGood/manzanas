package server

import (
	"encoding/json"
	"sync"

	"github.com/BariBariGood/manzanas/proto"
)

// eventHub fans server-initiated events out to connected WS clients.
type eventHub struct {
	mu   sync.Mutex
	subs map[chan proto.Envelope]struct{}
}

func newEventHub() *eventHub {
	return &eventHub{subs: make(map[chan proto.Envelope]struct{})}
}

func (h *eventHub) subscribe() chan proto.Envelope {
	ch := make(chan proto.Envelope, 16)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *eventHub) unsubscribe(ch chan proto.Envelope) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

func (h *eventHub) broadcast(event string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	env := proto.Envelope{V: proto.Version, Event: event, Result: raw}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- env:
		default: // drop rather than block a slow client
		}
	}
}
