// Package sshctl turns the device's SSH server on and off through systemd. It
// never touches boot enablement: this exists to open a diagnostic window, not
// to change what the device does after a restart.
package sshctl

import (
	"context"
	"strconv"
	"strings"

	"netcfg/internal/adapters/sysexec"
	"netcfg/internal/domain"
)

// Debian names the unit ssh; most other distributions call it sshd.
var candidates = []struct{ service, socket string }{
	{"ssh.service", "ssh.socket"},
	{"sshd.service", "sshd.socket"},
}

type Control struct{}

func New() *Control { return &Control{} }

// units resolves which pair this host actually ships.
func (c *Control) units(ctx context.Context) (service, socket string) {
	for _, pair := range candidates {
		if _, err := sysexec.Run(ctx, "systemctl", "cat", pair.service); err == nil {
			service = pair.service
			if _, err := sysexec.Run(ctx, "systemctl", "cat", pair.socket); err == nil {
				socket = pair.socket
			}
			return service, socket
		}
	}
	return "", ""
}

func (c *Control) Status(ctx context.Context) (domain.SSHStatus, error) {
	if !sysexec.Available("systemctl") {
		return domain.SSHStatus{Detail: domain.Msg("systemd is not available on this host")}, nil
	}

	service, socket := c.units(ctx)
	if service == "" {
		return domain.SSHStatus{Detail: domain.Msg("no SSH server is installed")}, nil
	}

	status := domain.SSHStatus{Available: true, Unit: service, Port: 22}
	// Socket activation means the socket unit, not the service, owns the port.
	status.Running = isActive(ctx, service) || (socket != "" && isActive(ctx, socket))
	status.EnabledAtBoot = isEnabled(ctx, service) || (socket != "" && isEnabled(ctx, socket))
	if port, ok := configuredPort(ctx); ok {
		status.Port = port
	}

	firewall := detectFirewall(ctx, status.Port)
	status.Firewall = firewall.Manager
	status.FirewallBlocks = firewall.Blocks()
	return status, nil
}

func (c *Control) Start(ctx context.Context) (bool, error) {
	service, socket := c.units(ctx)
	if service == "" {
		return false, domain.Unavailable("no SSH server is installed")
	}

	// Starting the socket is enough where it exists, and is what keeps the
	// port owned by systemd rather than a long-lived daemon.
	target := service
	if socket != "" {
		target = socket
	}
	if _, err := sysexec.Run(ctx, "systemctl", "start", target); err != nil {
		return false, domain.Internal("cannot start %s: %v", target, err)
	}

	port := 22
	if configured, ok := configuredPort(ctx); ok {
		port = configured
	}
	firewall := detectFirewall(ctx, port)
	if !firewall.Blocks() {
		return false, nil
	}
	if firewall.Manager != "ufw" {
		// Reported, not edited: see firewallState.
		return false, nil
	}
	if err := allowPort(ctx, port); err != nil {
		return false, domain.Internal("cannot open port %d in the firewall: %v", port, err)
	}
	return true, nil
}

func (c *Control) Stop(ctx context.Context, closeFirewall bool) error {
	service, socket := c.units(ctx)
	if service == "" {
		return domain.Unavailable("no SSH server is installed")
	}

	// The socket goes first; stopping only the service would leave systemd
	// listening and it would spawn a new one on the next connection.
	if socket != "" {
		if _, err := sysexec.Run(ctx, "systemctl", "stop", socket); err != nil {
			return domain.Internal("cannot stop %s: %v", socket, err)
		}
	}
	if _, err := sysexec.Run(ctx, "systemctl", "stop", service); err != nil {
		return domain.Internal("cannot stop %s: %v", service, err)
	}

	if closeFirewall {
		port := 22
		if configured, ok := configuredPort(ctx); ok {
			port = configured
		}
		if err := revokePort(ctx, port); err != nil {
			return domain.Internal("cannot close port %d in the firewall: %v", port, err)
		}
	}
	return nil
}

func isActive(ctx context.Context, unit string) bool {
	out, err := sysexec.Run(ctx, "systemctl", "is-active", unit)
	return err == nil && strings.TrimSpace(out) == "active"
}

func isEnabled(ctx context.Context, unit string) bool {
	out, err := sysexec.Run(ctx, "systemctl", "is-enabled", unit)
	if err != nil {
		return false
	}
	switch strings.TrimSpace(out) {
	case "enabled", "enabled-runtime", "alias", "static":
		return true
	default:
		return false
	}
}

// configuredPort asks sshd itself rather than parsing sshd_config, which can
// pull in Include directives.
func configuredPort(ctx context.Context) (int, bool) {
	out, err := sysexec.Run(ctx, "sshd", "-T")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(out, "\n") {
		field, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || field != "port" {
			continue
		}
		if port, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
			return port, true
		}
	}
	return 0, false
}
