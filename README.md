# Server Chief Agent

The Server Chief Agent is a small, static Go binary that inventories a Linux server (operating system, hardware, network, packages, websites and certificates) and reports what changed to [Server Chief](https://server.chief.app/). Each dataset is hashed on its canonical JSON form, so the agent only uploads a dataset when its contents actually change.

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

`serverchief sample` reads the host's usage twice, a second apart (`--over 10s` for longer), and prints the metrics sample the daemon would send. It also needs neither setup nor root.

```bash
serverchief sample --pretty
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
systemctl status serverchief                                  # systemd: runs as the serverchief user, logs to the journal
sudo serverchief run --log-file /var/log/serverchief/agent.log  # SysV: size-capped file log
serverchief run --config ./config.json --state-dir ./state      # development run with local state
```

Without `identity.json` the daemon exits with "not enrolled, run `serverchief setup <token>`". Otherwise it:

- heartbeats right away and then every `intervals.heartbeat` seconds (±10%), reporting the hashes and seq the backend acknowledged. Datasets listed in `resync` are sent in full at once, and a changed `config_etag` triggers `GET /config` with `If-None-Match`.
- collects each dataset the backend enabled on its backend-provided interval and batches the due, changed datasets into one envelope with a persisted, increasing `seq`. `packages`, `vhosts` and `certificates` are sent as deltas against the last acknowledged payload once the backend has one. Collector failures go in the envelope's `errors` map whenever they change. Hashes advance only for datasets listed in `accepted` with the hash that was sent. Datasets the backend `ignored` are not sent again until its configuration changes.
- subscribes to `private-agent.server_<id>.generation_<n>` when the configuration has a `realtime` block, authorising the channel with its credential. `config.updated` fetches the configuration and `commands.available` fetches the commands. Without `realtime` it polls only.
- runs backend commands. When a heartbeat answers `commands_available: true` or `commands.available` arrives, it calls `GET /commands`, acknowledges each new command and then executes it: `refresh_inventory` collects the listed (or all enabled) datasets and pushes what changed, `resync` sends them in full, `refresh_config` fetches `GET /config`, `update_agent` hands an agent update to the update helper (see *Agent updates*). Unknown kinds are acknowledged and reported `failed` with `unsupported command kind "<kind>"`. The result (`succeeded`, or `failed` with the error) is posted once the push or config fetch finishes. Acknowledgements and results are retried with the error table's backoff, and a journal of recent commands makes sure a restart neither executes a command twice nor loses its result: a command that was executing when the agent stopped is reported `failed`.
- follows the protocol's error table: `401 revoked`/`unauthenticated` stops every loop and exits with status 2, which the systemd unit does not restart (`RestartPreventExitStatus=2`). `426` pauses inventory and heartbeats hourly. `429` and `503` honour `Retry-After`. `400`, `404`, `413` and `422` are logged and not retried unchanged. Other `5xx` and network errors back off exponentially (2 s doubling to 5 min, with jitter).

`SIGINT` and `SIGTERM` stop the daemon gracefully. `SIGHUP` (`systemctl reload serverchief`) reloads the local configuration, reopens the log file and collects every dataset again.

Every key in `/etc/serverchief/config.json` is optional. These operator overrides win over the backend's intervals and `package_metadata_refresh`, and `datasets` can only narrow what the backend enabled:

```json
{
  "intervals": { "system": "5m", "hardware": "15m" },
  "heartbeat_interval": "5m",
  "datasets": ["system", "hardware", "network", "resolvers"],
  "package_metadata_refresh": { "enabled": true, "interval": "6h" }
}
```

State lives in `/var/lib/serverchief`: `state.json` (seq, acknowledged seq and hashes, last reported errors), `server-config.json` (the backend configuration with its ETag), `payloads/` (delta bases), `metadata-refresh.json` (the last successful package metadata refresh) `commands.json` (recent commands with their state and result, kept 48 hours after their result was reported) and, while an agent update runs, `update-request.json`, `update-state.json`, `update-ready.json` and `update-result.json` (see *Agent updates*). Every file is mode 0600 and replaced atomically (temporary file, fsync, rename).

### Metrics

When the backend's configuration turns metrics on (`metrics: {enabled, endpoint, interval}`, protocol 1.1.0), the daemon also samples host usage every interval (60 s by default) and delivers it to the metrics gateway, following the metrics protocol (`serverchief/protocol/metrics/v1`). Sampling runs next to the inventory loop, so a slow collection never delays it.

- **What:** CPU time shares, load, processes, memory, swap, per-file-system usage and inodes, per-disk throughput, IOPS and utilisation, per-interface traffic, packets, errors and drops, TCP connections and uptime, all from `/proc` and `statfs`; nothing needs root. Rates cover the interval before the sample, and a counter that went backwards (a reboot, a recreated interface) leaves its rates out once. Disks, file systems and interfaces follow the inventory's exclusions (no loop, ram or zram devices, empty optical drives, bind mounts of the same device, or container plumbing such as `veth*`, `docker*` and `br-*`), with at most 64 of each. Disk and network metrics also come as host totals (empty instance) over the devices that count, so a byte is counted once: disks without another block device beneath them (not LVM or md RAID) and interfaces backed by a network device (not bonds, bridges, VLANs or tunnels; in a container, those not stacked on another).
- **When:** samples are taken on interval boundaries (`at % interval == 0`), so a fleet lines up, and each agent sends a random 0–30 s after the boundary, so a fleet does not arrive at once.
- **Spool:** every sample is a file in `/var/lib/serverchief/metrics/samples` until the gateway stored it. Up to 60 samples go out as one batch, which is kept as `batch.json` and resent unchanged after an ambiguous failure, so the gateway stores it once. The spool keeps at most 24 hours and 8 MiB, dropping the oldest samples first.
- **Failures:** a `401` waits 5 minutes (a just-enrolled server the panel has not published yet), `403 metrics_disabled` drops the spool and waits for a new configuration, `426` waits for a new configuration, `429` and `503` honour `Retry-After`, and other errors back off from 2 s to 5 minutes. A rejected batch (`400`, `413`, `415`, `422`) is dropped.
- **Live:** the realtime event `metrics.live {until}` asks for live samples while someone watches the server's charts. The agent then reads usage every second (without file systems, so a slow network mount cannot hold it up) and posts two samples at a time to the gateway's `/v1/live`, until `until` (at most 5 minutes ahead; the panel repeats the event) or until the gateway reported no watching browser for 10 seconds. Live samples are never stored, spooled or retried; `401` and `403` end them until the next event.
- **Heartbeat:** `capabilities.metrics {enabled, last_sent_at, spool_bytes, error, live}` lets the panel show when samples stop arriving and whether it can ask for live samples. Turning metrics off clears the spool.

### Privileges

The packaged daemon runs as the `serverchief` system user, not as root. `serverchief.service` grants it only `CAP_DAC_READ_SEARCH`, so it can read package databases, web server and panel configuration and certificates, but cannot write anything outside `/var/lib/serverchief` and `/var/log/serverchief` (`ProtectSystem=strict`, `NoNewPrivileges`). `identity.json` is `root:serverchief 0640`.

The few things that need root go through the root helper: the daemon writes a request to `/var/lib/serverchief/helper/`, `serverchief-helper.path` starts `serverchief-helper.service` (`serverchief internal-helper`, root, oneshot), and the helper writes the result back. It knows a fixed list of actions without arguments: `ping`, `refresh-metadata` (the package metadata refresh), `plesk-sites` (reads the Plesk sites; the helper builds the queries) and `start-agent-update`. It opens requests without following symbolic links, only accepts small files of the directory's owner and creates results as new files.

Where systemd cannot grant ambient capabilities (before 229, such as CentOS 7) the package installs `/etc/systemd/system/serverchief.service` from `/usr/share/serverchief/serverchief-root.service`, and under SysV the init script runs the daemon as root: it then runs the same actions in-process. An update from a version that ran as root (before 0.2.0) finishes as root and then switches to the `serverchief` user on its own (`serverchief internal-unprivilege`). Every heartbeat reports the mode and the available helper actions, and the backend records a change of mode.

Because the daemon can read every file, a symbolic link inside a hosting user's tree is only followed when it stays inside that tree, and files are read up to a size cap.

### Agent updates

The backend sends `update_agent {"version": "X.Y.Z"}` only to agents whose heartbeat reports `capabilities.agent_update.available: true`. Every heartbeat carries it; the probe is cached for 10 minutes (1 minute while unavailable, since the new daemon first starts inside the package's `postinst`). It is available only when all of these hold, and otherwise `reason` says which failed:

- a stable release build (`dev` and pre-release builds never are),
- dpkg or rpm reports the `serverchief` package, fully installed, at the running version (not "installed from a tarball"),
- the Server Chief repository is configured: `/etc/apt/sources.list.d/serverchief.list` or `.sources`, or `/etc/yum.repos.d/serverchief.repo`; apt must be 1.1 or later (`--allow-downgrades`, deb822 files),
- the helper can be launched and the package can restart this daemon: under systemd, `systemd-run` exists and the daemon runs as `serverchief.service` (from `/proc/self/cgroup`); under SysV or Upstart, the init script exists and its pid file holds the daemon's PID. A container without an init system is not available.

The command carries nothing but the version, which must match `^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$`; other fields are rejected. The daemon checks the capability again, journals the command as `handed_off`, writes `update-request.json` (command ID, running and target version) and launches the helper so that it survives the package's restart of the daemon:

- systemd: `systemd-run --unit=serverchief-update --collect --no-block --description=… /usr/bin/serverchief internal-update`, a transient unit outside the daemon's unit and sandbox (systemd < 236 gets neither `--collect` nor `--no-block`, and a failed unit of the same name is reset first),
- SysV: a double fork (`internal-update --detach` in a new session starts the helper with `Setsid` and exits), with its output in `/var/log/serverchief/update.log`.

The helper (`serverchief internal-update`, hidden, root only) holds an fcntl lock on `update.lock` for its whole run, so a second launch exits, and records each phase in `update-state.json`:

1. **Wait for the package manager:** the `pkglock` locks are checked every 30 s for up to an hour (counted from the first attempt, across restarts), then `package_manager_busy`.
2. **Preflight:** refresh the Server Chief repository only, then require both the target and the installed version in it: `target_unavailable`, `rollback_unavailable`.
3. **Install** only `serverchief`, only from that repository (no shell, fixed argv):
   - `apt-get install -y --only-upgrade --allow-downgrades -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold -o Dir::Etc::sourcelist=<serverchief.list|.sources> -o Dir::Etc::sourceparts=- -o APT::Get::List-Cleanup=0 serverchief=<version>` (apt reads a `.sources` file named by `Dir::Etc::sourcelist` as deb822; the refresh is `apt-get update -qq` and the query `apt-cache madison serverchief` with the same restriction),
   - `dnf|yum --disablerepo=* --enablerepo=serverchief --setopt=exit_on_lock=True install -y serverchief-<version>` (`downgrade` when the version is older; refresh `dnf --refresh makecache` or `yum clean expire-cache`, query `--showduplicates list available serverchief`).
4. **Readiness:** the package's `postinst` restarts the daemon. A daemon that starts while a request is pending writes `update-ready.json` (command ID, version, PID) after its first successful heartbeat; the helper waits up to 2 minutes for a marker of the target version from a live process.
5. **Rollback:** otherwise the previous version is installed the same way and must report ready too.
6. **Result:** `update-result.json` with `updated`, `install_failed` (the package manager failed and the previous version is intact), `readiness_failed_rolled_back`, `rollback_failed`, the preflight codes, `package_manager_busy` or `unsupported_install`; then the request, state and marker are removed.

A crashed or interrupted helper resumes from `update-state.json` when it runs again: the daemon relaunches it once after a start when a request is pending and no helper holds the lock, and a helper that stopped five times gives up with what is installed. The daemon polls for the result every 5 s, reports it with `data: {code, previous_version, target_version, installed_version}` and deletes the file once the backend accepted it. A restarted daemon never runs a handed-off command again (it re-acknowledges it if the backend lists it again); one that has neither a request nor a result is reported `install_failed`. A second `update_agent` while one is in progress gets `package_manager_busy`, and the daemon's own metadata refresh waits until the update is done.

The daemon's unit hardening does not apply to the helper, which runs in its own transient unit. Within the unit, `ProtectSystem=full` and `MemoryDenyWriteExecute=yes` do not affect the metadata refresh: `apt-get update` and `dnf makecache` were verified as children of the hardened `serverchief.service` on Debian 12 and AlmaLinux 9 (full downloads into `/var/lib/apt/lists` and `/var/cache/dnf`, apt's `_apt` privilege drop under `NoNewPrivileges`), so the unit is unchanged.

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
| `packages` (optional) | `{manager: dpkg\|rpm, items: [{name, version, arch, source?, source_version?, versions?, origins?}]}` sorted by the delta key `manager/name/arch`; `source_version` is set when it differs from `version` (dpkg `Source: name (version)`, rpm `SOURCERPM` version-release with the binary's non-zero epoch); `versions` lists every installed version of rpm installonly packages such as `kernel-core`; `origins` lists the sorted `repositories` ids offering the installed version, `[]` when none does (a manually installed `.deb`/`.rpm`, a removed repository, a superseded version gone from the mirror), and is left out when it cannot be told (no apt lists yet, dnf failed) | `/var/lib/dpkg/status` parsed directly (fully installed packages only); `rpm -qa --queryformat` with an explicit format including the epoch. `gpg-pubkey` entries are skipped. Origins: apt streams the `*_Packages` lists of the enabled `deb` sources in `/var/lib/apt/lists` (plain, `.lz4`, as the Debian and Ubuntu container images keep them, `.gz` and `.bz2`), keeping only installed names and matching name, version and arch; each list is mapped to its source by apt's own file naming of the URI, suite and component. rpm: `dnf -C -q --setopt=exit_on_lock=True repoquery --installed --qf '%{name}\t%{arch}\t%{from_repo}\n'`, or `yum -C -q --setopt=exit_on_lock=True list installed` on EL7 (yumdb needs yum-utils), keeping configured repository ids only (`@System`, `@commandline`, `anaconda`, `installed` give none). The origin index is shared with `repositories` and reused until the package database, the sources or the lists change. Collected early, once idle, after the package database changes (see *Package metadata refresh and package activity*). |
| `repositories` (optional) | `{manager: apt\|yum\|dnf, items: [{id, file, type: deb\|deb-src\|rpm, enabled, uris, suite?, components?, name?, signing: {signed_by, trusted, gpgcheck}, transport, packages}]}` in configuration order. apt items are one per type, URI and suite, with the id `<suite>:<first 8 hex digits of the SHA-256 of file, type, URI, suite and components>`; rpm ids are the `.repo` sections. `uris` never contain credentials (`user:pass@`) or query strings, except the identifying `repo`, `arch`, `country`, `protocol`, `content`, `infra` and `release` parameters of `mirrorlist`/`metalink` URIs (`metalink?repo=epel-$releasever&arch=$basearch`). An inline deb822 `Signed-By` key is reported as its fingerprints (or `inline`), never whole. `transport` is the first URI's scheme, the method for `mirror+`/`tor+`. `packages` counts the installed packages whose `origins` include the id. | `/etc/apt/sources.list` and `sources.list.d/*.list` (one-line, `[signed-by= trusted= arch=]` options, `#` comments, malformed lines skipped) and `*.sources` (deb822: several `Types`/`URIs`/`Suites`, `Enabled: no`, `Trusted`, `Signed-By` as paths, fingerprints or an inline key). `/etc/yum.repos.d/*.repo` (`name`, `baseurl` over several lines, `mirrorlist:`/`metalink:` URIs, `enabled`, `gpgcheck` falling back to `[main]` of `dnf.conf`/`yum.conf`, the first `gpgkey` as `signed_by`; `$releasever`/`$basearch` left unexpanded) with the effective `enabled` from `dnf -C -q --setopt=exit_on_lock=True repolist --all` or `yum ... repolist all`. Never read: `/etc/apt/auth.conf` and `auth.conf.d`; `.repo` keys other than those listed (`username`, `password`, `proxy_password`, `sslclientkey`, …) are dropped while parsing. Collected early when the sources, `yum.repos.d` or apt's lists change, after the agent's own metadata refresh and after package activity. |
| `updates` (optional) | `{manager: apt\|yum\|dnf, items: [{name, current_version, candidate_version, arch, security}], security_count, reboot_required, cache_updated_at}` | `apt-get -s -o Debug::NoLocking=1 dist-upgrade` (works on 14.04+, unlike `apt list`; security from `-security` pockets); `yum`/`dnf -C check-update` and `updateinfo list security`; `/var/run/reboot-required`, `needs-restarting -r`. `reboot_required` is null when unknown. `cache_updated_at` (RFC 3339 UTC, null without a cache) is the newest of apt's success stamps (`/var/lib/apt/periodic/update-success-stamp`, `update-stamp`), the lists directory and its `*_Release`/`*_InRelease` files, or of yum/dnf's `repomd.xml`, `cachecookie` and `repodata/*primary.xml*` (whose mtime dnf resets on every metadata check), or the agent's own last successful metadata refresh when that is newer. `lists/partial` is not used because failed updates touch it too. A timestamp rather than an age keeps the hash stable; a refreshed cache also triggers an early collection. Skipped, keeping the previous dataset, while a package manager holds its lock. |
| `cloud` (optional) | `{provider, source: metadata\|dmi, region, zone, instance_type, instance_id, public_ipv4, public_ipv6}`, every field null when nothing is detected | DMI (`sys_vendor`, `product_name`, `bios_vendor`, `chassis_asset_tag`, `board_vendor`, plus `/sys/hypervisor/uuid` for Xen-era EC2) picks the provider, then only that provider's metadata leaves are read: Hetzner `/hetzner/v1/metadata`, AWS IMDSv2, DigitalOcean `/metadata/v1/*`, GCP `computeMetadata/v1`, Azure IMDS text leaves, OpenStack/OVH EC2-compatible leaves, Oracle `/opc/v2/instance/*`, Vultr `/v1.json`, Linode `/v1/instance` with a token, Scaleway `169.254.42.42/conf` from a privileged port. When DMI is inconclusive one probe of 169.254.169.254 runs (connect ≤ 500 ms, all of it ≤ 2 s). No proxy, no redirects, never user data, vendor data or credentials. Collected at start and daily. |
| `vhosts` (optional) | `{items: [{source, name, aliases, ports, ssl, http2, http3, document_root, php: {version, handler}, certificate: {subject, issuer, not_after, names, fingerprint}, config_file, owner, redirect: {status, target, host}, https_redirect}]}` sorted by the delta key `source/name` | `apache2ctl -S` / `httpd -S` / `apachectl -S` (first that works) plus the referenced `<VirtualHost>` blocks and their `Include`s; nginx: `nginx.conf` (the `--conf-path` of `nginx -V`, else the usual locations) parsed with its `include` globs followed, at most 8 levels and 5000 files deep, skipping missing files (never `nginx -t`/`-T`, whose config test opens and can create every log file); Plesk `plesk db -Ne` read-only queries (fallback `plesk bin site --list`), with the form-encoded public `certificates.cert` (never `cert_file`, which holds the key); DirectAdmin `data/users/*/domains.list` and `domains/<domain>.{conf,subdomains,pointers,cert}` with `custombuild/options.conf` (never `.key` or `.combined`). Panel items include the implicit `www.<name>` alias. Blocks that only redirect (nginx `return`/`rewrite`, Apache `Redirect`/`RewriteRule`) fold into the site they point to: an HTTPS redirect on the same host sets `https_redirect` and adds its port, a redirect to another served host (`www.` to the apex) adds its names as aliases; other redirects stay items with `redirect` set. Plesk's `ipv4.`/`ipv6.`/preview names and the `www.` of Plesk and DirectAdmin subdomains are dropped from every source unless the certificate covers them. `http2` comes from nginx `http2 on`/`listen … http2` or Apache `Protocols h2`, `http3` from an nginx `listen … quic` without `http3 off`. Sites no panel manages get the regular user whose home directory holds the document root as `owner` (Forge, Ploi). Apache `php_value`-only sites take the server-wide handler (mod_lsapi, mod_php) or, for DirectAdmin vhosts, its PHP selection. Certificates: first PEM certificate of the configured file only, never a key; `fingerprint` links the site to the `certificates` dataset. Collected early when `/etc/apache2`, `/etc/httpd`, `/etc/nginx`, `/etc/letsencrypt/live`, `/var/www/vhosts/system` or DirectAdmin's `data/users` change. |
| `certificates` (optional) | `{items: [{fingerprint, serial, subject: {cn, org}, issuer: {cn, org}, not_before, not_after, names, key: {type, bits, curve}, signature_algorithm, self_signed, chain: {complete, intermediates}, paths, used_by: [{kind, ref}], managed_by: {tool, name, plugin, installer, ca, ca_url, key_type, renewal, config}}]}` sorted by the delta key `fingerprint`, one item per leaf however many sites and services use it. `chain.complete` verifies the leaf against the system roots plus the embedded bundle with the intermediates of the same file or chain file, at a moment inside the leaf's validity (expired certificates do not read as incomplete); null for self-signed certificates. | Sites from the `vhosts` collection (reused when at most 30 minutes old and unchanged), including Apache's `SSLCertificateChainFile`, DirectAdmin's `.cacert` and Plesk's `certificates.cert`/`ca_cert`; Postfix `postconf -x smtpd_tls_cert_file smtpd_tls_eccert_file smtpd_tls_chain_files` (else `main.cf`); Dovecot `ssl_cert = <…` with `!include`/`!include_try`; Exim `tls_certificate` in the main section with macros, `.ifdef` and `.include`; Plesk `admin/conf/httpsd.pem`; DirectAdmin `cacert`/`carootcert`. Renewal: certbot `/etc/letsencrypt/renewal/*.conf` (`certbot.timer`, `snap.certbot.renew.timer` or `certbot-renew.timer` in a `*.wants` directory, else a `certbot renew` crontab line), acme.sh `~root/.acme.sh/<domain>[_ecc]/<domain>.conf` (an `acme.sh --cron` crontab line), Plesk `Lets Encrypt …` certificates and DirectAdmin `letsencrypt=1`/`.cert.creation_time`/`.san_config` (`panel`). PEM files are scanned block by block and only `CERTIFICATE` blocks are decoded; keys are skipped unread and the read buffer is wiped. Files over 1 MiB, non-regular files and symbolic links in user-owned trees (`/home/*`, `/srv/*`, `/var/www/*`, or any directory owned by a non-root user) that point outside that tree are skipped with a warning. Parsed files are cached by path, size, mtime and inode, shared with `vhosts`. Collected early when the web server, mail, panel, certbot or acme.sh trees or the directories of the certificates found change. |

`packages`, `repositories`, `updates`, `cloud`, `vhosts` and `certificates` are only collected when the backend enables them or when selected with `serverchief collect -d`. The collectors themselves never refresh package metadata; the daemon does that on its own schedule (below). Without a yum/dnf metadata cache, `updates` reports an error that says so until the first refresh. `dist-upgrade` rather than `upgrade` is simulated so that updates held back by new dependencies, such as kernel meta packages, are counted.

### Package metadata refresh and package activity

Apart from installing its own `serverchief` package through the update helper, the agent never installs, upgrades or removes packages. `TestPackageCommandAllowList` and `TestUpdateCommandAllowList` pin every package-manager command it can run, and `TestNoMutatingPackageVerbsInSource` fails on any string literal such as `install` or `remove` in the agent's sources outside `updater/commands.go`, which may only use `install`, `downgrade` and `-y`.

- **Metadata refresh.** While `updates` is enabled and `package_metadata_refresh.enabled` (backend default: on, every 21600 s), the daemon refreshes the metadata cache every interval ±10%, starting up to 5 minutes after start:
  - `nice -n 19 ionice -c3 -t apt-get update -qq`
  - `nice -n 19 ionice -c3 -t yum makecache fast -q --setopt=exit_on_lock=True` (EL7, Amazon Linux 2; plain `makecache` on other yum hosts)
  - `nice -n 19 ionice -c3 -t dnf makecache -q --setopt=exit_on_lock=True`

  The agent writes no file of apt, yum or dnf itself. It records the time of its last successful refresh in `/var/lib/serverchief/metadata-refresh.json` (atomic write), and `cache_updated_at` is the newest of the cache mtimes above and that time, for apt as well as yum/dnf (only when a cache exists). apt keeps the lists' mtimes when nothing changed upstream, so without the record a successful refresh would not show. The value only moves when a refresh succeeded or the cache changed, so the hash stays stable. `nice` and `ionice` are left out when missing; `ionice -t` ignores an IO scheduler without priorities. The refresh runs in the background with a 10-minute timeout (SIGTERM, then SIGKILL 10 s later). It is skipped when `cache_updated_at` is younger than the interval (apt's daily timer already ran) and when any package-manager lock is held, then tried again in 10-15 minutes. A failure (EOL mirrors, no network, an index that failed to download) is logged in one line and retried a cycle later, backing off to 24 hours; it is never a dataset error, because `updates` still reports from the existing cache. A success collects and pushes `updates` and `repositories` at once (`packages`, whose origins can change with the lists, follows on its interval).
- **Package activity trigger.** Every 30 s the daemon `stat`s the package database (`/var/lib/dpkg/status`, else `/var/lib/rpm/rpmdb.sqlite`, `/usr/lib/sysimage/rpm/rpmdb.sqlite`, `/var/lib/rpm/Packages`). After a change it waits until the database has not changed for 120 s **and** no lock is held, then collects `packages`, `updates` and `repositories` once. A long `apt upgrade` keeps both moving, so it is collected once, after it finished. When a lock has been held for over 2 hours (a hung dpkg), it collects once the database has settled. The metadata refresh only writes the lists and caches, never the database, so it does not trigger it.
- **Locks are only inspected, never taken** (`pkglock`): `fcntl(F_GETLK)` on a read-only descriptor of `/var/lib/dpkg/lock-frontend`, `/var/lib/dpkg/lock`, `/var/lib/apt/lists/lock`, `/var/cache/apt/archives/lock`, `/var/lib/rpm/.rpm.lock`, `/usr/lib/sysimage/rpm/.rpm.lock` and dnf5's `/run/dnf/rpmtransaction.lock` (missing files are not created), plus a live-PID check (`kill(pid, 0)`) of `/var/run/yum.pid` and dnf 4's `/var/cache/dnf/*_lock.pid` and `/var/lib/dnf/rpmdb_lock.pid`. Stale, empty or malformed PID files count as free. The log names the busy lock and its PID.
- **Collection never waits on a package manager.** `updates`, and on yum/dnf hosts `packages` and `repositories`, are skipped while a lock is held, keeping the previous dataset and any reported error, and retried in 10-15 minutes; after 2 hours of a held lock it runs anyway (apt's simulation uses `Debug::NoLocking`). yum and dnf, which otherwise wait forever for each other, run with `--setopt=exit_on_lock=True` and a 90-second timeout; a lock they report ("Existing lock", "already locked by", exit 200) also skips the round.

Local overrides in `config.json` win over the backend:

```json
{ "package_metadata_refresh": { "enabled": false } }
{ "package_metadata_refresh": { "interval": "12h" } }
```

The interval is at least 1 hour. For development only, `SERVERCHIEF_DEBUG_INTERVALS=1 serverchief run` shortens these timings (poll 2 s, idle 10 s, refresh every minute, lock retry 20 s, lock patience 2 minutes) so they can be watched in a throwaway container. It is not a supported setting.

`vhosts` sources run only when installed (the binary, `/usr/local/psa` or DirectAdmin's `data/users`). A failing source is logged and keeps its items from the last successful run; the dataset reports an error only when every installed source failed. Names that are IP addresses (Apache's fallback name for a vhost without `ServerName`) and nginx's `_` / `""` catch-alls are left out. `config_file` has no line number, so editing one site does not change the others.

Address scopes are `public`, `private` (RFC 1918 and IPv6 ULA `fc00::/7`), `shared` (carrier-grade NAT `100.64.0.0/10`), `loopback`, `link-local`, `multicast`, `unspecified` and `reserved` (documentation, benchmarking and other special-purpose ranges).

Payloads hold only values that are stable while the server is unchanged: capacities rather than usage, and no uptime. Ephemeral interfaces (`veth*`, `cali*`, `tap*`, …), IPv6 privacy addresses, loop and zram devices, and pseudo file systems are left out. Without those exclusions the hashes would change on every run.

### Planned collectors

- [ ] `software`: normalized products (PHP, MySQL/MariaDB, nginx, Apache, OpenSSL, Node.js, …) from packages and panel builds (`/opt/plesk/php/*`, DirectAdmin custombuild, `ea-php*`, `/opt/php-*`).

## Architecture

```
main.go          signal handling, embedded fallback CA roots
cmd/             cobra CLI: version, collect, sample, run, setup, internal-update (hidden)
collector/       Collector interface, registry and the built-in collectors, plus the package system for the daemon's metadata refresh
pkglock/         read-only package-manager lock inspection (F_GETLK, PID files)
trust/           trusted roots: the system pool plus the embedded Mozilla bundle
updater/         agent self-updates: capability, request/state/result files, the internal-update helper
inventory/       canonical JSON, SHA-256 hashes, envelope, keyed-list deltas, state store
metrics/         host metrics: samples derived from collector.Usage, the on-disk spool and delivery to the metrics gateway
agent/           protocol client, enrollment, daemon loop, settings, realtime glue
initsys/         init system detection plus systemd and SysV service managers
logging/         stdlib log setup and a size-capped rotating file writer
systemdnotify/   sd_notify(3) for Type=notify
pusher/          Pusher-protocol websocket client for realtime wake-ups
packaging/       systemd unit, LSB init script, shared deb/rpm maintainer scripts
bin/             build, packaging, repository and release scripts
```

- **Host abstraction:** collectors read the host only through `collector.Host`, which holds an `fs.FS` rooted at `/` plus function fields for live-only sources (`uname`, `statfs`, `net.Interfaces`, DNS). Tests point it at fixtures under `collector/testdata/`. Some of those fixtures were copied verbatim from the official Docker images.
- **Hashing:** `inventory.Canonical` encodes payloads with sorted object keys and no insignificant whitespace. Payloads implement `inventory.Normalizer` to sort lists whose order carries no meaning. The dataset hash is the SHA-256 of that encoding.
- **Envelope:** each upload is `{seq, datasets: {name: {hash, mode: full|delta, base_hash, payload}}, errors}`. `seq` is persisted before use and increases forever. `errors` reports collectors that failed in that round.
- **Delivery:** the daemon schedules each dataset on its own interval with ±10% jitter and uploads only the datasets whose hash differs from the one the backend acknowledged. A failed delivery leaves the state untouched; how it is retried follows the protocol's error table. A `resync` (from a heartbeat, an inventory answer or a `resync` command) makes the agent forget that dataset's hash and payload so it goes out in full right away. If the backend answers with a higher `seq` than the agent sent (state lost or reinstalled), the agent continues from there and resends.
- **Init systems:** the package ships both `serverchief.service` and `/etc/init.d/serverchief` and depends on neither init system. The maintainer scripts check for `/run/systemd/system` at runtime. The SysV script uses `start-stop-daemon` when available and otherwise falls back to `setsid` with a pid file (RHEL-likes).
- **TLS:** old distributions ship stale CA bundles. The agent's HTTP client and the realtime websocket therefore trust the system roots plus the embedded Mozilla bundle from `golang.org/x/crypto/x509roots/fallback`, plus the `--ca-file` CA when one was given at setup.
- **Protocol:** every request carries `ServerChief-Protocol-Revision: 1.0.0`, `User-Agent: serverchief-agent/<version> (linux; <arch>)` (followed by ` server/<id>` once enrolled, on the realtime connection too) and, except `POST /enroll`, `Authorization: Bearer <credential>`. Bodies above 8 KiB are gzipped. Errors are decoded from `application/problem+json` into `agent.ProblemError`. The channel-auth request only ever goes to the enrolled endpoint's origin. The protocol types are in `agent/types.go` and the tests replay the spec's fixtures from `agent/testdata/protocol/`.

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

For end-to-end tests of agent updates in throwaway containers, `TEST_BUILD_TAGS` adds test-only hooks to `bin/build.sh`: `serverchief_e2e_never_ready` (the daemon never writes its readiness marker, so an update to it rolls back) and `serverchief_e2e_short_lock_wait` (the helper gives up on a held lock after 30 s). Never set it for a release.

## Releasing

Add each change to the Unreleased section of `CHANGELOG.md` as it lands. To cut a release, run `bin/release.sh X.Y.Z` on a clean `main`: it moves the Unreleased entries under `## [X.Y.Z]`, adds the version to the links in the changelog footer, commits "Update changelog" and creates the signed `vX.Y.Z` tag on that commit. Review the result, then push it with `git push --atomic origin main vX.Y.Z`, or pass `--push` to do that right away.

Pushing a `vX.Y.Z` tag runs `.github/workflows/release.yml`: it builds the tarballs, `.deb` and `.rpm` packages, signs the RPMs and `SHA256SUMS` (`SHA256SUMS.asc`), adds the packages to the signed apt (`deb/`, suite `stable`, component `main`) and yum/dnf (`rpm/stable/<arch>/`) repositories, publishes them with `gpg.key` and `install.sh` over rsync, and creates the GitHub release from the version's `CHANGELOG.md` section. A manual run (`workflow_dispatch`) builds a version and publishes it only when `publish` is set; `rebuild_rpm_repo` regenerates the RPM metadata from every published RPM. Publishing stops with an error when the existing repository metadata cannot be downloaded, so a network or SSH failure never replaces the indexes with ones that lack the older versions; only paths that do not exist yet (the first release) are skipped. The steps live in `bin/sign-packages.sh`, `bin/build-repo.sh` and `bin/generate-install-sh.sh`, so they also run locally.

Repository secrets:

- `SSH_HOST`: `user@host` of the package server
- `SSH_PATH`: the package site's web root on that host
- `SSH_PRIVATE_KEY`: the deploy key (only rsync is used)
- `GPG_PRIVATE_KEY`: the ASCII-armored signing key
- `GPG_PASSPHRASE`: its passphrase

Repository variables:

- `GPG_KEY_ID`: the signing key's full 40-character fingerprint
- `PACKAGE_BASE_URL`: the HTTPS URL of the package site, without a trailing slash

The signing key is RSA 4096 (`Server Chief Team <hello@chief.app>`, no expiry): CentOS 7's rpm and Ubuntu 14.04's gpgv cannot verify Ed25519 signatures.

`install.sh` configures the repository under the names the updater checks (`/etc/apt/sources.list.d/serverchief.sources` with `Signed-By: /etc/apt/keyrings/serverchief.gpg`, `/etc/yum.repos.d/serverchief.repo` with `repo_gpgcheck=1`) and installs the package; it does not enroll the server. apt 1.0 (Ubuntu 14.04) gets `serverchief.list` and the key in `/etc/apt/trusted.gpg.d/` instead, and cannot self-update.

## License

The Server Chief Agent is licensed under the Apache License 2.0. See [LICENSE](LICENSE) for the license text.
