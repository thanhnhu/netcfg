# netcfg-web

[Tiếng Việt](README.md) · **English**

A web interface for configuring the network on headless Debian devices: Wi-Fi through whichever subsystem holds the radio (`wpa_supplicant` or NetworkManager), IPv4/IPv6 addressing through whichever backend actually manages the host, and several layers of safety so you can never lock yourself out of the device.

Two static Go binaries, no external dependencies. The user interface is embedded in the binary and available in **Vietnamese (default)** and English.

---

## Three core properties

### 1. You cannot lock yourself out — commit–confirm

Every change that could cut your connection is applied together with a rollback timer. Fail to confirm in time and the device restores the previous configuration.

```
POST /api/v1/plans        → preview the diff, see up front whether it is risky
POST /api/v1/ip           → apply; the agent arms a 90 second timer
POST .../confirm          → keep the change
(silence)                 → automatic rollback
```

The timer lives inside the **root** process, not in the browser or the web tier, so it still fires when both are gone. Harmless changes such as editing DNS are committed immediately without prompting.

### 2. Privilege separation

```
Browser ──HTTPS──► netcfg-web        user netcfg, no capabilities
                        │
                   Unix socket + SO_PEERCRED
                        ▼
                   netcfgd           root, CAP_NET_ADMIN
```

The entire network facing attack surface — TLS, HTTP, cookies, templates — runs unprivileged. The root process accepts only 23 strictly typed commands.

### 3. Fallback access point — the last resort

When no network is reachable, the device publishes its own Wi-Fi with a captive portal. Plug in power, open a phone, reconfigure it — no need to carry a monitor to the site.

---

## Wired / Wi-Fi failover

On a device with both Ethernet and Wi-Fi the order is decided by the **route metric**: the kernel uses the default route with the lowest metric and falls through to the next one when that link drops.

```
default via 192.168.2.1 dev eth0  metric 100   ← used while the cable is live
default via 192.168.2.1 dev wlan0 metric 600   ← standby
```

The **Failover** entry in the main menu lists every interface in its current order of preference, each row carrying its own metric field and a switch to drop that interface out of the fallback chain. The value is written through whichever backend owns the host — `RouteMetric=` for systemd-networkd, `ipv4.route-metric` for NetworkManager, `metric` for ifupdown. Changing it counts as a disruptive change, so it still goes through commit–confirm.

This is kernel level failover for a link going *down*. A gateway that stays up but stops forwarding is caught by the active monitor instead: `netcfgd` probes each interface through that interface alone, and after three failed checks moves its default route to the bottom of the table — in the kernel only, never in a configuration file, and only while another interface is still healthy. The panel shows each interface as up or down, reachable or not, demoted or not. Turn it off with `-failover-monitor=false`.

---

## Quick start

### Install the package

One file, one command; apt handles upgrades and removal:

```sh
# apt fetches as the _apt user, which cannot read /root
cd /tmp

base=https://github.com/thanhnhu/netcfg/releases/download/latest
curl -fsSLO "$base/SHA256SUMS"
# The .deb file name carries the version, so take it from SHA256SUMS
deb=$(awk '/arm64\.deb$/ {print $2}' SHA256SUMS)   # or amd64 / armhf
curl -fsSLO "$base/$deb"
grep "$deb" SHA256SUMS | sha256sum -c -

sudo apt install "./$deb"
```

The package asks for the administrator password and whether the interface may
switch the SSH server on and off (off by default), creates the `netcfg` system
user, installs both units and enables them, and pulls in `wpasupplicant`,
`hostapd`, `dnsmasq` and `iw` as recommended packages. Binaries land in
`/usr/bin`, so do not run `install.sh` on the same host — it writes a second copy
into `/usr/local/bin`. To change an answer later, run
`sudo dpkg-reconfigure netcfg`.

With no password given, or on an unattended install with no screen to prompt on,
`netcfgd` still starts and `netcfg-web` waits for:

```sh
sudo netcfg-web -set-password -username admin -config /etc/netcfg-web/config.json
sudo systemctl enable --now netcfg-web
```

Upgrade by installing the newer `.deb` the same way. `apt remove netcfg` stops
and removes the services but keeps the password, sessions and history;
`apt purge netcfg` deletes those too.

### Install a release archive

Runs on the device itself, with no Go toolchain and no build machine:

```sh
apt install wpasupplicant iproute2 hostapd dnsmasq iw

mkdir -p ~/netcfg-install && cd ~/netcfg-install
base=https://github.com/thanhnhu/netcfg/releases/download/latest
curl -fsSLO "$base/netcfg-latest-linux-arm64.tar.gz"    # or amd64 / armv7
curl -fsSLO "$base/SHA256SUMS"
grep linux-arm64 SHA256SUMS | sha256sum -c -

tar -xzf netcfg-*-linux-arm64.tar.gz && cd netcfg-*-linux-arm64
cat VERSION                  # the tag, commit and build time of this archive
sudo sh deploy/install.sh .
```

Do not skip the `sha256sum` step: the archive carries binaries that will run as root.

The path is `/releases/download/latest/`, not `/releases/latest/download/` — the
second form skips prereleases, and the rolling build is a prerelease.

For a versioned release, take the file name from the
[Releases page](https://github.com/thanhnhu/netcfg/releases): those archives
carry the version, e.g. `netcfg-v0.1.0-linux-arm64.tar.gz`.

Besides the `v*` releases, a manual build can publish a prerelease called
`latest`. It is convenient for a quick try, but it is replaced without notice, so
keep it away from anything in production.

### Build from source

On the build machine:

```sh
make arm64                 # or: make amd64 / make armv7
```

`make deb-arm64` (also `deb-amd64`, `deb-armv7`) wraps the same binaries into a
`.deb`. It needs `dpkg-deb`, so run it on a Debian host or in WSL.

On the Debian device:

```sh
apt install wpasupplicant iproute2 hostapd dnsmasq iw
scp dist/netcfgd dist/netcfg-web deploy/*.service deploy/install.sh pi@device:~/
sudo ./install.sh .
```

Either way the script creates the system user, installs both systemd units, asks for an administrator password, checks the prerequisites and prints the URLs you can reach the device at.

**Required** — `/etc/wpa_supplicant/wpa_supplicant-wlan0.conf` must contain:

```
ctrl_interface=DIR=/run/wpa_supplicant GROUP=netdev
update_config=1
country=VN
```

Without `update_config=1` Wi-Fi still connects but the configuration is lost after a reboot (the application warns you about this).

On a host running NetworkManager, skip this: the application detects it and drives the radio through `nmcli` instead.

### Upgrading

Copy the binaries and the units over, then restart. Password, sessions and saved configuration all survive:

```sh
sudo install -m 0755 netcfgd netcfg-web /usr/local/bin/
sudo install -m 0644 deploy/netcfgd.service deploy/netcfg-web.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl restart netcfgd netcfg-web
systemctl is-active netcfgd netcfg-web
```

Do not skip the two `.service` files: fixes land there as often as in the binaries.

### Uninstalling

Installed from the `.deb`:

```sh
sudo apt purge netcfg
```

Installed with `install.sh`:

```sh
sudo systemctl disable --now netcfgd netcfg-web
sudo rm -f /etc/systemd/system/netcfgd.service /etc/systemd/system/netcfg-web.service
sudo systemctl daemon-reload && sudo systemctl reset-failed

sudo rm -f /usr/local/bin/netcfgd /usr/local/bin/netcfg-web
sudo rm -rf /etc/netcfg-web        # administrator password
sudo rm -rf /var/lib/netcfg-web    # sessions, TLS certificate
sudo rm -rf /var/lib/netcfgd       # desired.json, last known good, history
sudo rm -rf /run/netcfgd

sudo userdel netcfg
sudo groupdel netcfg
```

Check nothing is left:

```sh
systemctl list-units --all 'netcfg*'
getent passwd netcfg; getent group netcfg
```

**Uninstalling does not revert the network.** Whatever was written to
`/etc/systemd/network/` stays, and the host keeps running with the metrics and
addresses you set. `install.sh` also ran `systemctl disable hostapd dnsmasq`; if
you used those for something else, enable them again by hand.

---

## LAN access

By default the UI listens on `:8090` on **every** interface over HTTPS, using a self-signed certificate that is generated automatically and covers the hostname, `<hostname>.local` and every IP address of the machine.

```sh
https://192.168.1.50:8090/          # by IP
https://device.local:8090/          # requires avahi-daemon

journalctl -u netcfg-web | grep fingerprint   # compare when the browser warns
ufw allow from 192.168.0.0/16 to any port 8090 proto tcp
```

For mDNS, reverse proxies and firewalls see [docs/deployment.md](docs/deployment.md).

---

## The administrator account

A single account, stored as a PBKDF2-HMAC-SHA256 digest over 240,000 iterations with its own salt. Set it during `install.sh`, or at any time with:

```sh
netcfg-web -set-password -username admin -config /etc/netcfg-web/config.json
```

From the interface, open the user name in the top right corner → **Change password**. Every other session is revoked afterwards while the one you are working in survives, so the change cannot lock you out. Guessing the current password here is rate limited by the same counter that guards the sign-in form.

---

## Documentation

| Document | Contents |
|---|---|
| [docs/architecture.md](docs/architecture.md) | Hexagonal layering, data flow, package layout, decision records |
| [docs/networking.md](docs/networking.md) | The three IP backends, IPv4/IPv6 dual stack, route metric failover, fallback AP and captive portal |
| [docs/api.md](docs/api.md) | API v1 reference, SSE, RFC 9457 error format |
| [docs/deployment.md](docs/deployment.md) | Installation, systemd, command line flags, LAN access, troubleshooting |
| [docs/security.md](docs/security.md) | Threat model and the controls that answer it |
| [docs/development.md](docs/development.md) | Build, test, fuzz, contribution conventions |

The files under `docs/` are written in English because they target developers; the operator facing interface defaults to Vietnamese.

---

## Status

| Feature | State |
|---|---|
| Wi-Fi: scan, connect, save/forget profiles, WPA2/WPA3 | Done |
| IPv4 static/DHCP, IPv6 static/auto/disabled | Done |
| Wired / Wi-Fi failover through route metrics | Done |
| Active failover (ping based health checks) | Done |
| IP backends: systemd-networkd, NetworkManager, ifupdown | Done |
| Wi-Fi backends: wpa_supplicant, NetworkManager | Done |
| Commit–confirm with automatic rollback | Done |
| Fallback access point with captive portal | Done |
| Sessions that survive a service restart | Done |
| System metrics: CPU, RAM, disks, auto-discovered sensors | Done |
| Administrator password change from the UI | Done |
| Localized interface (vi/en) | Done |
| VLAN, bonding, bridge configuration | Not yet |
| Multiple users and role based access | Not yet |
