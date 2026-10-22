#!/bin/bash
# Build every release artifact for VERSION:
#   - static binaries for linux/amd64 and linux/arm64
#   - tarballs plus SHA256SUMS for manual installs
#   - .deb and .rpm packages when dpkg-deb / rpmbuild are available
#
#   bin/build-all.sh VERSION [RPM_RELEASE]

set -euo pipefail

VERSION="${1:-0.0.0}"
RELEASE="${2:-1}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${SCRIPT_DIR}/.."

if [[ ! "${VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "Version must use numeric X.Y.Z format." >&2
    exit 1
fi

echo "Building Server Chief agent artifacts v${VERSION}"

"${SCRIPT_DIR}/build.sh" "${VERSION}" linux-amd64
"${SCRIPT_DIR}/build.sh" "${VERSION}" linux-arm64

export SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-0}"
STAGING="$(mktemp -d)"
trap 'rm -rf "${STAGING}"' EXIT

ARTIFACTS=()
for ARCH in amd64 arm64; do
    NAME="serverchief_${VERSION}_linux_${ARCH}"
    mkdir -p "${STAGING}/${NAME}"
    install -m 0755 "serverchief-linux-${ARCH}" "${STAGING}/${NAME}/serverchief"
    install -m 0644 packaging/serverchief.service "${STAGING}/${NAME}/serverchief.service"
    install -m 0755 packaging/serverchief.init "${STAGING}/${NAME}/serverchief.init"
    install -m 0644 README.md "${STAGING}/${NAME}/README.md"
    # Reproducible tarballs: fixed owner, order and timestamps.
    tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="@${SOURCE_DATE_EPOCH}" \
        -C "${STAGING}" -czf "${NAME}.tar.gz" "${NAME}" 2>/dev/null \
        || tar -C "${STAGING}" -czf "${NAME}.tar.gz" "${NAME}"
    ARTIFACTS+=("${NAME}.tar.gz")
done

if command -v dpkg-deb >/dev/null 2>&1; then
    "${SCRIPT_DIR}/build-deb.sh" "${VERSION}" amd64
    "${SCRIPT_DIR}/build-deb.sh" "${VERSION}" arm64
    ARTIFACTS+=("serverchief_${VERSION}_amd64.deb" "serverchief_${VERSION}_arm64.deb")
else
    echo "dpkg-deb not found; skipping Debian packages."
fi

if command -v rpmbuild >/dev/null 2>&1; then
    "${SCRIPT_DIR}/build-rpm.sh" "${VERSION}" "${RELEASE}" x86_64
    "${SCRIPT_DIR}/build-rpm.sh" "${VERSION}" "${RELEASE}" aarch64
    ARTIFACTS+=("serverchief-${VERSION}-${RELEASE}.x86_64.rpm" "serverchief-${VERSION}-${RELEASE}.aarch64.rpm")
else
    echo "rpmbuild not found; skipping RPM packages."
fi

if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${ARTIFACTS[@]}" > SHA256SUMS
else
    shasum -a 256 "${ARTIFACTS[@]}" > SHA256SUMS
fi

echo "All artifacts built successfully."
ls -lh "${ARTIFACTS[@]}" SHA256SUMS
