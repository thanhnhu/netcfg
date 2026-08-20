// Package ports declares the interfaces the application layer depends on.
// Every implementation lives under internal/adapters.
package ports

import (
	"context"
	"time"

	"netcfg/internal/domain"
)

// Supplicant abstracts whichever subsystem drives the radio: the wpa_supplicant
// control socket, NetworkManager, or anything added later.
type Supplicant interface {
	Scan(ctx context.Context, link string) ([]domain.AccessPoint, error)
	Status(ctx context.Context, link string) (domain.WiFiStatus, error)
	Profiles(ctx context.Context, link string) ([]domain.Profile, error)
	Secret(ctx context.Context, link string, id int) (domain.ProfileSecret, error)
	Upsert(ctx context.Context, req domain.WiFiRequest) (int, domain.Message, error)
	Select(ctx context.Context, link string, id int) error
	Remove(ctx context.Context, link string, id int) error
	Disconnect(ctx context.Context, link string) error
	Reconnect(ctx context.Context, link string) error
	Close() error
}

// WiFiBackend is one candidate driver for the radio. Implement this and hand it
// to wifibackend.New to teach the application a new subsystem; nothing above
// the registry needs to know it exists.
type WiFiBackend interface {
	Supplicant
	Kind() domain.WiFiBackendKind
	// Detect reports the wireless links this backend currently drives. The
	// error reaches the operator when no backend claims a link, so it should
	// explain why this one stepped aside.
	Detect(ctx context.Context) ([]string, error)
}

// IPBackend abstracts whichever subsystem owns addressing on this host.
type IPBackend interface {
	Kind() domain.BackendKind
	// Detect reports the links this backend currently manages. An empty slice
	// means the backend is present but idle; an error means it is unavailable.
	Detect(ctx context.Context) ([]string, error)
	Current(ctx context.Context, link string) (domain.IPPlan, error)
	Apply(ctx context.Context, plan domain.IPPlan) error
}

// LinkInspector reads interface state from the kernel.
type LinkInspector interface {
	Links(ctx context.Context) ([]domain.Link, error)
	SetUp(ctx context.Context, link string) error
}

// Hotspot runs the fallback access point used to recover an unreachable device.
type Hotspot interface {
	Available() error
	Start(ctx context.Context, cfg domain.HotspotConfig, reason domain.Message) error
	Stop(ctx context.Context) error
	Status(ctx context.Context) domain.HotspotStatus
}

// Store persists the desired state and the generations used for rollback.
type Store interface {
	Load(ctx context.Context) (domain.DesiredState, error)
	Save(ctx context.Context, state domain.DesiredState) error
	LastKnownGood(ctx context.Context) (domain.DesiredState, error)
	MarkGood(ctx context.Context, state domain.DesiredState) error
}

// Prober answers whether the host still has usable connectivity.
type Prober interface {
	Probe(ctx context.Context, link domain.Link) domain.ProbeResult
	// ProbeVia tests the path through this link alone. The failover monitor needs
	// it because the plain Probe follows the default route, which is the very
	// thing being judged.
	ProbeVia(ctx context.Context, link domain.Link) domain.ProbeResult
}

// RouteControl moves a link up or down the kernel's routing table without
// touching the backend's configuration files, so an automatic demotion is gone
// after a reboot and never fights the operator's saved settings.
type RouteControl interface {
	Available() error
	// DefaultRoute reports the gateway and metric the kernel currently uses for
	// this link, and false when the link installs no default route.
	DefaultRoute(ctx context.Context, link string) (gateway string, metric uint32, ok bool, err error)
	MoveDefault(ctx context.Context, link, gateway string, from, to uint32) error
}

// SystemInfo reads host health counters such as load, memory and temperature.
type SystemInfo interface {
	Stats(ctx context.Context) (domain.SystemStats, error)
}

// SSHControl opens and closes the device's SSH server for a diagnostic session.
// Start reports whether it had to open the port in the firewall, so Stop can put
// back exactly what was changed and nothing else.
type SSHControl interface {
	Status(ctx context.Context) (domain.SSHStatus, error)
	Start(ctx context.Context) (openedFirewall bool, err error)
	Stop(ctx context.Context, closeFirewall bool) error
}

// Clock is injected so commit-confirm timers can be tested without waiting.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is the subset of time.Timer the application needs.
type Timer interface {
	Stop() bool
}

// Publisher broadcasts state changes towards connected browsers.
type Publisher interface {
	Publish(evt domain.Event)
}
