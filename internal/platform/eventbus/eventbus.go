// Package eventbus is an in-process fan-out of domain events to subscribers.
package eventbus

import (
	"sync"

	"netcfg/internal/domain"
)

const bufferSize = 32

// Bus implements ports.Publisher and serves subscribers such as the SSE hub.
type Bus struct {
	mu   sync.RWMutex
	subs map[chan domain.Event]struct{}
}

func New() *Bus { return &Bus{subs: map[chan domain.Event]struct{}{}} }

// Publish delivers an event to every subscriber, dropping it for any subscriber
// that is not keeping up rather than blocking the producer.
func (b *Bus) Publish(evt domain.Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subs {
		select {
		case ch <- evt:
		default:
		}
	}
}

// Subscribe returns a channel and the function that releases it.
func (b *Bus) Subscribe() (<-chan domain.Event, func()) {
	ch := make(chan domain.Event, bufferSize)

	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()

	return ch, func() {
		b.mu.Lock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
		b.mu.Unlock()
	}
}
