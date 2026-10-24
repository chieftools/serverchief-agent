#!/bin/bash
# Build static serverchief binaries.
#
#   bin/build.sh [VERSION] [all|native|linux-amd64|linux-arm64]
#
# Linux builds are fully static (CGO_ENABLED=0 with the pure-Go resolver and
# user lookup), so one binary runs on every supported distribution
# regardless of its glibc.

set -euo pipefail

VERSION="${1:-dev}"
TARGET="${2:-all}"
case "${TARGET}" in
    all|native|linux-amd64|linux-arm64)
        ;;
    *)
        echo "Unsupported build target: ${TARGET}" >&2
        exit 1
        ;;
esac

cd "$(dirname "${BASH_SOURCE[0]}")/.."

GOTOOLCHAIN="go$(awk '/^go / { print $2 }' go.mod)"
export GOTOOLCHAIN
export CGO_ENABLED=0

# TEST_BUILD_TAGS adds test-only hooks for end-to-end runs in throwaway
# containers (see updater/testbuild_*.go), e.g.
# TEST_BUILD_TAGS=serverchief_e2e_never_ready. Never set it for a release.
TAGS="netgo,osusergo"
if [ -n "${TEST_BUILD_TAGS:-}" ]; then
    if [[ ! "${TEST_BUILD_TAGS}" =~ ^serverchief_e2e_[a-z_]+(,serverchief_e2e_[a-z_]+)*$ ]]; then
        echo "TEST_BUILD_TAGS only takes serverchief_e2e_* tags." >&2
        exit 1
    fi
    TAGS="${TAGS},${TEST_BUILD_TAGS}"
    echo "Test build with ${TEST_BUILD_TAGS}: not for release." >&2
fi
BUILD_FLAGS=(-mod=readonly -trimpath -buildvcs=false "-tags=${TAGS}")
LDFLAGS="-s -w -buildid= -X main.version=${VERSION}"

build() {
    local goos="$1" goarch="$2" output="$3"
    echo "Building Server Chief agent ${VERSION} for ${goos}/${goarch}..."
    GOOS="${goos}" GOARCH="${goarch}" go build "${BUILD_FLAGS[@]}" -ldflags="${LDFLAGS}" -o "${output}" .
}

if [ "${TARGET}" = all ] || [ "${TARGET}" = native ]; then
    build "$(go env GOOS)" "$(go env GOARCH)" serverchief
fi

if [ "${TARGET}" = all ] || [ "${TARGET}" = linux-amd64 ]; then
    build linux amd64 serverchief-linux-amd64
fi

if [ "${TARGET}" = all ] || [ "${TARGET}" = linux-arm64 ]; then
    build linux arm64 serverchief-linux-arm64
fi
