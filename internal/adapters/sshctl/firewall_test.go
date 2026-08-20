package sshctl

import "testing"

// TestParseUfwStatus locks down the table format, including the two ways a
// reader could get it wrong: "inactive" contains the word "active", and an SSH
// rule may be written as an application profile rather than a port.
func TestParseUfwStatus(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		port    int
		active  bool
		allowed bool
	}{
		{
			name:   "inactive is not mistaken for active",
			output: "Status: inactive\n",
			port:   22,
		},
		{
			name: "active with the port allowed",
			output: `Status: active

To                         Action      From
--                         ------      ----
22/tcp                     ALLOW       Anywhere
22/tcp (v6)                ALLOW       Anywhere (v6)
`,
			port: 22, active: true, allowed: true,
		},
		{
			name: "active with the port missing",
			output: `Status: active

To                         Action      From
--                         ------      ----
8443/tcp                   ALLOW       192.168.0.0/16
`,
			port: 22, active: true,
		},
		{
			name: "the OpenSSH application profile counts as allowed",
			output: `Status: active

To                         Action      From
--                         ------      ----
OpenSSH                    ALLOW       Anywhere
`,
			port: 22, active: true, allowed: true,
		},
		{
			name: "a deny rule does not count as allowed",
			output: `Status: active

To                         Action      From
--                         ------      ----
22/tcp                     DENY        Anywhere
`,
			port: 22, active: true,
		},
		{
			name:   "a non standard port is matched on its own number",
			output: "Status: active\n\n2222/tcp   ALLOW   Anywhere\n",
			port:   2222, active: true, allowed: true,
		},
		{
			name:   "another port does not satisfy the wanted one",
			output: "Status: active\n\n2222/tcp   ALLOW   Anywhere\n",
			port:   22, active: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			active, allowed := parseUfwStatus(tc.output, tc.port)
			if active != tc.active || allowed != tc.allowed {
				t.Errorf("got active=%v allowed=%v, want active=%v allowed=%v",
					active, allowed, tc.active, tc.allowed)
			}
		})
	}
}

func TestFirewallBlocksOnlyWhenActiveAndNotAllowed(t *testing.T) {
	cases := []struct {
		state firewallState
		want  bool
	}{
		{firewallState{}, false},
		{firewallState{Manager: "ufw", Active: false, Allowed: false}, false},
		{firewallState{Manager: "ufw", Active: true, Allowed: true}, false},
		{firewallState{Manager: "ufw", Active: true, Allowed: false}, true},
	}
	for _, tc := range cases {
		if got := tc.state.Blocks(); got != tc.want {
			t.Errorf("%+v.Blocks() = %v, want %v", tc.state, got, tc.want)
		}
	}
}
