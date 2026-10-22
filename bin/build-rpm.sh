#!/bin/bash
# Build the RPM package: bin/build-rpm.sh VERSION [RELEASE] [x86_64|aarch64]
#
# Like the Debian package, it ships a systemd unit and a SysV script and
# requires neither; the scriptlets pick one at runtime.

set -euo pipefail

PACKAGE_NAME="serverchief"
VERSION="${1:-0.0.0}"
RELEASE="${2:-1}"
ARCH="${3:-x86_64}"
DESCRIPTION="Server Chief Linux inventory agent."
URL="https://server.chief.app"
MAINTAINER="Server Chief Team <hello@chief.app>"

cd "$(dirname "${BASH_SOURCE[0]}")/.."

GOTOOLCHAIN="go$(awk '/^go / { print $2 }' go.mod)"
export GOTOOLCHAIN
export SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-0}"

if [[ ! "${VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "Version must use numeric X.Y.Z format." >&2
    exit 1
fi

if [[ ! "${RELEASE}" =~ ^[1-9][0-9]*$ ]]; then
    echo "Release must be a positive integer." >&2
    exit 1
fi

case "${ARCH}" in
    x86_64)
        GOARCH="amd64"
        ;;
    aarch64)
        GOARCH="arm64"
        ;;
    *)
        echo "Unsupported architecture: ${ARCH}" >&2
        exit 1
        ;;
esac

echo "Building Server Chief agent RPM package v${VERSION}-${RELEASE} (${ARCH})"

BUILD_DIR="$(mktemp -d)"
RPMBUILD_DIR="${BUILD_DIR}/rpmbuild"
trap 'rm -rf "${BUILD_DIR}"' EXIT
mkdir -p "${RPMBUILD_DIR}"/{BUILD,BUILDROOT,RPMS,SOURCES,SPECS,SRPMS}

# SKIP_BUILD=1 packages an existing serverchief-linux-${GOARCH} binary,
# for example one built once and packaged on another machine.
if [ "${SKIP_BUILD:-0}" != 1 ]; then
    ./bin/build.sh "${VERSION}" "linux-${GOARCH}"
fi

install -m 0755 "serverchief-linux-${GOARCH}" "${RPMBUILD_DIR}/SOURCES/serverchief"
cp packaging/serverchief.service packaging/serverchief.init README.md "${RPMBUILD_DIR}/SOURCES/"

# Scriptlets are inlined: %post -f would also work, but %preun and %postun
# need the shared scripts too.
POSTINSTALL="$(cat packaging/postinstall.sh)"
PREREMOVE="$(cat packaging/preremove.sh)"
POSTREMOVE="$(cat packaging/postremove.sh)"

cat > "${RPMBUILD_DIR}/SPECS/serverchief.spec" << EOF2
Name:           ${PACKAGE_NAME}
Version:        ${VERSION}
Release:        ${RELEASE}
Summary:        ${DESCRIPTION}

# TODO(license): set once the license is chosen (likely Apache-2.0).
License:        TBD
URL:            ${URL}
Packager:       ${MAINTAINER}
Source0:        serverchief.service
Source1:        serverchief.init
Source2:        README.md

%global debug_package %{nil}
%global _build_id_links none
%global __os_install_post %{nil}

AutoReqProv:    no

%description
Collects read-only inventory facts (OS, hardware, network, resolvers) and
reports changes to Server Chief. Supports systemd and SysV init.

%install
mkdir -p %{buildroot}/usr/bin
mkdir -p %{buildroot}/usr/lib/systemd/system
mkdir -p %{buildroot}/etc/rc.d/init.d
mkdir -p %{buildroot}/usr/share/doc/serverchief

install -m 0755 %{_sourcedir}/serverchief %{buildroot}/usr/bin/serverchief
install -m 0644 %{SOURCE0} %{buildroot}/usr/lib/systemd/system/serverchief.service
install -m 0755 %{SOURCE1} %{buildroot}/etc/rc.d/init.d/serverchief
install -m 0644 %{SOURCE2} %{buildroot}/usr/share/doc/serverchief/README.md

%files
%attr(0755, root, root) /usr/bin/serverchief
%attr(0644, root, root) /usr/lib/systemd/system/serverchief.service
%config(noreplace) %attr(0755, root, root) /etc/rc.d/init.d/serverchief
%doc %attr(0644, root, root) /usr/share/doc/serverchief/README.md

%post
${POSTINSTALL}

%preun
${PREREMOVE}

%postun
${POSTREMOVE}
EOF2

rpmbuild --define "_topdir ${RPMBUILD_DIR}" \
    --define "_tmppath ${BUILD_DIR}" \
    --define '_binary_payload w9.gzdio' \
    --define '_buildhost reproducible' \
    --define 'use_source_date_epoch_as_buildtime 1' \
    --define 'clamp_mtime_to_source_date_epoch 1' \
    --target "${ARCH}-linux" \
    -bb "${RPMBUILD_DIR}/SPECS/serverchief.spec"

PACKAGE_FILE="${PACKAGE_NAME}-${VERSION}-${RELEASE}.${ARCH}.rpm"
mv "${RPMBUILD_DIR}/RPMS/${ARCH}/${PACKAGE_FILE}" .

echo "Package built successfully: ${PACKAGE_FILE}"
rpm -qip "${PACKAGE_FILE}"
