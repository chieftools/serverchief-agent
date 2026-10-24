#!/bin/bash
# Build the Debian package: bin/build-deb.sh VERSION [amd64|arm64]
#
# The package ships both a systemd unit and a SysV script and depends on
# neither init system; the maintainer scripts pick one at runtime.

set -euo pipefail

PACKAGE_NAME="serverchief"
VERSION="${1:-0.0.0}"
ARCH="${2:-amd64}"
MAINTAINER="Server Chief Team <hello@chief.app>"
DESCRIPTION="Server Chief Linux inventory agent."

cd "$(dirname "${BASH_SOURCE[0]}")/.."

GOTOOLCHAIN="go$(awk '/^go / { print $2 }' go.mod)"
export GOTOOLCHAIN
export SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-0}"

if [[ ! "${VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "Version must use numeric X.Y.Z format." >&2
    exit 1
fi

case "${ARCH}" in
    amd64|arm64)
        GOARCH="${ARCH}"
        ;;
    *)
        echo "Unsupported architecture: ${ARCH}" >&2
        exit 1
        ;;
esac

echo "Building Server Chief agent Debian package v${VERSION} (${ARCH})"

BUILD_DIR="$(mktemp -d)"
DEB_DIR="${BUILD_DIR}/${PACKAGE_NAME}_${VERSION}_${ARCH}"
trap 'rm -rf "${BUILD_DIR}"' EXIT

mkdir -p "${DEB_DIR}/DEBIAN" \
    "${DEB_DIR}/usr/bin" \
    "${DEB_DIR}/etc/init.d" \
    "${DEB_DIR}/lib/systemd/system" \
    "${DEB_DIR}/usr/share/doc/serverchief"

# SKIP_BUILD=1 packages an existing serverchief-linux-${GOARCH} binary,
# for example one built once and packaged on another machine.
if [ "${SKIP_BUILD:-0}" != 1 ]; then
    ./bin/build.sh "${VERSION}" "linux-${GOARCH}"
fi

install -m 0755 "serverchief-linux-${GOARCH}" "${DEB_DIR}/usr/bin/serverchief"
install -m 0755 packaging/serverchief.init "${DEB_DIR}/etc/init.d/serverchief"
# /lib/systemd/system rather than /usr/lib: Ubuntu 14.04-era systemd and
# usrmerge-less Debian releases only read the former.
install -m 0644 packaging/serverchief.service "${DEB_DIR}/lib/systemd/system/serverchief.service"
install -m 0644 README.md "${DEB_DIR}/usr/share/doc/serverchief/README.md"
install -m 0644 LICENSE "${DEB_DIR}/usr/share/doc/serverchief/copyright"

cat > "${DEB_DIR}/DEBIAN/control" << EOF2
Package: ${PACKAGE_NAME}
Version: ${VERSION}
Section: admin
Priority: optional
Architecture: ${ARCH}
Maintainer: ${MAINTAINER}
Homepage: https://server.chief.app
Recommends: ca-certificates
Description: ${DESCRIPTION}
 Inventories the operating system, hardware, network, packages, websites and
 certificates of this server and reports changes to Server Chief. Supports
 systemd and SysV init.
EOF2

cat > "${DEB_DIR}/DEBIAN/conffiles" << EOF2
/etc/init.d/serverchief
EOF2

install -m 0755 packaging/postinstall.sh "${DEB_DIR}/DEBIAN/postinst"
install -m 0755 packaging/preremove.sh "${DEB_DIR}/DEBIAN/prerm"
install -m 0755 packaging/postremove.sh "${DEB_DIR}/DEBIAN/postrm"

PACKAGE_FILE="${PACKAGE_NAME}_${VERSION}_${ARCH}.deb"
# gzip keeps the package installable by Ubuntu 14.04's dpkg (no zstd).
dpkg-deb --build --root-owner-group -Zgzip "${DEB_DIR}" "${PACKAGE_FILE}"

echo "Package built successfully: ${PACKAGE_FILE}"
dpkg-deb --info "${PACKAGE_FILE}"
