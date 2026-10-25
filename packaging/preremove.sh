#!/bin/sh
# Shared by the Debian prerm ($1 = remove | upgrade | deconfigure) and the
# RPM %preun ($1 = number of versions left after the transaction).
set -eu

case "${1:-}" in
    remove|0)
        ;;
    *)
        exit 0
        ;;
esac

if [ -d /run/systemd/system ]; then
    systemctl stop serverchief.service serverchief-helper.path || true
    systemctl disable serverchief.service serverchief-helper.path >/dev/null 2>&1 || true
else
    if [ -x /etc/init.d/serverchief ]; then
        /etc/init.d/serverchief stop || true
    fi
    if command -v update-rc.d >/dev/null 2>&1; then
        update-rc.d -f serverchief remove >/dev/null 2>&1 || true
    elif command -v chkconfig >/dev/null 2>&1; then
        chkconfig --del serverchief >/dev/null 2>&1 || true
    fi
fi
