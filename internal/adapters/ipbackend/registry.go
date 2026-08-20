// Package ipbackend selects, at runtime, which subsystem owns IP configuration
// for a given link. Writing through the wrong one leaves two managers fighting
// over the same interface, which is the classic cause of flapping links.
package ipbackend

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"netcfg/internal/domain"
	"netcfg/internal/ports"
)

const cacheTTL = 20 * time.Second

// Registry probes each backend in priority order and routes per link.
type Registry struct {
	backends []ports.IPBackend
	log      *slog.Logger

	mu       sync.Mutex
	owners   map[string]ports.IPBackend
	active   domain.BackendKind
	detected time.Time
}

// NewRegistry takes backends in descending priority. NetworkManager must come
// first: when it is running it claims devices exclusively.
func NewRegistry(log *slog.Logger, backends ...ports.IPBackend) *Registry {
	return &Registry{
		backends: backends,
		log:      log,
		owners:   map[string]ports.IPBackend{},
		active:   domain.BackendNone,
	}
}

func (r *Registry) Kind() domain.BackendKind {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active
}

// Detect refreshes the link ownership map.
func (r *Registry) Detect(ctx context.Context) ([]string, error) {
	r.mu.Lock()
	fresh := time.Since(r.detected) < cacheTTL && len(r.owners) > 0
	r.mu.Unlock()
	if fresh {
		return r.knownLinks(), nil
	}

	owners := map[string]ports.IPBackend{}
	active := domain.BackendNone
	var problems []string

	for _, backend := range r.backends {
		links, err := backend.Detect(ctx)
		if err != nil {
			problems = append(problems, string(backend.Kind())+": "+err.Error())
			continue
		}
		if active == domain.BackendNone && len(links) > 0 {
			active = backend.Kind()
		}
		for _, link := range links {
			if _, taken := owners[link]; !taken {
				owners[link] = backend
			}
		}
	}

	r.mu.Lock()
	r.owners = owners
	r.active = active
	r.detected = time.Now()
	r.mu.Unlock()

	if len(owners) == 0 {
		return nil, domain.Unavailable("no IP management backend detected (%s)", strings.Join(problems, "; "))
	}
	r.log.Debug("detected IP backend", "active", active, "links", len(owners))
	return r.knownLinks(), nil
}

func (r *Registry) knownLinks() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.owners))
	for link := range r.owners {
		out = append(out, link)
	}
	return out
}

// For resolves the backend owning a link.
func (r *Registry) For(ctx context.Context, link string) (ports.IPBackend, error) {
	if err := domain.ValidateLinkName(link); err != nil {
		return nil, err
	}
	if _, err := r.Detect(ctx); err != nil {
		return nil, err
	}

	r.mu.Lock()
	backend, ok := r.owners[link]
	r.mu.Unlock()
	if !ok {
		return nil, domain.NotFound("no backend currently manages %s", link)
	}
	return backend, nil
}

// Current returns the running configuration of a link.
func (r *Registry) Current(ctx context.Context, link string) (domain.IPPlan, error) {
	backend, err := r.For(ctx, link)
	if err != nil {
		return domain.IPPlan{Link: link, Mode: domain.ModeDHCP, Mode6: domain.ModeAuto}, err
	}
	return backend.Current(ctx, link)
}

// Apply writes through the owning backend only.
func (r *Registry) Apply(ctx context.Context, plan domain.IPPlan) error {
	backend, err := r.For(ctx, plan.Link)
	if err != nil {
		return err
	}
	return backend.Apply(ctx, plan)
}

// KindFor reports which backend owns a link, for display purposes.
func (r *Registry) KindFor(ctx context.Context, link string) domain.BackendKind {
	backend, err := r.For(ctx, link)
	if err != nil {
		return domain.BackendNone
	}
	return backend.Kind()
}
