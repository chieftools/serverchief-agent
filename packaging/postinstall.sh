#!/bin/sh
# Shared by the Debian postinst ($1 = configure, $2 = previous version on
# upgrade) and the RPM %post ($1 = number of installed versions).
set -eu

install -d -m 0750 /etc/serverchief
install -d -m 0700 /var/lib/serverchief
install -d -m 0750 /var/log/serverchief

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

# Pick the init system at runtime; the package does not depend on systemd.
# Enabling the service is left to `serverchief setup`; an upgrade restarts
# an agent that is already running.
if [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
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
