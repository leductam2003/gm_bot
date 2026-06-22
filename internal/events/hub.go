// Package events is a tiny fan-out hub: producers (logger, task engine) Publish
// pre-marshaled JSON envelopes; the WebSocket handler Subscribes and relays each
// message to one browser. Slow subscribers drop messages rather than block.
package events

import "sync"

// Envelope is the wire shape every WS message uses: {"type":"log"|"task", "data":...}.
type Envelope struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

type sub struct {
	ch chan Envelope
}

type Hub struct {
	mu   sync.RWMutex
	subs map[*sub]struct{}
}

func NewHub() *Hub { return &Hub{subs: map[*sub]struct{}{}} }

// Publish delivers msg to every subscriber. Non-blocking: if a subscriber's
// buffer is full it simply misses this message.
func (h *Hub) Publish(typ string, data any) {
	env := Envelope{Type: typ, Data: data}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for s := range h.subs {
		select {
		case s.ch <- env:
		default:
		}
	}
}

// Subscribe returns a receive channel and an unsubscribe func.
func (h *Hub) Subscribe() (<-chan Envelope, func()) {
	s := &sub{ch: make(chan Envelope, 256)}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s.ch, func() {
		h.mu.Lock()
		delete(h.subs, s)
		h.mu.Unlock()
		close(s.ch)
	}
}
