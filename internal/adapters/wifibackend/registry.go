// Package wifibackend selects, at runtime, which subsystem drives the radio on
// a given link. Talking to the wrong one is not merely impolite: a supplicant
// owned by NetworkManager exposes no control socket at all, so the direct path
// cannot work there even in principle.
//
// Adding a backend means writing one type that satisfies ports.WiFiBackend and
// passing it to New in priority order. Nothing else in the application changes:
// the agent depends on the registry, never on a concrete backend.
package wifibackend

import (
	"context"
	"errors"
	"io"
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
	backends []ports.WiFiBackend
	log      *slog.Logger

	mu       sync.Mutex
	owners   map[string]ports.WiFiBackend
	problems []string
	detected time.Time
	nowFunc  func() time.Time
}

// New takes backends in descending priority. NetworkManager belongs first: when
// it runs it claims the radio exclusively.
func New(log *slog.Logger, backends ...ports.WiFiBackend) *Registry {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Registry{
		backends: backends,
		log:      log,
		owners:   map[string]ports.WiFiBackend{},
		nowFunc:  time.Now,
	}
}

// Kind reports the highest priority backend that claimed anything.
func (r *Registry) Kind() domain.WiFiBackendKind {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, backend := range r.backends {
		for _, owner := range r.owners {
			if owner == backend {
				return backend.Kind()
			}
		}
	}
	return domain.WiFiBackendNone
}

// Detect refreshes the link ownership map.
func (r *Registry) Detect(ctx context.Context) ([]string, error) {
	r.mu.Lock()
	fresh := r.nowFunc().Sub(r.detected) < cacheTTL && len(r.owners) > 0
	r.mu.Unlock()
	if fresh {
		return r.knownLinks(), nil
	}

	owners := map[string]ports.WiFiBackend{}
	var problems []string

	for _, backend := range r.backends {
		links, err := backend.Detect(ctx)
		if err != nil {
			problems = append(problems, string(backend.Kind())+": "+err.Error())
			continue
		}
		for _, link := range links {
			if _, taken := owners[link]; !taken {
				owners[link] = backend
			}
		}
	}

	r.mu.Lock()
	r.owners = owners
	r.problems = problems
	r.detected = r.nowFunc()
	r.mu.Unlock()

	if len(owners) == 0 {
		return nil, domain.Unavailable("no Wi-Fi backend detected (%s)", strings.Join(problems, "; "))
	}
	r.log.Debug("detected Wi-Fi backend", "kind", r.Kind(), "links", len(owners))
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

// For resolves the backend driving a link.
func (r *Registry) For(ctx context.Context, link string) (ports.WiFiBackend, error) {
	if err := domain.ValidateLinkName(link); err != nil {
		return nil, err
	}
	if _, err := r.Detect(ctx); err != nil {
		return nil, err
	}

	r.mu.Lock()
	backend, ok := r.owners[link]
	problems := strings.Join(r.problems, "; ")
	r.mu.Unlock()

	switch {
	case ok:
		return backend, nil
	case problems != "":
		return nil, domain.Unavailable("no Wi-Fi backend drives %s (%s)", link, problems)
	default:
		return nil, domain.NotFound("no Wi-Fi backend drives %s", link)
	}
}

func (r *Registry) Scan(ctx context.Context, link string) ([]domain.AccessPoint, error) {
	backend, err := r.For(ctx, link)
	if err != nil {
		return nil, err
	}
	return backend.Scan(ctx, link)
}

func (r *Registry) Status(ctx context.Context, link string) (domain.WiFiStatus, error) {
	backend, err := r.For(ctx, link)
	if err != nil {
		return domain.WiFiStatus{}, err
	}
	return backend.Status(ctx, link)
}

func (r *Registry) Profiles(ctx context.Context, link string) ([]domain.Profile, error) {
	backend, err := r.For(ctx, link)
	if err != nil {
		return nil, err
	}
	return backend.Profiles(ctx, link)
}

func (r *Registry) Secret(ctx context.Context, link string, id int) (domain.ProfileSecret, error) {
	backend, err := r.For(ctx, link)
	if err != nil {
		return domain.ProfileSecret{}, err
	}
	return backend.Secret(ctx, link, id)
}

func (r *Registry) Upsert(ctx context.Context, req domain.WiFiRequest) (int, domain.Message, error) {
	backend, err := r.For(ctx, req.Link)
	if err != nil {
		return 0, domain.Message{}, err
	}
	return backend.Upsert(ctx, req)
}

func (r *Registry) Select(ctx context.Context, link string, id int) error {
	backend, err := r.For(ctx, link)
	if err != nil {
		return err
	}
	return backend.Select(ctx, link, id)
}

func (r *Registry) Remove(ctx context.Context, link string, id int) error {
	backend, err := r.For(ctx, link)
	if err != nil {
		return err
	}
	return backend.Remove(ctx, link, id)
}

func (r *Registry) Disconnect(ctx context.Context, link string) error {
	backend, err := r.For(ctx, link)
	if err != nil {
		return err
	}
	return backend.Disconnect(ctx, link)
}

func (r *Registry) Reconnect(ctx context.Context, link string) error {
	backend, err := r.For(ctx, link)
	if err != nil {
		return err
	}
	return backend.Reconnect(ctx, link)
}

func (r *Registry) Close() error {
	var errs []error
	for _, backend := range r.backends {
		if err := backend.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
