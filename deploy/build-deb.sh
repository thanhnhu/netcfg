#!/bin/sh
# Package binaries that are already built into a single .deb.
#
# Usage: deploy/build-deb.sh [goarch]        goarch: amd64 | arm64 | arm
#
# Environment:
#   BIN_DIR     where netcfgd and netcfg-web live       (default: dist)
#   OUT_DIR     where the .deb is written               (default: dist)
#   VERSION     package version                         (default: git describe)
#   MAINTAINER  RFC 822 name and address for the control file
set -eu

SRC_DIR="$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)"
ROOT_DIR="$(CDPATH='' cd -- "$SRC_DIR/.." && pwd)"

GOARCH="${1:-${GOARCH:-amd64}}"
BIN_DIR="${BIN_DIR:-$ROOT_DIR/dist}"
OUT_DIR="${OUT_DIR:-$ROOT_DIR/dist}"
MAINTAINER="${MAINTAINER:-netcfg maintainers <netcfg@users.noreply.github.com>}"

if ! command -v dpkg-deb >/dev/null 2>&1; then
    echo "dpkg-deb not found. Build the package on a Debian host, in WSL, or in CI." >&2
    exit 1
fi

case "$GOARCH" in
    amd64)            DEB_ARCH=amd64 ;;
    arm64)            DEB_ARCH=arm64 ;;
    arm|armv7|armhf)  DEB_ARCH=armhf ;;
    386)              DEB_ARCH=i386 ;;
    *) echo "unsupported architecture: $GOARCH" >&2; exit 1 ;;
esac

for binary in netcfgd netcfg-web; do
    if [ ! -f "$BIN_DIR/$binary" ]; then
        echo "$BIN_DIR/$binary not found. Run 'make linux GOARCH=$GOARCH' first." >&2
        exit 1
    fi
done

# last_release is the newest v* tag. Without one there is nothing to anchor to,
# and 0.0.0 keeps the package installable while saying so.
last_release() {
    tag="$(git -C "$ROOT_DIR" describe --tags --abbrev=0 --match 'v*' 2>/dev/null || true)"
    tag="${tag#v}"
    printf '%s' "${tag:-0.0.0}"
}

version="${VERSION:-}"
if [ -z "$version" ]; then
    version="$(git -C "$ROOT_DIR" describe --tags --always --dirty 2>/dev/null || echo 0.0.0)"
fi
version="${version#v}"
# dpkg refuses a version that does not start with a digit. A named build such as
# "latest" is therefore anchored to the release it follows, so it still sorts
# after that release and before the next one.
case "$version" in
    [0-9]*) ;;
    *) version="$(last_release)+$version" ;;
esac
# A "-" would introduce a Debian revision, and git describe uses it for the
# commit count, so both it and anything else illegal is folded away.
version="$(printf '%s' "$version" | sed -e 's/-/+/g' -e 's/[^A-Za-z0-9.+~]/./g')"

stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT INT TERM
# mktemp hands back a 0700 directory, which would become the mode of "/" in the
# package and lock every non-root user out of the filesystem root on install.
chmod 0755 "$stage"

install -d -m 0755 "$stage/DEBIAN" "$stage/usr/bin" "$stage/lib/systemd/system" \
    "$stage/usr/share/doc/netcfg/examples"

install -m 0755 "$BIN_DIR/netcfgd"    "$stage/usr/bin/netcfgd"
install -m 0755 "$BIN_DIR/netcfg-web" "$stage/usr/bin/netcfg-web"

# The units name /usr/local/bin because that is where install.sh puts the
# binaries; a package is not allowed to touch /usr/local.
for unit in netcfgd netcfg-web; do
    sed 's#/usr/local/bin/#/usr/bin/#g' "$SRC_DIR/$unit.service" \
        > "$stage/lib/systemd/system/$unit.service"
    chmod 0644 "$stage/lib/systemd/system/$unit.service"
done

install -m 0644 "$ROOT_DIR/README.md" "$ROOT_DIR/README.en.md" "$stage/usr/share/doc/netcfg/"
install -m 0644 "$SRC_DIR/wpa_supplicant-wlan0.conf.example" \
    "$SRC_DIR/avahi-netcfg-web.service.xml" "$stage/usr/share/doc/netcfg/examples/"

for script in config postinst prerm postrm; do
    # A CRLF checkout on Windows would leave dpkg running "/bin/sh\r".
    sed 's/\r$//' "$SRC_DIR/debian/$script" > "$stage/DEBIAN/$script"
    chmod 0755 "$stage/DEBIAN/$script"
done
sed 's/\r$//' "$SRC_DIR/debian/templates" > "$stage/DEBIAN/templates"
chmod 0644 "$stage/DEBIAN/templates"

(cd "$stage" && find . -path ./DEBIAN -prune -o -type f -print0 |
    xargs -0 md5sum | sed 's|\./||') > "$stage/DEBIAN/md5sums"
chmod 0644 "$stage/DEBIAN/md5sums"

size_kb="$(du -sk "$stage" | cut -f1)"
sed -e "s/@VERSION@/$version/" \
    -e "s/@ARCH@/$DEB_ARCH/" \
    -e "s/@SIZE@/$size_kb/" \
    -e "s|@MAINTAINER@|$MAINTAINER|" \
    -e 's/\r$//' \
    "$SRC_DIR/debian/control.in" > "$stage/DEBIAN/control"
chmod 0644 "$stage/DEBIAN/control"

mkdir -p "$OUT_DIR"
package="$OUT_DIR/netcfg_${version}_${DEB_ARCH}.deb"
# --root-owner-group keeps every file owned by root without needing fakeroot.
dpkg-deb --root-owner-group --build "$stage" "$package" >/dev/null

echo "Built $package"
