package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"netcfg/internal/domain"
	"netcfg/internal/rpc"
)

const (
	hubBuffer        = 32
	reconnectBackoff = 2 * time.Second
	heartbeat        = 20 * time.Second
)

// Hub keeps one subscription to the agent and fans events out to browsers, so
// N open tabs cost exactly one RPC connection.
type Hub struct {
	client *rpc.Client
	log    *slog.Logger

	mu   sync.Mutex
	subs map[chan domain.Event]struct{}
}

// NewHub creates the fan-out.
func NewHub(client *rpc.Client, log *slog.Logger) *Hub {
	return &Hub{client: client, log: log, subs: map[chan domain.Event]struct{}{}}
}

// Run maintains the agent subscription until ctx is cancelled.
func (h *Hub) Run(ctx context.Context) {
	for ctx.Err() == nil {
		events, err := h.client.Subscribe(ctx)
		if err != nil {
			h.log.Warn("cannot subscribe to netcfgd events", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(reconnectBackoff):
			}
			continue
		}

		for evt := range events {
			h.broadcast(evt)
		}
		if ctx.Err() == nil {
			h.log.Warn("lost the netcfgd event channel, reconnecting")
			time.Sleep(reconnectBackoff)
		}
	}
}

func (h *Hub) broadcast(evt domain.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- evt:
		default:
		}
	}
}

func (h *Hub) subscribe() (chan domain.Event, func()) {
	ch := make(chan domain.Event, hubBuffer)

	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

// handleEvents streams agent events to one browser over Server-Sent Events.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeStatusProblem(w, r, http.StatusInternalServerError, "this connection does not support streaming")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	events, cancel := s.hub.subscribe()
	defer cancel()

	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case evt := <-events:
			payload, err := json.Marshal(evt)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", evt.Type, payload); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
