#!/bin/sh
# Install netcfgd + netcfg-web on a Debian host.
# Usage: sudo ./deploy/install.sh [directory-containing-the-binaries]
set -eu

BIN_DIR="${1:-dist}"
PREFIX=/usr/local/bin
UNIT_DIR=/etc/systemd/system
SRC_DIR="$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)"

if [ "$(id -u)" -ne 0 ]; then
    echo "This script must run as root." >&2
    exit 1
fi

for binary in netcfgd netcfg-web; do
    if [ ! -f "$BIN_DIR/$binary" ]; then
        echo "$BIN_DIR/$binary not found. Run 'make linux' first." >&2
        exit 1
    fi
done

echo "==> Creating the netcfg system user"
if ! getent group netcfg >/dev/null; then
    groupadd --system netcfg
fi
if ! getent passwd netcfg >/dev/null; then
    useradd --system --gid netcfg --home-dir /var/lib/netcfg-web \
        --shell /usr/sbin/nologin --comment "netcfg web UI" netcfg
fi

echo "==> Installing binaries"
install -m 0755 "$BIN_DIR/netcfgd" "$PREFIX/netcfgd"
install -m 0755 "$BIN_DIR/netcfg-web" "$PREFIX/netcfg-web"

echo "==> Installing systemd units"
install -m 0644 "$SRC_DIR/netcfgd.service" "$UNIT_DIR/netcfgd.service"
install -m 0644 "$SRC_DIR/netcfg-web.service" "$UNIT_DIR/netcfg-web.service"

install -d -m 0750 -o netcfg -g netcfg /etc/netcfg-web
install -d -m 0700 -o netcfg -g netcfg /var/lib/netcfg-web
install -d -m 0700 -o root -g root /var/lib/netcfgd

# NETCFGD_OPTS is where per-host options live. -allow-ssh stays off unless it is
# asked for: it lets whoever reaches the web interface open a shell.
if [ ! -f /etc/default/netcfgd ]; then
    options=""
    [ "${NETCFG_ALLOW_SSH:-0}" = "1" ] && options="-allow-ssh"
    cat > /etc/default/netcfgd <<EOF
# Extra options for netcfgd. "-allow-ssh" lets the web interface open and close
# the device's SSH server; see netcfgd -h for the rest.
NETCFGD_OPTS="$options"
EOF
    chmod 0644 /etc/default/netcfgd
    [ -n "$options" ] || echo "    (run with NETCFG_ALLOW_SSH=1, or edit /etc/default/netcfgd, to allow the SSH toggle)"
fi

if [ ! -f /etc/netcfg-web/config.json ]; then
    echo "==> Setting the administrator password"
    "$PREFIX/netcfg-web" -set-password -username admin -config /etc/netcfg-web/config.json
fi
# The web UI rewrites this file when the operator changes the password.
chown netcfg:netcfg /etc/netcfg-web/config.json
chmod 0600 /etc/netcfg-web/config.json

echo "==> Checking wpa_supplicant"
if ! command -v wpa_supplicant >/dev/null; then
    echo "    WARNING: wpasupplicant is not installed (apt install wpasupplicant)" >&2
fi
for conf in /etc/wpa_supplicant/wpa_supplicant-*.conf; do
    [ -f "$conf" ] || continue
    grep -q '^update_config=1' "$conf" || \
        echo "    WARNING: $conf lacks update_config=1, Wi-Fi settings will be lost on reboot" >&2
    grep -q 'ctrl_interface=' "$conf" || \
        echo "    WARNING: $conf lacks ctrl_interface=DIR=/run/wpa_supplicant" >&2
done

echo "==> Checking the fallback access point"
for tool in hostapd dnsmasq iw; do
    command -v "$tool" >/dev/null || \
        echo "    WARNING: $tool is missing, the fallback AP will not work (apt install hostapd dnsmasq iw)" >&2
done
# netcfgd starts hostapd and dnsmasq itself, so their system units must stay idle.
for unit in hostapd dnsmasq; do
    if systemctl is-enabled "$unit" >/dev/null 2>&1; then
        echo "    ==> Disabling $unit.service (netcfgd manages this process itself)"
        systemctl disable --now "$unit" >/dev/null 2>&1 || true
    fi
done

echo "==> Enabling services"
systemctl daemon-reload
systemctl enable --now netcfgd.service
systemctl enable --now netcfg-web.service

# A unit that cannot start is retried in the background, so enabling one says
# nothing about whether it runs. Without this check the installer reports success
# while the agent sits in a restart loop.
echo "==> Verifying the services came up"
sleep 3
failed=""
for unit in netcfgd netcfg-web; do
    state="$(systemctl is-active "$unit" 2>/dev/null || true)"
    if [ "$state" = "active" ]; then
        echo "    $unit: active"
    else
        echo "    $unit: $state" >&2
        failed="$failed $unit"
    fi
done

if [ -n "$failed" ]; then
    echo >&2
    for unit in $failed; do
        echo "--- journalctl -u $unit ---" >&2
        journalctl -u "$unit" -n 15 --no-pager >&2 || true
    done
    echo >&2
    echo "Installation finished but$failed did not start. Nothing above is reachable yet." >&2
    exit 1
fi

echo
echo "Done. Check the status with:"
echo "  systemctl status netcfgd netcfg-web"
echo
echo "Reach the interface from the LAN at:"
ip -4 -o addr show scope global 2>/dev/null | awk '{split($4,a,"/"); print "  https://" a[1] ":8090/"}'
echo
echo "If a firewall is active, open port 8090/tcp, for example:"
echo "  ufw allow from 192.168.0.0/16 to any port 8090 proto tcp"
