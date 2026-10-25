#!/bin/sh
# Shared by the Debian postrm ($1 = remove | purge | upgrade | …) and the
# RPM %postun ($1 = number of versions left after the transaction).
set -eu

case "${1:-}" in
    remove|purge|0)
        # The root override the package installed, never one of the operator's
        override=/etc/systemd/system/serverchief.service
        if [ -f "$override" ] && grep -q '^# serverchief-root-override:' "$override"; then
            rm -f "$override"
        fi
        ;;
esac

if [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
fi

# A Debian purge drops the identity and state as well. RPM has no purge;
# operators remove /etc/serverchief and /var/lib/serverchief by hand.
if [ "${1:-}" = purge ]; then
    rm -rf /etc/serverchief /var/lib/serverchief /var/log/serverchief
    if getent passwd serverchief >/dev/null; then
        userdel serverchief >/dev/null 2>&1 || true
    fi
    if getent group serverchief >/dev/null; then
        groupdel serverchief >/dev/null 2>&1 || true
    fi
fi
