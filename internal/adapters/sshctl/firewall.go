package sshctl

import (
	"context"
	"strconv"
	"strings"

	"netcfg/internal/adapters/sysexec"
)

// firewallState is what a packet filter says about the SSH port. Only ufw is
// driven: it has a stable, declarative interface. Raw nftables and iptables are
// reported but never edited, because a wrong rule there locks the operator out
// of the device for good.
type firewallState struct {
	Manager string
	Active  bool
	Allowed bool
}

// Blocks reports a filter that would drop an SSH connection.
func (f firewallState) Blocks() bool { return f.Active && !f.Allowed }

func detectFirewall(ctx context.Context, port int) firewallState {
	if sysexec.Available("ufw") {
		out, err := sysexec.Run(ctx, "ufw", "status")
		if err == nil {
			active, allowed := parseUfwStatus(out, port)
			return firewallState{Manager: "ufw", Active: active, Allowed: allowed}
		}
	}
	return firewallState{}
}

// parseUfwStatus reads the table `ufw status` prints. It is separate from the
// command so the format can be tested without ufw installed.
func parseUfwStatus(output string, port int) (active, allowed bool) {
	wanted := strconv.Itoa(port)

	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimSpace(raw)

		if rest, ok := strings.CutPrefix(line, "Status:"); ok {
			// "inactive" contains "active", so compare the whole word.
			active = strings.TrimSpace(rest) == "active"
			continue
		}

		// Rules read "To  Action  From", e.g. "22/tcp  ALLOW  Anywhere".
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.EqualFold(fields[1], "ALLOW") {
			continue
		}
		target, _, _ := strings.Cut(fields[0], "/")
		// ufw also lets a rule be written as the OpenSSH application profile.
		if target == wanted || strings.EqualFold(target, "OpenSSH") {
			allowed = true
		}
	}
	return active, allowed
}

func allowPort(ctx context.Context, port int) error {
	_, err := sysexec.Run(ctx, "ufw", "allow", strconv.Itoa(port)+"/tcp")
	return err
}

func revokePort(ctx context.Context, port int) error {
	_, err := sysexec.Run(ctx, "ufw", "delete", "allow", strconv.Itoa(port)+"/tcp")
	return err
}
