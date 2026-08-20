// Package linkinfo reads interface state from the kernel via net, /sys and /proc.
package linkinfo

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"netcfg/internal/adapters/sysexec"
	"netcfg/internal/domain"
)

const sysClassNet = "/sys/class/net"

// Inspector implements ports.LinkInspector.
type Inspector struct{}

func New() *Inspector { return &Inspector{} }

// Links returns every non-loopback interface, wireless ones first.
func (i *Inspector) Links(ctx context.Context) ([]domain.Link, error) {
	all, err := net.Interfaces()
	if err != nil {
		return nil, domain.Internal("cannot list network interfaces: %v", err)
	}

	gateways := defaultGateways()
	dns := resolvConfServers()

	out := make([]domain.Link, 0, len(all))
	for _, ifi := range all {
		if ifi.Flags&net.FlagLoopback != 0 || domain.ValidateLinkName(ifi.Name) != nil {
			continue
		}

		link := domain.Link{
			Name:     ifi.Name,
			MAC:      ifi.HardwareAddr.String(),
			Wireless: isWireless(ifi.Name),
			AdminUp:  ifi.Flags&net.FlagUp != 0,
			OperUp:   operState(ifi.Name) == "up",
			Gateway:  gateways[ifi.Name],
			DNS:      dns,
		}
		if addrs, err := ifi.Addrs(); err == nil {
			for _, a := range addrs {
				link.Addresses = append(link.Addresses, a.String())
			}
		}
		out = append(out, link)
	}

	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Wireless != out[b].Wireless {
			return out[a].Wireless
		}
		return out[a].Name < out[b].Name
	})
	return out, nil
}

// SetUp brings an interface up; wpa_supplicant cannot scan on a down device.
func (i *Inspector) SetUp(ctx context.Context, link string) error {
	if err := domain.ValidateLinkName(link); err != nil {
		return err
	}
	_, err := sysexec.Run(ctx, "ip", "link", "set", "dev", link, "up")
	return err
}

func isWireless(name string) bool {
	for _, probe := range []string{"wireless", "phy80211"} {
		if _, err := os.Stat(filepath.Join(sysClassNet, name, probe)); err == nil {
			return true
		}
	}
	return false
}

func operState(name string) string {
	b, err := os.ReadFile(filepath.Join(sysClassNet, name, "operstate"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func defaultGateways() map[string]string {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return map[string]string{}
	}
	defer f.Close()

	out := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 || fields[1] != "00000000" {
			continue
		}
		raw, err := hex.DecodeString(fields[2])
		if err != nil || len(raw) != 4 {
			continue
		}
		var be [4]byte
		binary.BigEndian.PutUint32(be[:], binary.LittleEndian.Uint32(raw))
		out[fields[0]] = netip.AddrFrom4(be).String()
	}
	return out
}

func resolvConfServers() []string {
	f, err := os.Open("/etc/resolv.conf")
	if err != nil {
		return nil
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && fields[0] == "nameserver" {
			out = append(out, fields[1])
		}
	}
	return out
}
