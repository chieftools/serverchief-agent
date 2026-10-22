# Server Chief Agent

The Server Chief Agent is a small, static Go binary that inventories a Linux server (operating system, hardware, network and DNS resolvers) and reports what changed to [Server Chief](https://server.chief.app/). Each dataset is hashed on its canonical JSON form, so the agent only uploads a dataset when its contents actually change.

The agent only makes outbound connections. It never listens on a port.

> [!CAUTION]
> This agent is in early development. It speaks revision 1.0.0 of the agent protocol (`serverchief/protocol/agent/v1`), which may still change before the first production deploy.

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
serverchief collect -d packages -d updates --pretty       # optional datasets, see below
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

### Set up

```bash
sudo serverchief setup <token>
sudo serverchief setup --endpoint https://server.chief.test/agent/v1 --ca-file /path/to/ca.pem <token>
```

Setup exchanges the one-time `scenr_…` token from the dashboard for this server's identity:

1. It generates an attempt ID and a `scagt_…` credential and journals both in `/etc/serverchief/setup.json` (0600) **before** calling the backend, so a retried setup (after a crash or a network error) sends the same values and the backend can replay its answer.
2. It calls `POST /enroll`, writes `/etc/serverchief/identity.json` (0600) with the endpoint, server ID, generation, credential and CA file, caches the configuration from the response, resets the delivery state and deletes `setup.json`.
3. It enables and starts the service through systemd or the SysV script. On a host without a supported init system, or without the packaged unit or script (a container with only the binary), it prints how to run `serverchief run` yourself and still succeeds.

Running setup with a new token on an enrolled server re-enrolls it: the request carries `previous_server_id` and `previous_generation`, the old credential is revoked by the backend, and the service is restarted with the new identity. Running it again with a token that already completed only restarts the service.

`--endpoint` defaults to `$SERVERCHIEF_ENDPOINT`, then `https://server.chief.app/agent/v1`. `--ca-file` (or `$SERVERCHIEF_CA_FILE`) adds a PEM CA to the trusted roots for a local development backend; its absolute path is stored in `identity.json`, so `serverchief run` trusts it too.

### Run the daemon

```bash
sudo serverchief run                                          # systemd: logs to the journal
sudo serverchief run --log-file /var/log/serverchief/agent.log  # SysV: size-capped file log
serverchief run --config ./config.json --state-dir ./state      # development run with local state
```

Without `identity.json` the daemon exits with "not enrolled, run `serverchief setup <token>`". Otherwise it:

- heartbeats right away and then every `intervals.heartbeat` seconds (±10%), reporting the hashes and seq the backend acknowledged. Datasets listed in `resync` are sent in full at once, and a changed `config_etag` triggers `GET /config` with `If-None-Match`.
- collects each dataset the backend enabled on its backend-provided interval and batches the due, changed datasets into one envelope with a persisted, increasing `seq`. `packages` is sent as a delta against the last acknowledged payload once the backend has one. Collector failures go in the envelope's `errors` map whenever they change. Hashes advance only for datasets listed in `accepted` with the hash that was sent. Datasets the backend `ignored` are not sent again until its configuration changes.
- subscribes to `private-agent.server_<id>.generation_<n>` when the configuration has a `realtime` block, authorising the channel with its credential. `config.updated` fetches the configuration and `commands.available` fetches the commands. Without `realtime` it polls only.
- runs backend commands. When a heartbeat answers `commands_available: true` or `commands.available` arrives, it calls `GET /commands`, acknowledges each new command and then executes it: `refresh_inventory` collects the listed (or all enabled) datasets and pushes what changed, `resync` sends them in full, `refresh_config` fetches `GET /config`. Unknown kinds are acknowledged and reported `failed` with `unsupported command kind "<kind>"`. The result (`succeeded`, or `failed` with the error) is posted once the push or config fetch finishes. Acknowledgements and results are retried with the error table's backoff, and a journal of recent commands makes sure a restart neither executes a command twice nor loses its result: a command that was executing when the agent stopped is reported `failed`.
- follows the protocol's error table: `401 revoked`/`unauthenticated` stops every loop and exits with status 2, which the systemd unit does not restart (`RestartPreventExitStatus=2`). `426` pauses inventory and heartbeats hourly. `429` and `503` honour `Retry-After`. `400`, `404`, `413` and `422` are logged and not retried unchanged. Other `5xx` and network errors back off exponentially (2 s doubling to 5 min, with jitter).

`SIGINT` and `SIGTERM` stop the daemon gracefully. `SIGHUP` (`systemctl reload serverchief`) reloads the local configuration, reopens the log file and collects every dataset again.

Every key in `/etc/serverchief/config.json` is optional. These operator overrides win over the backend's intervals, and `datasets` can only narrow what the backend enabled:

```json
{
  "intervals": { "system": "5m", "hardware": "15m" },
  "heartbeat_interval": "5m",
  "datasets": ["system", "hardware", "network", "resolvers"]
}
```

State lives in `/var/lib/serverchief`: `state.json` (seq, acknowledged seq and hashes, last reported errors), `server-config.json` (the backend configuration with its ETag), `payloads/` (delta bases) and `commands.json` (recent commands with their state and result, kept 48 hours after their result was reported). Every file is replaced atomically (temporary file, fsync, rename).

### End-to-end test against a local backend

The Server Chief app runs locally at `https://server.chief.test` under Laravel Valet, whose CA is `~/.config/valet/CA/LaravelValetCASelfSigned.pem`. Create a server in the local app, copy its token, then enroll a container. Under OrbStack, containers reach the Mac's Valet through `host.orb.internal`; `--add-host server.chief.test:host-gateway` maps the app's hostname to that address (0.250.250.254), or pass the IPv4 address of `host.orb.internal` explicitly:

```bash
./bin/build.sh dev linux-arm64

docker run --rm -it \
  --add-host server.chief.test:host-gateway \
  -v "$PWD/serverchief-linux-arm64:/usr/bin/serverchief:ro" \
  -v "$HOME/.config/valet/CA/LaravelValetCASelfSigned.pem:/valet-ca.pem:ro" \
  ubuntu:16.04 sh -c '
    serverchief setup --endpoint https://server.chief.test/agent/v1 --ca-file /valet-ca.pem scenr_… &&
    serverchief run'
```

The container has no init system, so setup prints the manual-start hint and `serverchief run` runs in the foreground. The realtime host (`socket.chieftools.cloud`) is public and needs no extra mapping. Without a container, run both commands as root on a Linux VM with the same `--add-host`-style `/etc/hosts` entry.

## Datasets

| Dataset | Contents | Sources |
| --- | --- | --- |
| `system` | hostname, FQDN, OS family, ID, version, point release and codename, kernel, architecture, virtualization, boot time and ID, timezone, init system | `/etc/os-release` with `/etc/redhat-release`, `/etc/lsb-release` and `/etc/debian_version` fallbacks; `uname(2)`; `/etc/hosts` then DNS for the FQDN; DMI, `/proc/1/environ`, `/proc/1/cgroup`, `/proc/vz`, `/proc/xen`, kvm-clock and the CPU `hypervisor` flag for virtualization (no `systemd-detect-virt` needed) |
| `hardware` | CPU vendor, model, sockets, cores, threads and flags; RAM and swap totals; block devices with partitions; mounted storage file systems with size and inode totals | `/proc/cpuinfo` (x86 and ARM layouts, ARM implementer/part names), sysfs CPU topology, `/proc/meminfo`, `/sys/block`, `/proc/mounts` + `statfs(2)` |
| `network` | interfaces (kind, MAC, MTU, state, virtual flag), addresses with scope, default IPv4 and IPv6 routes | `/sys/class/net`, `net.Interfaces()`, `/proc/net/route`, `/proc/net/ipv6_route`, `/proc/net/if_inet6` |
| `resolvers` | nameservers, search domains and options in file order, the managing tool, systemd-resolved upstreams behind the 127.0.0.53 stub | `/etc/resolv.conf`, `/run/systemd/resolve/resolv.conf` |
| `packages` (optional) | `{manager: dpkg\|rpm, items: [{name, version, arch, source?, versions?}]}` sorted by the delta key `manager/name/arch`; `versions` lists every installed version of rpm installonly packages such as `kernel-core` | `/var/lib/dpkg/status` parsed directly (fully installed packages only); `rpm -qa --queryformat` with an explicit format including the epoch. `gpg-pubkey` entries are skipped. Collected early when the package database mtime changes. |
| `updates` (optional) | `{manager: apt\|yum\|dnf, items: [{name, current_version, candidate_version, arch, security}], security_count, reboot_required, cache_updated_at}` | `apt-get -s -o Debug::NoLocking=1 dist-upgrade` (works on 14.04+, unlike `apt list`; security from `-security` pockets); `yum`/`dnf -C check-update` and `updateinfo list security`; `/var/run/reboot-required`, `needs-restarting -r`. `reboot_required` is null when unknown. `cache_updated_at` (RFC 3339 UTC, null without a cache) is the newest of apt's success stamps (`/var/lib/apt/periodic/update-success-stamp`, `update-stamp`), the lists directory and its `*_Release`/`*_InRelease` files, or of yum/dnf's `repomd.xml`, `cachecookie` and `repodata/*primary.xml*` (whose mtime dnf resets on every metadata check). `lists/partial` is not used because failed updates touch it too. A timestamp rather than an age keeps the hash stable; a refreshed cache also triggers an early collection. |

`packages` and `updates` are only collected when the backend enables them or when selected with `serverchief collect -d`. They never refresh package metadata: no `apt-get update`, no `makecache`. Without a yum/dnf metadata cache, `updates` reports an error that says so. `dist-upgrade` rather than `upgrade` is simulated so that updates held back by new dependencies, such as kernel meta packages, are counted.

Address scopes are `public`, `private` (RFC 1918 and IPv6 ULA `fc00::/7`), `shared` (carrier-grade NAT `100.64.0.0/10`), `loopback`, `link-local`, `multicast`, `unspecified` and `reserved` (documentation, benchmarking and other special-purpose ranges).

Payloads hold only values that are stable while the server is unchanged: capacities rather than usage, and no uptime. Ephemeral interfaces (`veth*`, `cali*`, `tap*`, …), IPv6 privacy addresses, loop and zram devices, and pseudo file systems are left out. Without those exclusions the hashes would change on every run.

### Planned collectors

- [ ] `software`: normalized products (PHP, MySQL/MariaDB, nginx, Apache, OpenSSL, Node.js, …) from packages and panel builds (`/opt/plesk/php/*`, DirectAdmin custombuild, `ea-php*`, `/opt/php-*`).
- [ ] `vhosts`: Apache (`apachectl -S`), nginx (`nginx -T`), Plesk and DirectAdmin, with docroot, PHP handler and certificate expiry.
- [ ] `location`: cloud metadata endpoints (Hetzner, AWS, DigitalOcean, GCP, Azure, OVH) and a manual override.

## Architecture

```
main.go          signal handling, embedded fallback CA roots
cmd/             cobra CLI: version, collect, run, setup
collector/       Collector interface, registry and the built-in collectors
inventory/       canonical JSON, SHA-256 hashes, envelope, keyed-list deltas, state store
agent/           protocol client, enrollment, daemon loop, settings, realtime glue
initsys/         init system detection plus systemd and SysV service managers
logging/         stdlib log setup and a size-capped rotating file writer
systemdnotify/   sd_notify(3) for Type=notify
pusher/          Pusher-protocol websocket client for realtime wake-ups
packaging/       systemd unit, LSB init script, shared deb/rpm maintainer scripts
bin/             build scripts
```

- **Host abstraction:** collectors read the host only through `collector.Host`, which holds an `fs.FS` rooted at `/` plus function fields for live-only sources (`uname`, `statfs`, `net.Interfaces`, DNS). Tests point it at fixtures under `collector/testdata/`. Some of those fixtures were copied verbatim from the official Docker images.
- **Hashing:** `inventory.Canonical` encodes payloads with sorted object keys and no insignificant whitespace. Payloads implement `inventory.Normalizer` to sort lists whose order carries no meaning. The dataset hash is the SHA-256 of that encoding.
- **Envelope:** each upload is `{seq, datasets: {name: {hash, mode: full|delta, base_hash, payload}}, errors}`. `seq` is persisted before use and increases forever. `errors` reports collectors that failed in that round.
- **Delivery:** the daemon schedules each dataset on its own interval with ±10% jitter and uploads only the datasets whose hash differs from the one the backend acknowledged. A failed delivery leaves the state untouched; how it is retried follows the protocol's error table. A `resync` (from a heartbeat, an inventory answer or a `resync` command) makes the agent forget that dataset's hash and payload so it goes out in full right away. If the backend answers with a higher `seq` than the agent sent (state lost or reinstalled), the agent continues from there and resends.
- **Init systems:** the package ships both `serverchief.service` and `/etc/init.d/serverchief` and depends on neither init system. The maintainer scripts check for `/run/systemd/system` at runtime. The SysV script uses `start-stop-daemon` when available and otherwise falls back to `setsid` with a pid file (RHEL-likes).
- **TLS:** old distributions ship stale CA bundles. The agent's HTTP client and the realtime websocket therefore trust the system roots plus the embedded Mozilla bundle from `golang.org/x/crypto/x509roots/fallback`, plus the `--ca-file` CA when one was given at setup.
- **Protocol:** every request carries `ServerChief-Protocol-Revision: 1.0.0`, `User-Agent: serverchief-agent/<version> (linux; <arch>)` and, except `POST /enroll`, `Authorization: Bearer <credential>`. Bodies above 8 KiB are gzipped. Errors are decoded from `application/problem+json` into `agent.ProblemError`. The channel-auth request only ever goes to the enrolled endpoint's origin. The protocol types are in `agent/types.go` and the tests replay the spec's fixtures from `agent/testdata/protocol/`.

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
