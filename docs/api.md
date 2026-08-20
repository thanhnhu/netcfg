# API v1

Every endpoint under `/api/v1` requires a session cookie. Every state changing verb additionally requires the `X-CSRF-Token` header and a same-origin `Origin`.

The CSRF token is in the meta tag of the main page:

```html
<meta name="csrf-token" content="...">
```

## Error format

Errors follow RFC 9457 with `Content-Type: application/problem+json`:

```json
{
  "type": "https://netcfg.local/errors/invalid_input",
  "title": "Invalid request",
  "status": 400,
  "detail": "gateway 10.0.0.1 is outside subnet 192.168.1.0/24",
  "instance": "/api/v1/ip",
  "code": "invalid_input"
}
```

| `code` | HTTP | Meaning |
|---|---|---|
| `invalid_input` | 400 | The payload failed validation |
| `not_found` | 404 | Unknown interface, profile or generation |
| `conflict` | 409 | Another change is already awaiting confirmation |
| `unavailable` | 503 | wpa_supplicant or the IP backend is not ready |
| `internal` | 500 | Internal failure; details only in the logs |

## State

### `GET /api/v1/state?link=<name>`

Leave `link` empty to get the first wireless interface.

```json
{
  "links": [{ "name": "wlan0", "wireless": true, "operUp": true, "addresses": ["192.168.1.50/24"], "gateway": "192.168.1.1", "dns": ["1.1.1.1"], "mac": "dc:a6:32:11:22:33" }],
  "view": {
    "link": { "name": "wlan0", "wireless": true },
    "backend": "systemd-networkd",
    "ip": { "link": "wlan0", "mode": "dhcp", "mode6": "auto", "dns": [] },
    "wifi": { "state": "COMPLETED", "ssid": "My Home", "signal": -47, "associated": true, "profileId": 0 },
    "profiles": [{ "id": 0, "ssid": "My Home", "current": true, "enabled": true }],
    "pending": null,
    "hotspot": { "active": false },
    "notices": []
  },
  "serverTime": "2026-08-18T21:57:42Z"
}
```

`serverTime` lets the UI correct for clock skew while counting down a confirmation window.

### `GET /healthz`

No authentication. Returns `503` when the agent is unreachable.

```json
{ "web": "ok", "agent": "ok" }
```

### `GET /api/v1/system`

Host health, read from `/proc` and `/sys` on every call. Safe to poll; the UI
refreshes it every 5 seconds while the metrics panel is open.

```json
{
  "stats": {
    "at": "2026-08-19T10:38:01+07:00",
    "host": {
      "hostname": "device", "os": "Debian GNU/Linux 13 (trixie)",
      "kernel": "6.12.0-rpi", "arch": "arm64",
      "model": "Raspberry Pi 5 Model B", "cpuModel": "Cortex-A76"
    },
    "uptimeSeconds": 747170,
    "cpuCount": 4,
    "cpuPercent": 3.88,
    "load": [0.14, 0.07, 0.07],
    "memory": { "total": 3872239616, "used": 1630000000, "available": 2242239616, "percent": 42.1 },
    "swap": { "total": 0, "used": 0, "available": 0, "percent": 0 },
    "filesystems": [
      { "device": "/dev/root", "mountpoint": "/", "fstype": "ext4",
        "usage": { "total": 29338013696, "used": 9315819520, "available": 18505953280, "percent": 33.5 } }
    ],
    "sensors": [
      { "name": "coretemp", "sensors": [
        { "label": "Package id 0", "kind": "temperature", "value": 48, "unit": "°C", "critical": 100 },
        { "label": "Core 0", "kind": "temperature", "value": 46, "unit": "°C" }
      ]},
      { "name": "BAT0", "sensors": [
        { "label": "status", "kind": "state", "text": "Discharging" },
        { "label": "capacity", "kind": "charge", "value": 82, "unit": "%" }
      ]}
    ]
  }
}
```

`cpuPercent` is the delta between two polls; the very first call after the agent
starts reports the average since boot instead. `percent` divides by usable
capacity rather than raw size, so a filesystem agrees with `df`, which excludes
the blocks ext4 reserves for root.

### `GET /api/v1/failover`

What the active failover monitor sees. `demoted` marks a link whose default
route the monitor pushed down; the change is kernel only and reversed once the
path answers again.

```json
{
  "status": {
    "enabled": true,
    "interval": 10000000000,
    "fails": 3,
    "recovers": 2,
    "targets": ["1.1.1.1:53"],
    "links": [
      { "link": "eth0", "gateway": "192.168.1.1", "adminUp": true, "operUp": true,
        "reachable": false, "failures": 4, "successes": 0, "demoted": true,
        "detail": { "format": "no target responded through %s", "args": ["eth0"] },
        "since": "2026-08-19T10:31:00+07:00", "checkedAt": "2026-08-19T10:38:01+07:00" },
      { "link": "wlan0", "gateway": "192.168.2.1", "adminUp": true, "operUp": true,
        "reachable": true, "failures": 0, "successes": 42, "demoted": false }
    ],
    "at": "2026-08-19T10:38:01+07:00"
  }
}
```

`enabled` is false when the monitor was turned off with `-failover-monitor=false`
or when `iproute2` is missing, in which case `detail` says which. A state change
also arrives as a `failover` SSE event carrying the same object.

### Sensor discovery

Nothing in `sensors` is hard coded for a particular board. Each group is one
chip or power supply, and readings are whatever the kernel exposes:

| Source | Picked up |
|---|---|
| `/sys/class/hwmon/*` | `temp` · `fan` · `in` · `curr` · `power` · `energy` · `humidity` · `freq`, with `_label`, `_max` and `_crit` when present |
| `/sys/class/thermal/thermal_zone*` | Boards whose probes are not mirrored into hwmon |
| `/sys/class/power_supply/*` | Batteries, UPS hats and mains adapters: status, capacity, voltage, current, power, temperature |

Values are scaled to their natural unit following
`Documentation/hwmon/sysfs-interface`, so millivolts become volts and microwatts
become watts. hwmon is read first because it carries labels and alarm
thresholds; a thermal zone is skipped when a chip already reports the same probe
(`cpu-thermal` and `cpu_thermal` are treated as one). `kind` tells the interface
which unit and how many decimals to render, so a device with a fan or a shunt
needs no change to the UI.

A host with no sensors returns an empty list rather than an error.

## Wi-Fi

### `POST /api/v1/scan`

```json
{ "link": "wlan0" }
```

Waits for the actual `CTRL-EVENT-SCAN-RESULTS` event instead of sleeping a fixed interval. Results are deduplicated by SSID, keeping the strongest AP, sorted by descending signal.

```json
{ "networks": [{ "ssid": "My Home", "bssid": "aa:bb:cc:dd:ee:ff", "freq": 5180, "band": "5 GHz", "signal": -47, "quality": 100, "security": "psk-sae" }] }
```

`security` is one of `open` · `wep` · `psk` · `sae` · `psk-sae`. WEP is refused on connect.

### `POST /api/v1/wifi`

```json
{
  "link": "wlan0",
  "ssid": "My Home",
  "security": "psk",
  "passphrase": "...",
  "hidden": false,
  "confirmWindowSeconds": 90,
  "noRollback": false
}
```

Always creates a pending confirmation unless `noRollback` is true.

```json
{ "pending": { "generation": 7, "kind": "wifi", "link": "wlan0", "deadline": "...", "summary": [...] }, "warning": "" }
```

`warning` appears when the connection succeeded but the configuration could not be saved — the signature of a `wpa_supplicant.conf` without `update_config=1`.

### `POST /api/v1/profiles/select` · `/api/v1/profiles/remove`

```json
{ "link": "wlan0", "id": 0 }
```

### `POST /api/v1/profiles/secret`

Reveals the passphrase of a saved network. Same body as above.

```json
{ "ssid": "Home", "value": "hunter2hunter2", "hashed": false }
```

`hashed` is true when the backend only kept a derived key. That value still joins
the network, but it is not what the operator originally typed.

### `POST /api/v1/disconnect` · `/api/v1/reconnect`

```json
{ "link": "wlan0" }
```

## IP addressing

### `POST /api/v1/plans` — dry run

Same body as `/api/v1/ip`, but nothing is changed.

```json
{
  "link": "eth0",
  "backend": "systemd-networkd",
  "changes": [{ "field": "mode", "from": "dhcp", "to": "static" }],
  "disruptive": true,
  "warning": "This change may cut your connection to eth0."
}
```

`warning` states the risk only. What follows from it depends on the `noRollback`
the caller will send, which the dry run does not receive, so the caller spells
out the consequence itself.

### `POST /api/v1/ip`

```json
{
  "link": "eth0",
  "mode": "static",
  "address": "192.168.1.50/24",
  "gateway": "192.168.1.1",
  "mode6": "static",
  "address6": "2001:db8::10/64",
  "gateway6": "fe80::1",
  "metric": 100,
  "noDefaultRoute": false,
  "dns": ["1.1.1.1", "2606:4700:4700::1111"],
  "confirmWindowSeconds": 90,
  "noRollback": false
}
```

`metric` is the default route metric for both families. The lowest metric wins,
so it is what orders wired-over-Wi-Fi failover; omit it or send `0` to keep the
backend's own default. `noDefaultRoute` takes the link out of the failover order
altogether: it still gets an address but installs no default route, and any
static gateway on the plan is dropped. Changing either is treated as disruptive.

Returns `{"pending": null}` when the change cannot break connectivity — those are committed immediately.

## Commit–confirm

### `GET /api/v1/pending`

```json
{
  "pending": {
    "generation": 12,
    "kind": "ip",
    "link": "eth0",
    "startedAt": "2026-08-18T21:57:42Z",
    "deadline": "2026-08-18T21:59:12Z",
    "probe": { "ok": true, "detail": "gateway 192.168.1.1 responded" },
    "summary": [{ "field": "address", "from": "", "to": "192.168.1.50/24" }]
  }
}
```

### `POST /api/v1/pending/{generation}/confirm`

Stops the timer and promotes the configuration to last known good. Send an empty body `{}`.

### `POST /api/v1/pending/{generation}/rollback`

Reverts immediately on request.

Both return `404` when the generation does not match, meaning the timer already fired.

## Fallback access point

### `GET /api/v1/hotspot`

```json
{
  "status": {
    "active": true,
    "link": "ap0",
    "mode": "concurrent",
    "ssid": "netcfg",
    "passphrase": "12345678",
    "address": "192.168.4.1/24",
    "portalUrl": "http://192.168.4.1/",
    "since": "2026-08-18T22:03:00Z",
    "clients": 1,
    "reason": "no connectivity for 5m0s"
  }
}
```

### `POST /api/v1/hotspot/start`

```json
{ "link": "wlan0" }
```

Leave `link` empty to use the first wireless interface.

### `POST /api/v1/hotspot/stop`

Send an empty body `{}`.

## Account

### `POST /api/v1/password`

Changes the password of the signed-in administrator. The user name cannot be
changed here.

```json
{ "current": "old-secret", "new": "new-secret", "confirm": "new-secret" }
```

```json
{ "revokedSessions": 2 }
```

The new password must be 8–256 characters and differ from the current one. On
success every **other** session is revoked; the calling session survives, so the
operator is not signed out by their own change. A wrong current password counts
against the same rate limit as a failed sign-in and returns `400`; once the limit
is reached the endpoint answers `429`.

## Remote access

These three endpoints exist only when `netcfgd` runs with `-allow-ssh`. Without
it they answer `503`, because opening a shell on the device is not something the
web tier should be able to do by default.

### `GET /api/v1/ssh`

```json
{
  "status": {
    "available": true,
    "unit": "ssh.service",
    "running": false,
    "enabledAtBoot": false,
    "port": 22,
    "firewall": "ufw",
    "firewallBlocks": true,
    "stopsAt": "2026-08-19T21:30:00Z"
  }
}
```

`running` and `enabledAtBoot` are deliberately separate: a server started for a
diagnostic session is not the same as one the operator wants back after a reboot.
`stopsAt` appears only while netcfgd holds a timer to close access again.

### `POST /api/v1/ssh/enable`

```json
{ "windowSeconds": 1800 }
```

Starts the SSH server and arms a timer to close it again; the window is clamped
to 5 minutes–12 hours. If the operator had already enabled SSH at boot, no timer
is armed — the application will not switch off something it did not switch on.
When ufw is active and the port is not allowed, a matching rule is added and
removed with the window; nftables and iptables are reported but never edited.

### `POST /api/v1/ssh/disable`

Empty body. Stops the server and undoes the firewall rule if this application
added it.

## Real-time events

### `GET /api/v1/events`

Server-Sent Events. One RPC connection to the agent serves every open tab.

```
event: apply_pending
data: {"type":"apply_pending","link":"eth0","message":"Confirmation required...","data":{...},"at":"..."}
```

| `event` | When |
|---|---|
| `apply_pending` | A change was applied and is awaiting confirmation |
| `apply_confirmed` | Confirmed, or committed immediately |
| `apply_reverted` | Rolled back, either on timeout or on request |
| `probe` | A connectivity check produced a result |
| `wifi_state` | The Wi-Fi association changed; `message` says **"Wrong Wi-Fi password"** on `WRONG_KEY` |
| `scan_results` | The radio finished a scan |
| `hotspot` | The fallback AP started or stopped |

A `: ping` comment keeps the stream alive every 20 seconds.

## curl examples

```sh
BASE=https://device:8090
COOKIE=$(mktemp)

curl -sk -c "$COOKIE" -d 'username=admin&password=...' "$BASE/login" -o /dev/null
CSRF=$(curl -sk -b "$COOKIE" "$BASE/" | grep -oP 'csrf-token" content="\K[a-f0-9]+')

curl -sk -b "$COOKIE" "$BASE/api/v1/state" | jq .

curl -sk -b "$COOKIE" -H "X-CSRF-Token: $CSRF" -H "Content-Type: application/json" \
  -d '{"link":"eth0","mode":"static","address":"192.168.1.50/24","gateway":"192.168.1.1"}' \
  "$BASE/api/v1/plans" | jq .
```
