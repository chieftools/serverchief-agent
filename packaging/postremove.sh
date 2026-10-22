#!/bin/sh
# Shared by the Debian postrm ($1 = remove | purge | upgrade | …) and the
# RPM %postun ($1 = number of versions left after the transaction).
set -eu

if [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
fi

# A Debian purge drops the identity and state as well. RPM has no purge;
# operators remove /etc/serverchief and /var/lib/serverchief by hand.
if [ "${1:-}" = purge ]; then
    rm -rf /etc/serverchief /var/lib/serverchief /var/log/serverchief
fi
