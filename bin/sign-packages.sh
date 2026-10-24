#!/bin/bash
# Sign the release artifacts built by bin/build-all.sh:
#   - the RPM packages (rpmsign), in place
#   - SHA256SUMS over the tarballs and packages, regenerated after signing
#     the RPMs, with a detached signature in SHA256SUMS.asc
#
#   bin/sign-packages.sh VERSION [RPM_RELEASE]
#
# Run it in the directory holding the artifacts. GPG_KEY_ID is the signing
# key's fingerprint; GPG_PASSPHRASE_FILE, when set, holds its passphrase.

set -euo pipefail

VERSION="${1:-}"
RELEASE="${2:-1}"

if [[ ! "${VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "Version must use numeric X.Y.Z format." >&2
    exit 1
fi

if [ -z "${GPG_KEY_ID:-}" ]; then
    echo "GPG_KEY_ID is required." >&2
    exit 1
fi

GPG_ARGS=(--batch --yes --local-user "${GPG_KEY_ID}")
RPM_GPG_EXTRA="--batch"
if [ -n "${GPG_PASSPHRASE_FILE:-}" ]; then
    GPG_ARGS+=(--pinentry-mode loopback --passphrase-file="${GPG_PASSPHRASE_FILE}")
    RPM_GPG_EXTRA="--batch --pinentry-mode loopback --passphrase-file ${GPG_PASSPHRASE_FILE}"
fi

ARTIFACTS=()
for ARCH in amd64 arm64; do
    ARTIFACTS+=("serverchief_${VERSION}_linux_${ARCH}.tar.gz" "serverchief_${VERSION}_${ARCH}.deb")
done

for RPM_ARCH in x86_64 aarch64; do
    RPM_FILE="serverchief-${VERSION}-${RELEASE}.${RPM_ARCH}.rpm"
    rpmsign --addsign --define "_gpg_name ${GPG_KEY_ID}" \
        --define "__gpg $(command -v gpg)" \
        --define '_gpg_digest_algo sha256' \
        --define "_gpg_sign_cmd_extra_args ${RPM_GPG_EXTRA}" \
        "${RPM_FILE}"

    SIGNATURE="$(rpm -qp --qf '%{RSAHEADER:pgpsig}' "${RPM_FILE}")"
    if [ -z "${SIGNATURE}" ] || [ "${SIGNATURE}" = "(none)" ]; then
        echo "${RPM_FILE} is not signed." >&2
        exit 1
    fi
    echo "${RPM_FILE}: ${SIGNATURE}"
    ARTIFACTS+=("${RPM_FILE}")
done

sha256sum "${ARTIFACTS[@]}" > SHA256SUMS
gpg "${GPG_ARGS[@]}" --detach-sign --armor -o SHA256SUMS.asc SHA256SUMS
gpg --batch --verify SHA256SUMS.asc SHA256SUMS
sha256sum -c SHA256SUMS
