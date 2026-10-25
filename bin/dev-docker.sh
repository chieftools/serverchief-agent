#!/bin/bash
# Run development agents in Docker containers against a local Server Chief.
#
#   bin/dev-docker.sh up NAME --token TOKEN [options]   start and enroll an agent
#   bin/dev-docker.sh down NAME|--all                   remove agent container(s)
#   bin/dev-docker.sh logs NAME [docker logs flags]     show an agent's log
#   bin/dev-docker.sh list                              list agent containers
#   bin/dev-docker.sh rebuild                           rebuild the binary and restart all agents
#
# Options for "up":
#   --token TOKEN        enrollment token (scenr_...) from the UI, the API or
#                        `php artisan serverchief:dev-agent` in the app
#   --image IMAGE        distribution image (default ubuntu:24.04), e.g.
#                        ubuntu:16.04, debian:12, centos:7, almalinux:9
#   --platform PLATFORM  docker platform, e.g. linux/amd64 (default: host arch)
#   --endpoint URL       agent endpoint (default https://server.chief.test/agent/v1)
#   --ca-file PATH       CA to trust (default: Laravel Valet's CA)
#   --provision CMD      shell command to run once before enrolling,
#                        e.g. "apt-get install -y nginx php"
#   --no-refresh         don't refresh the package metadata cache on first start
#
# The metrics gateway is reached at metrics.<endpoint host> (a local
# `valet proxy`, see serverchief-metrics/docs/deployment.md).
#
# Containers are named serverchief-dev-NAME and restart until removed. The
# binary is mounted from .dev-agent/ in this repository, so `rebuild` picks up
# code changes without re-enrolling. Enrollment state lives inside the container, so
# `down` followed by `up` needs a fresh token.

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

PREFIX=serverchief-dev-
LABEL=tools.chief.serverchief.dev-agent=1

usage() {
    sed -n '2,27p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
    exit "${1:-1}"
}

docker_arch() {
    local platform="${1:-}"
    if [ -n "${platform}" ]; then
        echo "${platform#linux/}"
        return
    fi
    case "$(docker info --format '{{.Architecture}}')" in
        aarch64|arm64) echo arm64 ;;
        x86_64|amd64) echo amd64 ;;
        *) echo "Unsupported docker architecture" >&2; exit 1 ;;
    esac
}

# Build into .dev-agent/linux-ARCH/ and swap the binary in with a rename.
# Containers mount that directory (not the file), so a rebuild never leaves a
# running or starting container without a binary.
build_binary() {
    local dir=".dev-agent/linux-$1"
    bin/build.sh dev "linux-$1" >/dev/null
    mkdir -p "${dir}"
    cp "serverchief-linux-$1" "${dir}/serverchief.tmp"
    chmod 0755 "${dir}/serverchief.tmp"
    mv -f "${dir}/serverchief.tmp" "${dir}/serverchief"
    echo "Built ${dir}/serverchief"
}

container_command() {
    # POSIX sh so it runs on every supported image, including busybox-less old ones.
    cat <<'SH'
PATH="/opt/serverchief-dev:${PATH}"
if [ ! -f /etc/serverchief/identity.json ]; then
    if [ "${SERVERCHIEF_DEV_REFRESH}" = 1 ]; then
        echo "Refreshing package metadata cache..."
        if command -v apt-get >/dev/null 2>&1; then
            apt-get update -qq >/dev/null 2>&1 || echo "apt-get update failed; continuing"
        elif command -v dnf >/dev/null 2>&1; then
            dnf -q makecache >/dev/null 2>&1 || echo "dnf makecache failed; continuing"
        elif command -v yum >/dev/null 2>&1; then
            yum -q makecache >/dev/null 2>&1 || echo "yum makecache failed (EOL mirrors?); continuing"
        fi
    fi
    if [ -n "${SERVERCHIEF_DEV_PROVISION}" ]; then
        echo "Provisioning: ${SERVERCHIEF_DEV_PROVISION}"
        sh -c "${SERVERCHIEF_DEV_PROVISION}" || echo "Provisioning failed; continuing"
    fi
    serverchief setup "${SERVERCHIEF_DEV_TOKEN}" || exit 1
fi
exec serverchief run
SH
}

cmd_up() {
    local name="${1:-}"
    [ -n "${name}" ] || usage
    shift

    local token="" image="ubuntu:24.04" platform="" provision="" refresh=1
    local endpoint="https://server.chief.test/agent/v1"
    local ca_file="${HOME}/.config/valet/CA/LaravelValetCASelfSigned.pem"
    while [ $# -gt 0 ]; do
        case "$1" in
            --token) token="$2"; shift 2 ;;
            --image) image="$2"; shift 2 ;;
            --platform) platform="$2"; shift 2 ;;
            --endpoint) endpoint="$2"; shift 2 ;;
            --ca-file) ca_file="$2"; shift 2 ;;
            --provision) provision="$2"; shift 2 ;;
            --no-refresh) refresh=0; shift ;;
            *) echo "Unknown option: $1" >&2; usage ;;
        esac
    done
    case "${token}" in
        scenr_*) ;;
        *) echo "--token must be an enrollment token (scenr_...)" >&2; exit 1 ;;
    esac
    [ -f "${ca_file}" ] || { echo "CA file not found: ${ca_file}" >&2; exit 1; }
    if docker container inspect "${PREFIX}${name}" >/dev/null 2>&1; then
        echo "${PREFIX}${name} already exists; remove it first with: bin/dev-docker.sh down ${name}" >&2
        exit 1
    fi

    local arch host
    arch="$(docker_arch "${platform}")"
    build_binary "${arch}"
    host="$(printf '%s' "${endpoint}" | sed -E 's#^[a-z]+://([^/:]+).*#\1#')"

    local platform_args=()
    [ -n "${platform}" ] && platform_args=(--platform "${platform}")

    # The +-expansion keeps an empty array working under `set -u` on macOS bash 3.2.
    docker run -d ${platform_args[@]+"${platform_args[@]}"} \
        --name "${PREFIX}${name}" \
        --hostname "${name}" \
        --label "${LABEL}" \
        --restart unless-stopped \
        --add-host "${host}:host-gateway" \
        --add-host "metrics.${host}:host-gateway" \
        -v "${PWD}/.dev-agent/linux-${arch}:/opt/serverchief-dev:ro" \
        -v "${ca_file}:/etc/serverchief-dev-ca.pem:ro" \
        -e SERVERCHIEF_ENDPOINT="${endpoint}" \
        -e SERVERCHIEF_CA_FILE=/etc/serverchief-dev-ca.pem \
        -e SERVERCHIEF_DEV_TOKEN="${token}" \
        -e SERVERCHIEF_DEV_REFRESH="${refresh}" \
        -e SERVERCHIEF_DEV_PROVISION="${provision}" \
        "${image}" sh -c "$(container_command)" >/dev/null

    echo "Started ${PREFIX}${name} (${image}, ${arch}); follow it with: bin/dev-docker.sh logs ${name} -f"
}

cmd_down() {
    local target="${1:-}"
    [ -n "${target}" ] || usage
    if [ "${target}" = --all ]; then
        local ids
        ids="$(docker ps -aq --filter "label=${LABEL}")"
        [ -n "${ids}" ] && docker rm -f ${ids} >/dev/null
        echo "Removed all development agents"
    else
        docker rm -f "${PREFIX}${target}" >/dev/null
        echo "Removed ${PREFIX}${target}"
    fi
}

cmd_logs() {
    local name="${1:-}"
    [ -n "${name}" ] || usage
    shift
    docker logs "$@" "${PREFIX}${name}"
}

cmd_list() {
    docker ps -a --filter "label=${LABEL}" --format 'table {{.Names}}\t{{.Image}}\t{{.Status}}'
}

cmd_rebuild() {
    local ids arch
    ids="$(docker ps -q --filter "label=${LABEL}")"
    if [ -n "${ids}" ]; then
        for arch in arm64 amd64; do
            # shellcheck disable=SC2086 # ids is a whitespace-separated list
            if docker inspect --format '{{range .Mounts}}{{.Source}}{{"\n"}}{{end}}' ${ids} | grep -q "/.dev-agent/linux-${arch}$"; then
                build_binary "${arch}"
            fi
        done
        docker restart ${ids} >/dev/null
        echo "Restarted $(echo "${ids}" | wc -l | tr -d ' ') development agent(s)"
    else
        echo "No running development agents"
    fi
}

case "${1:-}" in
    up) shift; cmd_up "$@" ;;
    down) shift; cmd_down "$@" ;;
    logs) shift; cmd_logs "$@" ;;
    list) cmd_list ;;
    rebuild) cmd_rebuild ;;
    -h|--help|help) usage 0 ;;
    *) usage ;;
esac
