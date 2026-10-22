# Server Chief Agent

The Server Chief Agent is a small, static Go binary that inventories a Linux server (operating system, hardware, network and DNS resolvers) and reports what changed to [Server Chief](https://server.chief.app/). Each dataset is hashed on its canonical JSON form, so the agent only uploads a dataset when its contents actually change.

The agent only makes outbound connections. It never listens on a port.

> [!CAUTION]
> This agent is in early development. Enrollment and delivery to the Server Chief backend are not implemented yet: `serverchief run` collects on schedule and logs what it would send. `serverchief collect` already works as a local inspection tool.

## Supported systems

The agent is a single static binary (`CGO_ENABLED=0`, `netgo`, `osusergo`), so it does not depend on the host's glibc. Its only floor is the Linux 3.2 kernel that current Go requires.

| Distribution | Versions | Init system |
| --- | --- | --- |
| Ubuntu | 14.04, 16.04 and later | Upstart (14.04) via SysV scripts, systemd |
| Debian | 8 and later | systemd or SysV |
| CentOS / RHEL | 7 | systemd, or SysV in containers |
| AlmaLinux, Rocky Linux, CloudLinux | 8 and later | systemd |

Architectures: `amd64` (x86_64) and `arm64` (aarch64).

CentOS 6 and other 2.6.x kernels are not supported.

## Usage

### Inspect what the agent reports

`serverchief collect` runs the collectors once and prints the envelope the agent would send, including each dataset's hash. It needs neither setup nor root, though some sources (for example `/proc/1/environ`) are only readable by root.

```bash
serverchief collect --pretty
serverchief collect --dataset system --dataset network
serverchief collect --root /mnt/image --dataset system   # read an offline tree
```

Trimmed output from `debian:12` on arm64:

```json
{
  "seq": 0,
  "datasets": {
    "system": {
      "hash": "cb787b9ecdb1f079ff8ee08b38c1c39b544435f3eb621b102545e0f629a660b4",
      "mode": "full",
      "payload": {
        "agent_arch": "arm64",
        "arch": "arm64",
        "boot_time": "2026-10-18T05:37:22Z",
        "init": "sysv",
        "kernel": { "machine": "aarch64", "name": "Linux", "release": "7.2.9-…" },
        "os": {
          "codename": "bookworm",
          "family": "debian",
          "id": "debian",
          "pretty_name": "Debian GNU/Linux 12 (bookworm)",
          "version": "12",
          "version_full": "12.15"
        },
        "timezone": "Etc/UTC",
        "virtualization": { "role": "container", "technology": "docker" }
      }
    },
    "resolvers": {
      "hash": "05956c5cc21442782a8fca0d1a7ae88e07bbd5e4154b77562153119da46cfbaf",
      "mode": "full",
      "payload": { "managed_by": "docker", "nameservers": ["0.250.250.200"], "options": [], "search": [], "systemd_resolved": null }
    }
  }
}
```

### Run the daemon

```bash
sudo serverchief run                                          # systemd: logs to the journal
sudo serverchief run --log-file /var/log/serverchief/agent.log  # SysV: size-capped file log
serverchief run --config ./config.json --state-dir ./state      # unprivileged development run
```

`SIGINT` and `SIGTERM` stop the daemon gracefully. `SIGHUP` (`systemctl reload serverchief`) reloads the configuration, reopens the log file and collects every dataset again.

Every key in `/etc/serverchief/config.json` is optional:

```json
{
  "intervals": { "system": "5m", "hardware": "15m" },
  "heartbeat_interval": "5m",
  "datasets": ["system", "hardware", "network", "resolvers"]
}
```

### Set up

```bash
sudo serverchief setup <token>
```

This command currently checks for root, a supported platform and a well-formed `scenr_…` token, then reports that it is not implemented yet. Until the backend exists, start the service by hand: run `systemctl enable --now serverchief`, or on SysV hosts run `update-rc.d serverchief defaults` or `chkconfig --add serverchief`, followed by `service serverchief start`.

## Datasets

| Dataset | Contents | Sources |
| --- | --- | --- |
| `system` | hostname, FQDN, OS family, ID, version, point release and codename, kernel, architecture, virtualization, boot time and ID, timezone, init system | `/etc/os-release` with `/etc/redhat-release`, `/etc/lsb-release` and `/etc/debian_version` fallbacks; `uname(2)`; `/etc/hosts` then DNS for the FQDN; DMI, `/proc/1/environ`, `/proc/1/cgroup`, `/proc/vz`, `/proc/xen`, kvm-clock and the CPU `hypervisor` flag for virtualization (no `systemd-detect-virt` needed) |
| `hardware` | CPU vendor, model, sockets, cores, threads and flags; RAM and swap totals; block devices with partitions; mounted storage file systems with size and inode totals | `/proc/cpuinfo` (x86 and ARM layouts, ARM implementer/part names), sysfs CPU topology, `/proc/meminfo`, `/sys/block`, `/proc/mounts` + `statfs(2)` |
| `network` | interfaces (kind, MAC, MTU, state, virtual flag), addresses with scope, default IPv4 and IPv6 routes | `/sys/class/net`, `net.Interfaces()`, `/proc/net/route`, `/proc/net/ipv6_route`, `/proc/net/if_inet6` |
| `resolvers` | nameservers, search domains and options in file order, the managing tool, systemd-resolved upstreams behind the 127.0.0.53 stub | `/etc/resolv.conf`, `/run/systemd/resolve/resolv.conf` |

Address scopes are `public`, `private` (RFC 1918 and IPv6 ULA `fc00::/7`), `shared` (carrier-grade NAT `100.64.0.0/10`), `loopback`, `link-local`, `multicast`, `unspecified` and `reserved` (documentation, benchmarking and other special-purpose ranges).

Payloads hold only values that are stable while the server is unchanged: capacities rather than usage, and no uptime. Ephemeral interfaces (`veth*`, `cali*`, `tap*`, …), IPv6 privacy addresses, loop and zram devices, and pseudo file systems are left out. Without those exclusions the hashes would change on every run.

### Planned collectors

- [ ] `packages`: dpkg status database and `rpm -qa --queryformat`, sent as deltas (`inventory.Diff`), triggered by package database mtime changes.
- [ ] `updates`: `apt list --upgradable` with origin and pocket, `yum`/`dnf updateinfo`, `/var/run/reboot-required` and `needs-restarting`, using the existing metadata cache by default.
- [ ] `software`: normalized products (PHP, MySQL/MariaDB, nginx, Apache, OpenSSL, Node.js, …) from packages and panel builds (`/opt/plesk/php/*`, DirectAdmin custombuild, `ea-php*`, `/opt/php-*`).
- [ ] `vhosts`: Apache (`apachectl -S`), nginx (`nginx -T`), Plesk and DirectAdmin, with docroot, PHP handler and certificate expiry.
- [ ] `location`: cloud metadata endpoints (Hetzner, AWS, DigitalOcean, GCP, Azure, OVH) and a manual override.

## Architecture

```
main.go          signal handling, embedded fallback CA roots
cmd/             cobra CLI: version, collect, run, setup
collector/       Collector interface, registry and the built-in collectors
inventory/       canonical JSON, SHA-256 hashes, envelope, keyed-list deltas, state store
agent/           daemon loop, config, client interface, enrollment types, HTTP client
initsys/         init system detection plus systemd and SysV service managers
logging/         stdlib log setup and a size-capped rotating file writer
systemdnotify/   sd_notify(3) for Type=notify
pusher/          Pusher-protocol websocket client for realtime wake-ups (not wired up yet)
packaging/       systemd unit, LSB init script, shared deb/rpm maintainer scripts
bin/             build scripts
```

- **Host abstraction:** collectors read the host only through `collector.Host`, which holds an `fs.FS` rooted at `/` plus function fields for live-only sources (`uname`, `statfs`, `net.Interfaces`, DNS). Tests point it at fixtures under `collector/testdata/`. Some of those fixtures were copied verbatim from the official Docker images.
- **Hashing:** `inventory.Canonical` encodes payloads with sorted object keys and no insignificant whitespace. Payloads implement `inventory.Normalizer` to sort lists whose order carries no meaning. The dataset hash is the SHA-256 of that encoding.
- **Envelope:** each upload is `{seq, datasets: {name: {hash, mode: full|delta, base_hash, payload}}, errors}`. `seq` is persisted before use and increases forever. `errors` reports collectors that failed in that round.
- **Delivery:** the daemon schedules each dataset on its own interval with ±10% jitter and uploads only the datasets whose hash differs from the last delivered one. `/var/lib/serverchief/state.json` records those hashes and is replaced atomically (temporary file, fsync, rename). A failed delivery leaves the state untouched and retries with exponential backoff, capped at 5 minutes. A `409 resync` answer makes the agent forget that dataset's hash so the next upload sends it in full.
- **Init systems:** the package ships both `serverchief.service` and `/etc/init.d/serverchief` and depends on neither init system. The maintainer scripts check for `/run/systemd/system` at runtime. The SysV script uses `start-stop-daemon` when available and otherwise falls back to `setsid` with a pid file (RHEL-likes).
- **TLS:** old distributions ship stale CA bundles. The agent's HTTP client therefore trusts the system roots plus the embedded Mozilla bundle from `golang.org/x/crypto/x509roots/fallback`.

The agent ↔ backend protocol (enrollment with generation fencing, heartbeat, inventory, commands and realtime wake-ups) follows the Backup Chief agent. Its types are in `agent/types.go`. The HTTP client is a TODO until the backend's `/agent/v1` endpoints exist.

## Development

Requires Go 1.27.

```bash
go test ./...                       # unit tests (fixtures, no root needed)
go vet ./...
gofmt -l .

./bin/build.sh dev native           # ./serverchief for this machine
./bin/build.sh 0.1.0 all            # plus static linux-amd64 and linux-arm64 binaries
./bin/build-all.sh 0.1.0            # binaries, tarballs, .deb/.rpm when the tools exist, SHA256SUMS

# Try the static binary on old distributions
docker run --rm -v "$PWD/serverchief-linux-arm64:/usr/bin/serverchief:ro" centos:7 serverchief collect --pretty
docker run --rm -v "$PWD/serverchief-linux-arm64:/usr/bin/serverchief:ro" ubuntu:16.04 serverchief collect --pretty
```

`bin/build-deb.sh` and `bin/build-rpm.sh` also accept `SKIP_BUILD=1` to package a binary built earlier.

## License

The license is still to be decided. It will likely be Apache-2.0, like the sibling Chief Tools agents.
