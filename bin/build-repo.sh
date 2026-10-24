#!/bin/bash
# Add a release's packages to the apt and yum/dnf repositories in REPO_DIR
# and sign their metadata:
#
#   bin/build-repo.sh VERSION REPO_DIR [RPM_RELEASE]
#
# Run it in the directory holding the packages (RPMs already signed by
# bin/sign-packages.sh). REPO_DIR holds the published repository's metadata
# (deb/dists, rpm/stable/<arch>/repodata) and receives:
#
#   deb/pool/main/s/serverchief/serverchief_VERSION_{amd64,arm64}.deb
#   deb/dists/stable/{Release,InRelease,Release.gpg}
#   deb/dists/stable/main/binary-{amd64,arm64}/Packages{,.gz,.bz2}
#   rpm/stable/{x86_64,aarch64}/serverchief-VERSION-RELEASE.<arch>.rpm
#   rpm/stable/{x86_64,aarch64}/repodata/ with repomd.xml.asc
#   gpg.key
#
# Packages entries of earlier versions are kept, so older versions stay
# installable (the agent's updater needs the running version to roll back).
# The RPM metadata is updated incrementally from the existing repodata,
# unless REBUILD_RPM_REPO=true and REPO_DIR holds every RPM.
#
# GPG_KEY_ID is the signing key's fingerprint; GPG_PASSPHRASE_FILE, when
# set, holds its passphrase.

set -euo pipefail

VERSION="${1:-}"
REPO_ROOT="${2:-}"
RELEASE="${3:-1}"

if [[ ! "${VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "Version must use numeric X.Y.Z format." >&2
    exit 1
fi

if [ -z "${REPO_ROOT}" ] || [ -z "${GPG_KEY_ID:-}" ]; then
    echo "Usage: GPG_KEY_ID=<fingerprint> $0 VERSION REPO_DIR [RPM_RELEASE]" >&2
    exit 1
fi

GPG_ARGS=(--batch --yes --local-user "${GPG_KEY_ID}")
if [ -n "${GPG_PASSPHRASE_FILE:-}" ]; then
    GPG_ARGS+=(--pinentry-mode loopback --passphrase-file="${GPG_PASSPHRASE_FILE}")
fi

PACKAGES_ROOT="$(pwd)"
mkdir -p "${REPO_ROOT}"
REPO_ROOT="$(cd "${REPO_ROOT}" && pwd)"

# DEB repository
POOL_DIR="${REPO_ROOT}/deb/pool/main/s/serverchief"
mkdir -p "${POOL_DIR}"

for DEB_ARCH in amd64 arm64; do
    DEB_FILE="serverchief_${VERSION}_${DEB_ARCH}.deb"
    PACKAGES_DIR="${REPO_ROOT}/deb/dists/stable/main/binary-${DEB_ARCH}"

    # Place new package in pool directory structure
    cp "${PACKAGES_ROOT}/${DEB_FILE}" "${POOL_DIR}/"

    # Generate Packages entries for all .deb files in pool
    NEW_ENTRY=$(cd "${REPO_ROOT}/deb" && apt-ftparchive packages pool/)

    # Prepare Packages file
    mkdir -p "${PACKAGES_DIR}"
    touch "${PACKAGES_DIR}/Packages"

    # Remove any existing entry for this version (in case of re-release)
    if [ -s "${PACKAGES_DIR}/Packages" ]; then
        awk -v RS='\n\n' -v ORS='\n\n' -v ver="serverchief_${VERSION}_${DEB_ARCH}.deb" \
            '$0 !~ ver && NF' "${PACKAGES_DIR}/Packages" > "${PACKAGES_DIR}/Packages.tmp"
        mv "${PACKAGES_DIR}/Packages.tmp" "${PACKAGES_DIR}/Packages"
    fi

    # Append new entry, filtered to only entries matching this architecture
    printf '%s' "${NEW_ENTRY}" | awk -v RS='\n\n' -v ORS='\n\n' -v arch="${DEB_ARCH}" \
        '$0 ~ "Architecture: "arch' >> "${PACKAGES_DIR}/Packages"

    # Generate compressed versions
    gzip -9c "${PACKAGES_DIR}/Packages" > "${PACKAGES_DIR}/Packages.gz"
    bzip2 -9c "${PACKAGES_DIR}/Packages" > "${PACKAGES_DIR}/Packages.bz2"
done

# Generate and sign the Release file
(
    cd "${REPO_ROOT}/deb"
    # Drop the previous Release and its signatures first, so none of them
    # ends up hashed in the new one.
    rm -f dists/stable/Release dists/stable/InRelease dists/stable/Release.gpg
    RELEASE_TMP="$(mktemp)"
    apt-ftparchive \
        -o APT::FTPArchive::Release::Origin="Server Chief" \
        -o APT::FTPArchive::Release::Label="Server Chief" \
        -o APT::FTPArchive::Release::Suite="stable" \
        -o APT::FTPArchive::Release::Codename="stable" \
        -o APT::FTPArchive::Release::Components="main" \
        -o APT::FTPArchive::Release::Architectures="amd64 arm64" \
        release dists/stable/ > "${RELEASE_TMP}"
    install -m 0644 "${RELEASE_TMP}" dists/stable/Release
    rm -f "${RELEASE_TMP}"

    gpg "${GPG_ARGS[@]}" --clearsign -o dists/stable/InRelease dists/stable/Release
    gpg "${GPG_ARGS[@]}" --detach-sign --armor -o dists/stable/Release.gpg dists/stable/Release
    gpg --batch --verify dists/stable/InRelease
    gpg --batch --verify dists/stable/Release.gpg dists/stable/Release
)

# RPM repositories
for RPM_ARCH in x86_64 aarch64; do
    RPM_FILE="serverchief-${VERSION}-${RELEASE}.${RPM_ARCH}.rpm"
    REPO_DIR="${REPO_ROOT}/rpm/stable/${RPM_ARCH}"

    SIGNATURE="$(rpm -qp --qf '%{RSAHEADER:pgpsig}' "${PACKAGES_ROOT}/${RPM_FILE}")"
    if [ -z "${SIGNATURE}" ] || [ "${SIGNATURE}" = "(none)" ]; then
        echo "${RPM_FILE} is not signed; run bin/sign-packages.sh first." >&2
        exit 1
    fi

    # Copy new RPM to repo directory
    mkdir -p "${REPO_DIR}"
    cp "${PACKAGES_ROOT}/${RPM_FILE}" "${REPO_DIR}/"

    if [ "${REBUILD_RPM_REPO:-false}" = "true" ]; then
        # Full rebuild: all RPM files are present, generate fresh metadata
        createrepo_c "${REPO_DIR}"
    else
        # Check if existing repo metadata exists for incremental update
        OLD_PRIMARY=$(find "${REPO_DIR}/repodata" -name "*-primary.xml.gz" 2>/dev/null | head -1)
        if [ -n "${OLD_PRIMARY}" ]; then
            # Create placeholders for old RPMs so createrepo_c --update
            # preserves their metadata without needing the actual files
            gunzip -c "${OLD_PRIMARY}" | grep -oP 'href="\K[^"]+' | while read -r rpm; do
                [ -f "${REPO_DIR}/${rpm}" ] || touch "${REPO_DIR}/${rpm}"
            done
            createrepo_c --update --skip-stat "${REPO_DIR}"
        else
            # No existing metadata (new architecture), create fresh repo
            createrepo_c "${REPO_DIR}"
        fi
    fi

    # Sign the repository metadata
    gpg "${GPG_ARGS[@]}" --detach-sign --armor -o "${REPO_DIR}/repodata/repomd.xml.asc" "${REPO_DIR}/repodata/repomd.xml"
    gpg --batch --verify "${REPO_DIR}/repodata/repomd.xml.asc" "${REPO_DIR}/repodata/repomd.xml"

    # Clean up empty placeholder files
    find "${REPO_DIR}" -maxdepth 1 -name "*.rpm" -empty -delete
done

# Export GPG public key
gpg --batch --armor --export "${GPG_KEY_ID}" > "${REPO_ROOT}/gpg.key"
