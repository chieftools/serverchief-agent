Captured on 2026-10-22 from the official Docker images (OrbStack; centos:7 runs as amd64 under emulation, the others as arm64):

- `*/var/lib/dpkg/status`: `/var/lib/dpkg/status` of `ubuntu:16.04` and `debian:12`, verbatim.
- `ubuntu-16.04/commands/apt-get-dist-upgrade.txt`: `apt-get -s -o Debug::NoLocking=1 dist-upgrade` after `apt-get update`.
- `debian-12/commands/apt-get-dist-upgrade-current.txt`: the same on `debian:12` (no updates pending); `apt-get-dist-upgrade-12.0.txt` on `debian:12.0`, which is 45 updates behind.
- `centos-7/commands/`: `rpm -qa --queryformat` (the agent's format), `yum -C -q check-update` and `yum -C -q updateinfo list security` (empty: CentOS ships no updateinfo) after pointing the repos at vault.centos.org and `yum makecache`, `needs-restarting -r` from yum-utils.
- `almalinux-9/commands/`: `rpm -qa --queryformat`, `dnf -C -q check-update` (with and without a metadata cache), `dnf -C -q updateinfo list --security`, and `needs-restarting -r` after updating glibc inside the running container.
- `almalinux-9/commands/repolist.txt` and `repoquery-from-repo.txt` (captured 2026-10-24): `dnf -C -q --setopt=exit_on_lock=True repolist --all` and `repoquery --installed --qf '%{name}\t%{arch}\t%{from_repo}\n'` on a fresh `almalinux:9` without a metadata cache.
- `centos-7/commands/repolist.txt` and `list-installed.txt` (captured 2026-10-24, amd64): `yum -C -q --setopt=exit_on_lock=True repolist all` and `list installed` on a fresh `centos:7` (its packages come from the image build's `@CentOS`/`@Updates`, which are not configured repositories).
