# Deployment

## Prerequisites

```sh
apt install wpasupplicant iproute2
apt install hostapd dnsmasq iw          # for the fallback access point
apt install avahi-daemon                # optional, for .local names

systemctl enable --now systemd-networkd  # or use NetworkManager
systemctl enable --now wpa_supplicant@wlan0
systemctl disable --now hostapd dnsmasq  # netcfgd manages these itself
```

`/etc/wpa_supplicant/wpa_supplicant-wlan0.conf`:

```
ctrl_interface=DIR=/run/wpa_supplicant GROUP=netdev
update_config=1
country=VN
```

See the sample in [deploy/wpa_supplicant-wlan0.conf.example](../deploy/wpa_supplicant-wlan0.conf.example).

On a host where NetworkManager owns the radio, skip both the package and the
configuration file: the application detects NetworkManager and drives Wi-Fi
through `nmcli` instead. See [networking.md](networking.md) for how the backend
is chosen per link.

## Installation

### From the .deb package

```sh
cd /tmp
base=https://github.com/thanhnhu/netcfg/releases/download/latest
curl -fsSLO "$base/SHA256SUMS"
deb=$(awk '/arm64\.deb$/ {print $2}' SHA256SUMS)   # or amd64 / armhf
curl -fsSLO "$base/$deb"
grep "$deb" SHA256SUMS | sha256sum -c -

sudo apt install "./$deb"
```

Download somewhere world readable. apt drops to the `_apt` user to fetch, even
for a local file, and cannot reach `/root`; it then prints a notice and carries
on unsandboxed, which works but says more than it needs to.

Package versions follow the last `v*` tag: a release is `netcfg_0.1.0_arm64.deb`
and a rolling build is `netcfg_0.1.0+latest_arm64.deb`. Every rolling build
carries that same version, so apt considers a newer one already installed — use
`sudo apt install --reinstall ./netcfg_*.deb` to move between two of them.

The package does what `install.sh` does, through dpkg: it creates the `netcfg`
system user, installs the units into `/lib/systemd/system`, creates the state and
configuration directories, asks for the administrator password over debconf,
disables `hostapd` and `dnsmasq` so netcfgd can drive them itself, and enables
both services. The binaries go to `/usr/bin`, not `/usr/local/bin`, so a host
must not have both installations at once.

With a noninteractive frontend debconf answers nothing, no account is created and
`netcfg-web` is left stopped; `netcfgd` still starts. Finish the install with:

```sh
sudo netcfg-web -set-password -username admin -config /etc/netcfg-web/config.json
sudo systemctl enable --now netcfg-web
```

Or preseed the answer before installing:

```sh
echo 'netcfg netcfg/admin-password password s3cret' | sudo debconf-set-selections
```

The clear text password is passed to `netcfg-web -set-password` through
`NETCFG_PASSWORD` and cleared from the debconf database in the same postinst run;
only the PBKDF2 digest is kept.

`apt remove netcfg` stops and removes the services and the binaries. `apt purge
netcfg` additionally deletes `/etc/netcfg-web`, `/var/lib/netcfg-web` and
`/var/lib/netcfgd` and drops the `netcfg` user. Neither reverts the network
configuration that was written to `/etc/systemd/network` or
`/etc/network/interfaces.d`.

To build the package yourself, on a Debian host or in WSL:

```sh
make deb-arm64          # also deb-amd64, deb-armv7; needs dpkg-deb
```

### From a release

Nothing but `curl` and `tar` is needed on the device:

```sh
mkdir -p ~/netcfg-install && cd ~/netcfg-install
base=https://github.com/thanhnhu/netcfg/releases/download/latest
curl -fsSLO "$base/netcfg-latest-linux-arm64.tar.gz"
curl -fsSLO "$base/SHA256SUMS"
grep linux-arm64 SHA256SUMS | sha256sum -c -

tar -xzf netcfg-*-linux-arm64.tar.gz && cd netcfg-*-linux-arm64
cat VERSION
sudo sh deploy/install.sh .
```

`/releases/download/latest/` names the tag directly. `/releases/latest/download/`
is a different route that resolves to the newest **non-prerelease**, so it returns
404 while the rolling build is the only release. For a versioned archive take the
file name from the Releases page.

Each archive carries the two binaries, the `deploy/` directory and a `VERSION`
file naming the tag, commit and build time. Check the SHA256 before running
anything: these binaries run as root.

The rolling `latest` prerelease is replaced on every manual build, so a device
installed from it cannot be told apart from another by version alone — that is
what `VERSION` is for.

### From a local build

```sh
sudo ./deploy/install.sh dist
```

The script creates the `netcfg` system user, installs the binaries into `/usr/local/bin`, installs both systemd units, creates the directories with correct ownership, asks for an administrator password, warns about missing prerequisites, enables the services and prints the reachable URLs.

### By hand

```sh
groupadd --system netcfg
useradd --system --gid netcfg --home-dir /var/lib/netcfg-web --shell /usr/sbin/nologin netcfg

install -m 0755 dist/netcfgd dist/netcfg-web /usr/local/bin/
install -m 0644 deploy/netcfgd.service deploy/netcfg-web.service /etc/systemd/system/

install -d -m 0750 -o root   -g netcfg /etc/netcfg-web
install -d -m 0700 -o netcfg -g netcfg /var/lib/netcfg-web
install -d -m 0700 -o root   -g root   /var/lib/netcfgd

netcfg-web -set-password -username admin -config /etc/netcfg-web/config.json
chown root:netcfg /etc/netcfg-web/config.json && chmod 0640 /etc/netcfg-web/config.json

systemctl daemon-reload
systemctl enable --now netcfgd netcfg-web
```

Set the password non-interactively:

```sh
NETCFG_PASSWORD='...' netcfg-web -set-password -username admin
```

## File layout

| Path | Owner | Contents |
|---|---|---|
| `/etc/netcfg-web/config.json` | `root:netcfg` `0640` | Administrator account (PBKDF2 digest only) |
| `/var/lib/netcfg-web/tls/` | `netcfg` `0700` | Self-signed certificate |
| `/var/lib/netcfg-web/sessions.json` | `netcfg` `0600` | Session digests |
| `/var/lib/netcfgd/desired.json` | `root` `0600` | The configuration the operator asked for |
| `/var/lib/netcfgd/last-known-good.json` | `root` `0600` | Rollback target |
| `/var/lib/netcfgd/history/` | `root` `0700` | Last 20 snapshots |
| `/run/netcfgd/` | `root:netcfg` `0750` | Holds the socket; the group must be able to traverse it |
| `/run/netcfgd/netcfgd.sock` | `root:netcfg` `0660` | RPC socket |
| `/etc/default/netcfgd` | `root` `0644` | `NETCFGD_OPTS`, the per-host options the unit appends to `ExecStart` |

## Letting the interface switch SSH on and off

The SSH endpoints answer `503` unless `netcfgd` runs with `-allow-ssh`, because
opening a shell from the web tier is a real widening of the attack surface. The
option lives in `/etc/default/netcfgd`, which the unit reads through
`EnvironmentFile`:

```sh
NETCFGD_OPTS="-allow-ssh"
```

The package asks about it during installation and writes the file from the
answer. Change it later with:

```sh
sudo dpkg-reconfigure netcfg
```

Editing the file by hand also works — the package then recognises it as yours
and stops rewriting it, so any other flag you put in `NETCFGD_OPTS` survives an
upgrade. Restart the agent afterwards:

```sh
sudo systemctl restart netcfgd
```

With `install.sh`, pass `NETCFG_ALLOW_SSH=1` to get the same file with the
option already on.

## Command line flags

### `netcfgd`

| Flag | Default | Meaning |
|---|---|---|
| `-socket` | `/run/netcfgd/netcfgd.sock` | Socket served to the web tier |
| `-socket-group` | `netcfg` | Group allowed to reach the socket |
| `-allow-users` | `netcfg` | Users allowed to call the agent (root always is) |
| `-ctrl-dir` | `/run/wpa_supplicant` | wpa_supplicant control sockets |
| `-ctrl-local-dir` | `/run/netcfgd` | Where the local reply socket is bound |
| `-wpa-conf-dir` | `/etc/wpa_supplicant` | Where saved networks are written |
| `-state-dir` | `/var/lib/netcfgd` | Desired state and history |
| `-network-dir` | `/etc/systemd/network` | networkd unit directory |
| `-ifupdown-dir` | `/etc/network/interfaces.d` | ifupdown drop-in directory |
| `-confirm-window` | `90s` | Default confirmation window |
| `-probe-targets` | – | Extra `host:port` probes besides the gateway |
| `-failover-monitor` | `true` | Probe each default route and demote one that stops forwarding |
| `-failover-interval` | `10s` | How often each link is probed |
| `-failover-fails` | `3` | Consecutive failures before a link is demoted |
| `-failover-recovers` | `2` | Consecutive successes before it is restored |
| `-ap-fallback` | `true` | Start the fallback AP after losing connectivity |
| `-ap-fallback-after` | `5m` | How long without connectivity before it starts |
| `-ap-auto-stop` | `true` | Stop the AP once connectivity returns |
| `-ap-ssid` / `-ap-passphrase` | `netcfg` / `12345678` | Pin the fallback AP credentials. The defaults are guessable on purpose; change them where that matters |
| `-ap-channel` / `-ap-country` | `6` / `VN` | hostapd parameters |
| `-ap-address` | `192.168.4.1/24` | Portal address the AP hands out |
| `-allow-ssh` | `false` | Let the interface open the SSH server for a diagnostic window. Set through `NETCFGD_OPTS` in `/etc/default/netcfgd` |
| `-log-format` / `-log-level` | `text` / `info` | Use `json` under systemd |

### `netcfg-web`

| Flag | Default | Meaning |
|---|---|---|
| `-listen` | `:8090` | Main listen address |
| `-agent-socket` | `/run/netcfgd/netcfgd.sock` | Agent socket |
| `-config` | `/etc/netcfg-web/config.json` | Credential file |
| `-state-dir` | `/var/lib/netcfg-web` | Sessions and certificates |
| `-tls-cert` / `-tls-key` | – | Use an existing certificate |
| `-tls-dir` | `<state-dir>/tls` | Where the self-signed certificate is kept |
| `-tls-hosts` | – | Extra names or IPs for the self-signed certificate |
| `-no-tls` | `false` | Plain HTTP, only behind a reverse proxy |
| `-portal-listen` | – | Captive portal address, e.g. `:80` |
| `-portal-url` | `http://192.168.4.1/` | Portal redirect target |
| `-session-ttl` | `30m` | Idle time before a session expires |
| `-session-max` | `12h` | Absolute session lifetime |
| `-trusted-proxy` | – | Proxy IPs trusted for `X-Forwarded-For` |
| `-log-format` / `-log-level` | `text` / `info` | Use `json` under systemd |
| `-set-password` / `-username` | – | Set the account and exit |

## LAN access

### HTTPS with a self-signed certificate

The certificate is generated on first start. Its SANs cover `localhost`, the hostname, `<hostname>.local`, `127.0.0.1`, `::1` and **every non-loopback IP address** of the machine, so reaching it by IP does not trigger a name mismatch.

```sh
journalctl -u netcfg-web | grep fingerprint
```

After changing to a static IP the old certificate may lack the new SAN. Regenerate it:

```sh
systemctl stop netcfg-web
rm /var/lib/netcfg-web/tls/server.*
systemctl start netcfg-web
```

Or declare it up front: `-tls-hosts 192.168.1.50,device.company.local`

### .local names over mDNS

```sh
apt install avahi-daemon
install -m 0644 deploy/avahi-netcfg-web.service.xml /etc/avahi/services/netcfg-web.service
systemctl restart avahi-daemon
```

Then browse to `https://<hostname>.local:8090/`.

### Firewall

```sh
ufw allow from 192.168.0.0/16 to any port 8090 proto tcp
nft add rule inet filter input ip saddr 192.168.0.0/16 tcp dport 8090 accept
```

Port 80 (the captive portal) only needs to be reachable on the fallback AP network, not on the main LAN.

### Behind a reverse proxy

```sh
netcfg-web -no-tls -listen 127.0.0.1:8080 -trusted-proxy 127.0.0.1
```

nginx:

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $remote_addr;
    proxy_buffering off;            # required: SSE stalls with buffering on
    proxy_read_timeout 3600s;
}
```

`proxy_buffering off` is mandatory, otherwise the event stream never reaches the browser.

## Operations

```sh
systemctl status netcfgd netcfg-web
journalctl -u netcfgd -f
journalctl -u netcfg-web | grep audit        # who changed what, and when
curl -sk https://localhost:8090/healthz
```

Inspect the desired state and the rollback target:

```sh
jq . /var/lib/netcfgd/desired.json
jq . /var/lib/netcfgd/last-known-good.json
ls -t /var/lib/netcfgd/history/ | head
```

## Troubleshooting

| Symptom | Usual cause |
|---|---|
| `netcfgd` restarts forever, `status=226/NAMESPACE` | A path in `ReadWritePaths=` does not exist. They carry a `-` prefix so systemd may skip them; an edited unit that dropped the prefix fails on any host without ifupdown |
| `503 cannot reach netcfgd at /run/netcfgd/netcfgd.sock` | The agent is not running. `systemctl status netcfgd` and the journal say why |
| `503 ... connect: permission denied` | The socket exists but its directory is not searchable by the `netcfg` group. netcfgd repairs this at startup; a stale process from before that fix needs a restart |
| `503 no Wi-Fi backend detected` | Neither NetworkManager nor a wpa_supplicant control socket is present; the message names why each one declined |
| `503 wpa_supplicant on <link> is not answering yet` | The supplicant is restarting, or the socket file it left behind is orphaned. It clears itself within seconds |
| `503 the fallback access point is using the radio` | The AP holds the radio in exclusive mode; stop it to manage Wi-Fi |
| `Connected, but the configuration could not be saved` | `update_config=1` is missing — Wi-Fi will be lost on reboot |
| `503 no IP management backend detected` | No networkd, NetworkManager or ifupdown manages that link |
| The configuration reverts after 90 seconds | Working as designed — you did not confirm |
| `409 a change is already awaiting confirmation` | Confirm or roll back the previous change first |
| The fallback AP does not come up | `hostapd`/`dnsmasq`/`iw` missing, or their system units are running and holding the port |
| The captive portal never appears | `-portal-listen :80` missing, or `CAP_NET_BIND_SERVICE` not granted |
| Unreachable after an IP change | Wait for the automatic rollback, use Ethernet, or wait for the fallback AP |

### Recovering from total loss of access

```sh
# From the physical console or over SSH on Ethernet
systemctl stop netcfgd netcfg-web
cp /var/lib/netcfgd/last-known-good.json /var/lib/netcfgd/desired.json
systemctl start netcfgd netcfg-web    # the agent reconciles at startup
```

To start over completely, delete `/var/lib/netcfgd/desired.json` and configure the network with the usual system tools.
