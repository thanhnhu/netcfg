package wifibackend

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"netcfg/internal/domain"
	"netcfg/internal/ports"
)

type fakeBackend struct {
	name    string
	links   []string
	failure error
	asked   int
	closed  bool
}

func (f *fakeBackend) Kind() domain.WiFiBackendKind { return domain.WiFiBackendKind(f.name) }

func (f *fakeBackend) Detect(context.Context) ([]string, error) {
	f.asked++
	if f.failure != nil {
		return nil, f.failure
	}
	return f.links, nil
}

func (f *fakeBackend) Scan(_ context.Context, link string) ([]domain.AccessPoint, error) {
	return []domain.AccessPoint{{SSID: f.name}}, nil
}

func (f *fakeBackend) Status(context.Context, string) (domain.WiFiStatus, error) {
	return domain.WiFiStatus{State: f.name}, nil
}
func (f *fakeBackend) Profiles(context.Context, string) ([]domain.Profile, error) { return nil, nil }
func (f *fakeBackend) Secret(context.Context, string, int) (domain.ProfileSecret, error) {
	return domain.ProfileSecret{}, nil
}
func (f *fakeBackend) Upsert(context.Context, domain.WiFiRequest) (int, domain.Message, error) {
	return 0, domain.Message{}, nil
}
func (f *fakeBackend) Select(context.Context, string, int) error { return nil }
func (f *fakeBackend) Remove(context.Context, string, int) error { return nil }
func (f *fakeBackend) Disconnect(context.Context, string) error  { return nil }
func (f *fakeBackend) Reconnect(context.Context, string) error   { return nil }
func (f *fakeBackend) Close() error                              { f.closed = true; return nil }

var _ ports.Supplicant = (*Registry)(nil)

func TestTheFirstClaimantWins(t *testing.T) {
	nm := &fakeBackend{name: "nm", links: []string{"wlan0"}}
	wpa := &fakeBackend{name: "wpa", links: []string{"wlan0", "wlan1"}}
	r := New(nil, nm, wpa)

	if got, _ := r.Scan(context.Background(), "wlan0"); got[0].SSID != "nm" {
		t.Fatalf("wlan0 went to %q, want the higher priority backend", got[0].SSID)
	}
	if got, _ := r.Scan(context.Background(), "wlan1"); got[0].SSID != "wpa" {
		t.Fatalf("wlan1 went to %q, want wpa", got[0].SSID)
	}
	if r.Kind() != domain.WiFiBackendKind("nm") {
		t.Fatalf("Kind = %q, want the highest priority claimant", r.Kind())
	}
}

// TestNobodyClaimsAnythingNamesEveryReason is what makes a third or fourth
// backend diagnosable: the operator must see why each one stepped aside.
func TestNobodyClaimsAnythingNamesEveryReason(t *testing.T) {
	nm := &fakeBackend{name: "nm", failure: errors.New("nmcli is not installed")}
	wpa := &fakeBackend{name: "wpa", failure: errors.New("no control sockets in /run/wpa_supplicant")}
	iwd := &fakeBackend{name: "iwd", failure: errors.New("iwd is not running")}
	r := New(nil, nm, wpa, iwd)

	_, err := r.Scan(context.Background(), "wlan0")
	if err == nil {
		t.Fatal("expected an error when no backend claims the link")
	}
	for _, want := range []string{"nm: nmcli is not installed", "wpa: no control sockets", "iwd: iwd is not running"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if r.Kind() != domain.WiFiBackendNone {
		t.Fatalf("Kind = %q, want none", r.Kind())
	}
}

// TestAClaimedHostStillRefusesAnUnknownLink separates "nothing works here" from
// "this particular link is not wireless".
func TestAClaimedHostStillRefusesAnUnknownLink(t *testing.T) {
	r := New(nil, &fakeBackend{name: "wpa", links: []string{"wlan0"}})

	if _, err := r.Scan(context.Background(), "eth0"); err == nil {
		t.Fatal("expected an error for a link no backend drives")
	} else if !strings.Contains(err.Error(), "eth0") {
		t.Fatalf("error %q does not name the link", err)
	}
}

// TestOwnershipIsCached stops every API call from shelling out to nmcli.
func TestOwnershipIsCached(t *testing.T) {
	nm := &fakeBackend{name: "nm", links: []string{"wlan0"}}
	r := New(nil, nm, &fakeBackend{name: "wpa", failure: errors.New("absent")})

	for i := 0; i < 5; i++ {
		r.Scan(context.Background(), "wlan0")
	}
	if nm.asked != 1 {
		t.Fatalf("probed %d times, want 1 within the cache window", nm.asked)
	}

	now := time.Now()
	r.nowFunc = func() time.Time { return now.Add(2 * cacheTTL) }
	r.Scan(context.Background(), "wlan0")
	if nm.asked != 2 {
		t.Fatalf("probed %d times, want a re-probe once the entry expired", nm.asked)
	}
}

// TestUpsertRoutesOnTheRequestLink guards the one method whose link is buried
// in the request rather than passed alongside it.
func TestUpsertRoutesOnTheRequestLink(t *testing.T) {
	nm := &fakeBackend{name: "nm", links: []string{"wlan1"}}
	wpa := &fakeBackend{name: "wpa", failure: errors.New("absent")}
	r := New(nil, nm, wpa)

	if _, _, err := r.Upsert(context.Background(), domain.WiFiRequest{Link: "wlan1", SSID: "x"}); err != nil {
		t.Fatalf("Upsert routed to nobody: %v", err)
	}
	if nm.asked == 0 {
		t.Fatal("Upsert did not consult the backends for the request link")
	}
}

func TestCloseReachesEveryBackend(t *testing.T) {
	nm := &fakeBackend{name: "nm"}
	wpa := &fakeBackend{name: "wpa"}
	r := New(nil, nm, wpa)

	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !nm.closed || !wpa.closed {
		t.Fatal("Close must release every backend, not just the active one")
	}
}
