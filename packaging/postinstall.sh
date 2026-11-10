#!/bin/sh
# Shared by the Debian postinst ($1 = configure, $2 = previous version on
# upgrade) and the RPM %post ($1 = number of installed versions).
set -eu

upgrade=false
case "${1:-}" in
    configure)
        if [ -n "${2:-}" ]; then
            upgrade=true
        fi
        ;;
    [2-9])
        upgrade=true
        ;;
esac

# The daemon runs as the serverchief system user with CAP_DAC_READ_SEARCH;
# serverchief-helper.service runs the few actions that need root.
if ! getent group serverchief >/dev/null; then
    groupadd --system serverchief
fi
if ! getent passwd serverchief >/dev/null; then
    nologin="$(command -v nologin || echo /bin/false)"
    useradd --system --gid serverchief --home-dir /var/lib/serverchief \
        --no-create-home --shell "$nologin" --comment 'Server Chief agent' serverchief
fi

# Where /proc is mounted with hidepid (CloudLinux, hardened hosts) only the
# group its gid= option names sees other users' processes; needs-restarting
# reads the boot time from /proc/1. Their environment and memory stay
# private. The upgrade restart below picks up the group.
proc_gid="$(awk '$2 == "/proc" && $3 == "proc" && $4 ~ /(^|,)hidepid=/ {
    n = split($4, options, ",")
    for (i = 1; i <= n; i++) {
        if (options[i] ~ /^gid=[0-9]+$/) {
            print substr(options[i], 5)
        }
    }
    exit
}' /proc/mounts 2>/dev/null || true)"
if [ -n "$proc_gid" ] && [ "$proc_gid" != 0 ]; then
    proc_group="$(getent group "$proc_gid" | cut -d: -f1 || true)"
    if [ -n "$proc_group" ] && ! id -nG serverchief | tr ' ' '\n' | grep -qx "$proc_group"; then
        usermod -a -G "$proc_group" serverchief || true
    fi
fi

state=/var/lib/serverchief
override=/etc/systemd/system/serverchief.service
template=/usr/share/serverchief/serverchief-root.service

# An agent update from a version that ran as root is driven by that
# version's update helper, which only trusts files root owns. The new
# daemon then runs as root once more and hands the state directory to the
# serverchief user itself when the update is done (a transitional override).
transitional=false
if [ -e "$state/update-request.json" ] && [ "$(stat -c %u "$state" 2>/dev/null || echo 0)" = 0 ]; then
    transitional=true
fi

# The daemon only runs unprivileged under systemd 229 or later, which can
# grant it CAP_DAC_READ_SEARCH. Older systemd and SysV init run it as root.
systemd_version=0
if [ -d /run/systemd/system ]; then
    systemd_version="$(systemctl --version 2>/dev/null | awk 'NR == 1 { print $2 }')"
    case "$systemd_version" in
        ''|*[!0-9]*) systemd_version=0 ;;
    esac
fi
root_daemon=true
if [ "$systemd_version" -ge 229 ]; then
    root_daemon=false
fi

install -d -m 0750 -o root -g serverchief /etc/serverchief
for file in identity.json config.json; do
    if [ -f "/etc/serverchief/$file" ]; then
        chgrp serverchief "/etc/serverchief/$file"
        chmod 0640 "/etc/serverchief/$file"
    fi
done
install -d -m 0700 "$state"
install -d -m 0750 /var/log/serverchief
if [ "$root_daemon" = true ]; then
    # A root daemon checks its files against root and the update helpers
    # against the owner of the state directory, so it stays root's. A
    # request the serverchief user left behind never reached a helper: drop
    # it instead of handing it to root, which would run it on start.
    find "$state" -maxdepth 1 -type f -name '*-request.json' ! -uid 0 -exec rm -f {} +
    chown -R root:root "$state" /var/log/serverchief
elif [ "$transitional" = false ]; then
    chown -R serverchief:serverchief "$state" /var/log/serverchief
fi
install -d -m 0700 -o serverchief -g serverchief "$state/helper"

# Pick the init system at runtime; the package does not depend on systemd.
# Enabling the service is left to `serverchief setup`; an upgrade restarts
# an agent that is already running.
if [ -d /run/systemd/system ]; then
    if [ "$root_daemon" = true ]; then
        # No ambient capabilities: run the daemon as root, as before.
        install -m 0644 "$template" "$override"
    elif [ "$transitional" = true ]; then
        sed 's/^# serverchief-root-override: permanent$/# serverchief-root-override: transitional/' "$template" > "$override"
        chmod 0644 "$override"
    elif [ -f "$override" ] && grep -q '^# serverchief-root-override:' "$override"; then
        rm -f "$override"
    fi
    systemctl daemon-reload || true
    if [ "$root_daemon" = false ]; then
        systemctl enable serverchief-helper.path >/dev/null 2>&1 || true
        systemctl start serverchief-helper.path || true
    fi
    if [ "$upgrade" = true ]; then
        systemctl try-restart serverchief.service || true
    fi
elif [ -x /etc/init.d/serverchief ]; then
    if [ "$upgrade" = true ]; then
        /etc/init.d/serverchief try-restart || true
    fi
fi

if [ "$upgrade" = false ]; then
    echo 'Server Chief agent installed. Run: sudo serverchief setup <token>'
    echo 'Preview what it reports with: serverchief collect --pretty'
fi
