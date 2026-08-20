// Package nmwifi drives Wi-Fi through NetworkManager. On a host where NM owns
// the radio, wpa_supplicant has no control socket of its own to talk to: NM
// runs it as a private child.
package nmwifi

import (
	"context"
	"hash/fnv"
	"strconv"
	"strings"
	"time"

	"netcfg/internal/adapters/sysexec"
	"netcfg/internal/domain"
	"netcfg/internal/ports"
)

var _ ports.WiFiBackend = (*Adapter)(nil)

type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Kind() domain.WiFiBackendKind { return domain.WiFiBackendNM }

// Detect lists the wireless devices NetworkManager currently manages.
func (a *Adapter) Detect(ctx context.Context) ([]string, error) {
	if !sysexec.Available("nmcli") {
		return nil, domain.Unavailable("nmcli is not installed")
	}
	out, err := sysexec.Run(ctx, "nmcli", "-t", "-f", "DEVICE,TYPE,STATE", "device", "status")
	if err != nil {
		return nil, err
	}

	var links []string
	for _, line := range strings.Split(out, "\n") {
		fields := splitEscaped(strings.TrimSpace(line))
		if len(fields) < 3 || fields[1] != "wifi" || fields[2] == "unmanaged" {
			continue
		}
		links = append(links, fields[0])
	}
	if len(links) == 0 {
		return nil, domain.Unavailable("NetworkManager manages no wireless device")
	}
	return links, nil
}

func (a *Adapter) Scan(ctx context.Context, link string) ([]domain.AccessPoint, error) {
	if err := domain.ValidateLinkName(link); err != nil {
		return nil, err
	}
	out, err := sysexec.Run(ctx, "nmcli", "-t", "-f", "SSID,BSSID,SIGNAL,FREQ,SECURITY",
		"device", "wifi", "list", "ifname", link, "--rescan", "yes")
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var networks []domain.AccessPoint
	for _, line := range strings.Split(out, "\n") {
		fields := splitEscaped(strings.TrimSpace(line))
		if len(fields) < 5 || fields[0] == "" {
			continue
		}
		ssid := fields[0]
		if seen[ssid] {
			continue
		}
		seen[ssid] = true

		signal, _ := strconv.Atoi(fields[2])
		freq := parseFreq(fields[3])
		networks = append(networks, domain.AccessPoint{
			SSID:  ssid,
			BSSID: fields[1],
			Freq:  freq,
			Band:  domain.Band(freq),
			// nmcli reports quality 0-100 where wpa_supplicant reports dBm.
			Signal:   quality(signal),
			Quality:  signal,
			Security: securityFromNM(fields[4]),
			Flags:    fields[4],
		})
	}
	return networks, nil
}

func (a *Adapter) Status(ctx context.Context, link string) (domain.WiFiStatus, error) {
	status := domain.WiFiStatus{State: "DISCONNECTED"}
	if err := domain.ValidateLinkName(link); err != nil {
		return status, err
	}

	out, err := sysexec.Run(ctx, "nmcli", "-t", "-f", "GENERAL.STATE,GENERAL.CONNECTION",
		"device", "show", link)
	if err != nil {
		return status, err
	}
	var connection string
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		switch key {
		case "GENERAL.STATE":
			status.State = strings.ToUpper(stateWord(value))
			status.Associated = strings.Contains(value, "connected") && !strings.Contains(value, "disconnected")
		case "GENERAL.CONNECTION":
			if value != "" && value != "--" {
				connection = value
			}
		}
	}
	if !status.Associated {
		return status, nil
	}

	// The active row of the scan list carries the live signal and frequency.
	list, err := sysexec.Run(ctx, "nmcli", "-t", "-f", "ACTIVE,SSID,BSSID,SIGNAL,FREQ",
		"device", "wifi", "list", "ifname", link)
	if err == nil {
		for _, line := range strings.Split(list, "\n") {
			fields := splitEscaped(strings.TrimSpace(line))
			if len(fields) < 5 || fields[0] != "yes" {
				continue
			}
			status.SSID, status.BSSID = fields[1], fields[2]
			signal, _ := strconv.Atoi(fields[3])
			status.Signal = quality(signal)
			status.Freq = parseFreq(fields[4])
			break
		}
	}
	if status.SSID == "" {
		status.SSID = connection
	}
	if uuid := a.uuidFor(ctx, connection); uuid != "" {
		status.ProfileID = profileID(uuid)
	}
	return status, nil
}

func (a *Adapter) Profiles(ctx context.Context, link string) ([]domain.Profile, error) {
	if err := domain.ValidateLinkName(link); err != nil {
		return nil, err
	}
	saved, err := a.savedConnections(ctx)
	if err != nil {
		return nil, err
	}

	active, _ := a.Status(ctx, link)
	profiles := make([]domain.Profile, 0, len(saved))
	for _, c := range saved {
		profiles = append(profiles, domain.Profile{
			ID:      profileID(c.uuid),
			SSID:    c.ssid,
			Current: active.Associated && c.name == active.SSID,
			Enabled: true,
		})
	}
	return profiles, nil
}

func (a *Adapter) Secret(ctx context.Context, link string, id int) (domain.ProfileSecret, error) {
	connection, err := a.byID(ctx, id)
	if err != nil {
		return domain.ProfileSecret{}, err
	}

	// -s is what makes nmcli print secrets rather than <hidden>.
	out, err := sysexec.Run(ctx, "nmcli", "-s", "-g", "802-11-wireless-security.psk",
		"connection", "show", "uuid", connection.uuid)
	if err != nil {
		return domain.ProfileSecret{}, err
	}
	value := strings.TrimSpace(out)
	if value == "" {
		return domain.ProfileSecret{SSID: connection.ssid}, nil
	}
	// NetworkManager stores what the operator typed, never a derived key.
	return domain.ProfileSecret{SSID: connection.ssid, Value: domain.NewSecret(value)}, nil
}

func (a *Adapter) Upsert(ctx context.Context, req domain.WiFiRequest) (int, domain.Message, error) {
	if err := req.Validate(); err != nil {
		return 0, domain.Message{}, err
	}

	args := []string{"device", "wifi", "connect", req.SSID, "ifname", req.Link}
	if req.Security.NeedsPassphrase() {
		args = append(args, "password", req.Passphrase.Reveal())
	}
	if req.Hidden {
		args = append(args, "hidden", "yes")
	}
	if _, err := sysexec.RunTimeout(ctx, 60*time.Second, "nmcli", args...); err != nil {
		return 0, domain.Message{}, err
	}

	uuid := a.uuidFor(ctx, req.SSID)
	return profileID(uuid), domain.Msg("Joined %s", req.SSID), nil
}

func (a *Adapter) Select(ctx context.Context, link string, id int) error {
	connection, err := a.byID(ctx, id)
	if err != nil {
		return err
	}
	_, err = sysexec.Run(ctx, "nmcli", "connection", "up", "uuid", connection.uuid, "ifname", link)
	return err
}

func (a *Adapter) Remove(ctx context.Context, link string, id int) error {
	connection, err := a.byID(ctx, id)
	if err != nil {
		return err
	}
	_, err = sysexec.Run(ctx, "nmcli", "connection", "delete", "uuid", connection.uuid)
	return err
}

func (a *Adapter) Disconnect(ctx context.Context, link string) error {
	if err := domain.ValidateLinkName(link); err != nil {
		return err
	}
	_, err := sysexec.Run(ctx, "nmcli", "device", "disconnect", link)
	return err
}

func (a *Adapter) Reconnect(ctx context.Context, link string) error {
	if err := domain.ValidateLinkName(link); err != nil {
		return err
	}
	_, err := sysexec.Run(ctx, "nmcli", "device", "connect", link)
	return err
}

func (a *Adapter) Close() error { return nil }

type connection struct {
	name, uuid, ssid string
}

func (a *Adapter) savedConnections(ctx context.Context) ([]connection, error) {
	out, err := sysexec.Run(ctx, "nmcli", "-t", "-f", "NAME,UUID,TYPE", "connection", "show")
	if err != nil {
		return nil, err
	}

	var saved []connection
	for _, line := range strings.Split(out, "\n") {
		fields := splitEscaped(strings.TrimSpace(line))
		if len(fields) < 3 || !strings.Contains(fields[2], "wireless") {
			continue
		}
		saved = append(saved, connection{name: fields[0], uuid: fields[1], ssid: a.ssidOf(ctx, fields[1], fields[0])})
	}
	return saved, nil
}

// ssidOf reads the real SSID, which may differ from the profile name.
func (a *Adapter) ssidOf(ctx context.Context, uuid, fallback string) string {
	out, err := sysexec.Run(ctx, "nmcli", "-g", "802-11-wireless.ssid", "connection", "show", "uuid", uuid)
	if ssid := strings.TrimSpace(out); err == nil && ssid != "" {
		return ssid
	}
	return fallback
}

func (a *Adapter) byID(ctx context.Context, id int) (connection, error) {
	saved, err := a.savedConnections(ctx)
	if err != nil {
		return connection{}, err
	}
	for _, c := range saved {
		if profileID(c.uuid) == id {
			return c, nil
		}
	}
	return connection{}, domain.NotFound("no saved network with id %d", id)
}

func (a *Adapter) uuidFor(ctx context.Context, name string) string {
	if name == "" {
		return ""
	}
	saved, err := a.savedConnections(ctx)
	if err != nil {
		return ""
	}
	for _, c := range saved {
		if c.name == name || c.ssid == name {
			return c.uuid
		}
	}
	return ""
}

// profileID folds a UUID into the int the API speaks, so NetworkManager fits
// the same contract as wpa_supplicant's numeric network ids.
func profileID(uuid string) int {
	if uuid == "" {
		return 0
	}
	sum := fnv.New32a()
	sum.Write([]byte(uuid))
	return int(sum.Sum32() & 0x7fffffff)
}

// splitEscaped splits nmcli's terse output, where a literal colon inside a
// value is backslash escaped.
func splitEscaped(line string) []string {
	var fields []string
	var current strings.Builder

	for i := 0; i < len(line); i++ {
		switch {
		case line[i] == '\\' && i+1 < len(line):
			i++
			current.WriteByte(line[i])
		case line[i] == ':':
			fields = append(fields, current.String())
			current.Reset()
		default:
			current.WriteByte(line[i])
		}
	}
	fields = append(fields, current.String())
	return fields
}

func securityFromNM(flags string) domain.Security {
	upper := strings.ToUpper(flags)
	switch {
	case strings.Contains(upper, "SAE") && strings.Contains(upper, "WPA2"):
		return domain.SecPSKSAE
	case strings.Contains(upper, "SAE"), strings.Contains(upper, "WPA3"):
		return domain.SecSAE
	case strings.Contains(upper, "WPA"):
		return domain.SecPSK
	case strings.Contains(upper, "WEP"):
		return domain.SecWEP
	default:
		return domain.SecOpen
	}
}

// quality converts nmcli's 0-100 bar into the dBm figure the rest of the app
// displays, using the usual linear mapping between -100 and -50.
func quality(percent int) int {
	if percent <= 0 {
		return -100
	}
	if percent >= 100 {
		return -50
	}
	return percent/2 - 100
}

func parseFreq(value string) int {
	digits := strings.Fields(strings.TrimSpace(value))
	if len(digits) == 0 {
		return 0
	}
	freq, _ := strconv.Atoi(digits[0])
	return freq
}

// stateWord turns "100 (connected)" into "connected".
func stateWord(value string) string {
	if open := strings.Index(value, "("); open >= 0 {
		if close := strings.Index(value[open:], ")"); close > 0 {
			return value[open+1 : open+close]
		}
	}
	return strings.TrimSpace(value)
}
