# How the agent works

This page describes how the agent behaves on a server and how its code is organised. The [agent protocol](protocol/README.md) specifies what goes over the wire, [Datasets](datasets.md) what the agent collects and from where, and [Remote actions](remote-actions.md) how reboots, package updates and agent updates run. The [README](../README.md) covers installing and using the agent.

## Enrollment

Setup exchanges the one-time `scenr_…` token from the dashboard for this server's identity:

1. It generates an attempt ID and a `scagt_…` credential and journals both in `/etc/serverchief/setup.json` (0600) **before** calling the backend, so a retried setup (after a crash or a network error) sends the same values and the backend can replay its answer.
2. It calls `POST /enroll`, writes `/etc/serverchief/identity.json` (0600) with the endpoint, server ID, generation, credential and CA file, caches the configuration from the response, resets the delivery state and deletes `setup.json`.
3. It enables and starts the service through systemd or the SysV script. On a host without a supported init system, or without the packaged unit or script (a container with only the binary), it prints how to run `serverchief run` yourself and still succeeds.

Running setup with a new token on an enrolled server re-enrolls it: the request carries `previous_server_id` and `previous_generation`, the old credential is revoked by the backend, and the service is restarted with the new identity. Running it again with a token that already completed only restarts the service.

`--endpoint` defaults to `$SERVERCHIEF_ENDPOINT`, then `https://server.chief.app/agent/v1`. `--ca-file` (or `$SERVERCHIEF_CA_FILE`) adds a PEM CA to the trusted roots for a local development backend; its absolute path is stored in `identity.json`, so `serverchief run` trusts it too.

## The daemon

```bash
systemctl status serverchief                                  # systemd: runs as the serverchief user, logs to the journal
sudo serverchief run --log-file /var/log/serverchief/agent.log  # SysV: size-capped file log
serverchief run --config ./config.json --state-dir ./state      # development run with local state
```

Without `identity.json` the daemon exits with "not enrolled, run `serverchief setup <token>`". Otherwise it:

- heartbeats right away and then every `intervals.heartbeat` seconds (±10%), reporting the hashes and seq the backend acknowledged. Datasets listed in `resync` are sent in full at once, and a changed `config_etag` triggers `GET /config` with `If-None-Match`.
- collects each dataset the backend enabled on its backend-provided interval and batches the due, changed datasets into one envelope with a persisted, increasing `seq`. `packages`, `vhosts`, `certificates` and `applications` are sent as deltas against the last acknowledged payload once the backend has one. Collector failures go in the envelope's `errors` map whenever they change. Hashes advance only for datasets listed in `accepted` with the hash that was sent. Datasets the backend `ignored` are not sent again until its configuration changes.
- subscribes to `private-agent.server_<id>.generation_<n>` when the configuration has a `realtime` block, authorising the channel with its credential. `config.updated` fetches the configuration and `commands.available` fetches the commands. Without `realtime` it polls only.
- runs backend commands. When a heartbeat answers `commands_available: true` or `commands.available` arrives, it calls `GET /commands`, acknowledges each new command and then executes it: `refresh_inventory` collects the listed (or all enabled) datasets and pushes what changed, `resync` sends them in full, `refresh_config` fetches `GET /config`, `update_agent` hands an agent update to the update helper (see [Agent updates](remote-actions.md#agent-updates)), and `reboot`, `upgrade_packages` and `disable_capability` are remote actions (see [Remote actions](remote-actions.md)). Unknown kinds are acknowledged and reported `failed` with `unsupported command kind "<kind>"`. The result (`succeeded`, or `failed` with the error) is posted once the push or config fetch finishes. Acknowledgements and results are retried with the error table's backoff, and a journal of recent commands makes sure a restart neither executes a command twice nor loses its result: a command that was executing when the agent stopped is reported `failed`.
- follows the [protocol's error table](protocol/README.md): `401 revoked`/`unauthenticated` stops every loop and exits with status 2, which the systemd unit does not restart (`RestartPreventExitStatus=2`). `426` pauses inventory and heartbeats hourly. `429` and `503` honour `Retry-After`. `400`, `404`, `413` and `422` are logged and not retried unchanged. Other `5xx` and network errors back off exponentially (2 s doubling to 5 min, with jitter).

`SIGINT` and `SIGTERM` stop the daemon gracefully. `SIGHUP` (`systemctl reload serverchief`) reloads the local configuration, reopens the log file and collects every dataset again.

### Local configuration

Every key in `/etc/serverchief/config.json` is optional; the [README](../README.md#configuration) lists them. These operator overrides win over the backend's intervals and `package_metadata_refresh`, and `datasets` can only narrow what the backend enabled. `SIGHUP` applies a change.

### State

State lives in `/var/lib/serverchief`: `state.json` (seq, acknowledged seq and hashes, last reported errors), `server-config.json` (the backend configuration with its ETag), `payloads/` (delta bases), `metadata-refresh.json` (the last successful package metadata refresh) `commands.json` (recent commands with their state and result, kept 48 hours after their result was reported), `helper/` (root helper requests and results) and, while a remote action runs, its request, state and result files: `update-*.json` (see [Agent updates](remote-actions.md#agent-updates)), `upgrade-*.json` with `upgrade-output.log` (see [Package updates](remote-actions.md#package-updates)) and `reboot-request.json`. Every file is mode 0600 and replaced atomically (temporary file, fsync, rename).

## Metrics

When the backend's configuration turns metrics on (`metrics: {enabled, endpoint, interval}`, protocol 1.1.0), the daemon also samples host usage every interval (60 s by default) and delivers it to the metrics gateway, following the Server Chief metrics protocol (v1). Sampling runs next to the inventory loop, so a slow collection never delays it.

- **What:** CPU time shares, load, processes, memory, swap, per-file-system usage and inodes, per-disk throughput, IOPS and utilisation, per-interface traffic, packets, errors and drops, TCP connections and uptime, all from `/proc` and `statfs`; nothing needs root. Rates cover the interval before the sample, and a counter that went backwards (a reboot, a recreated interface) leaves its rates out once. Disks, file systems and interfaces follow the inventory's exclusions (no loop, ram or zram devices, empty optical drives, bind mounts of the same device, or container plumbing such as `veth*`, `docker*` and `br-*`), with at most 64 of each. Disk and network metrics also come as host totals (empty instance) over the devices that count, so a byte is counted once: disks without another block device beneath them (not LVM or md RAID) and interfaces backed by a network device (not bonds, bridges, VLANs or tunnels; in a container, those not stacked on another).
- **When:** samples are taken on interval boundaries (`at % interval == 0`), so a fleet lines up, and each agent sends a random 0–30 s after the boundary, so a fleet does not arrive at once.
- **Spool:** every sample is a file in `/var/lib/serverchief/metrics/samples` until the gateway stored it. Up to 60 samples go out as one batch, which is kept as `batch.json` and resent unchanged after an ambiguous failure, so the gateway stores it once. The spool keeps at most 24 hours and 8 MiB, dropping the oldest samples first.
- **Failures:** a `401` waits 5 minutes (a just-enrolled server the panel has not published yet), `403 metrics_disabled` drops the spool and waits for a new configuration, `426` waits for a new configuration, `429` and `503` honour `Retry-After`, and other errors back off from 2 s to 5 minutes. A rejected batch (`400`, `413`, `415`, `422`) is dropped.
- **Live:** the realtime event `metrics.live {until}` asks for live samples while someone watches the server's charts. The agent then reads usage every second (without file systems, so a slow network mount cannot hold it up) and posts two samples at a time to the gateway's `/v1/live`, until `until` (at most 5 minutes ahead; the panel repeats the event) or until the gateway reported no watching browser for 10 seconds. Live samples are never stored, spooled or retried; `401` and `403` end them until the next event.
- **Heartbeat:** `capabilities.metrics {enabled, last_sent_at, spool_bytes, error, live}` lets the panel show when samples stop arriving and whether it can ask for live samples. Turning metrics off clears the spool.

## Privileges

The packaged daemon runs as the `serverchief` system user, not as root. `serverchief.service` grants it only `CAP_DAC_READ_SEARCH`, so it can read package databases, web server and panel configuration and certificates, but cannot write anything outside `/var/lib/serverchief` and `/var/log/serverchief` (`ProtectSystem=strict`, `NoNewPrivileges`). `identity.json` is `root:serverchief 0640`.

Where `/proc` is mounted with `hidepid` (CloudLinux, hardened hosts), the package adds the `serverchief` user to the group the mount's `gid=` option names, such as CloudLinux's `clsupergid`, so it sees every process as it does on other hosts; `needs-restarting -r` reads the boot time from `/proc/1`. The command lines of other users' processes become readable to it, while their environment and memory stay private. Without a `gid=` option the RHEL reboot flag is reported as unknown.

The few things that need root go through the root helper: the daemon writes a request to `/var/lib/serverchief/helper/`, `serverchief-helper.path` starts `serverchief-helper.service` (`serverchief internal-helper`, root, oneshot), and the helper writes the result back. It knows a fixed list of actions without arguments: `ping`, `refresh-metadata` (the package metadata refresh), `plesk-sites` (reads the Plesk sites; the helper builds the queries), `apache-vhosts`, `disk-health` (reads the SMART data of the physical disks with `smartctl`, skipping virtual machines and containers; never starts a self-test), `start-agent-update`, `start-package-upgrade`, `reboot` (schedules a reboot in one minute) and `disable-capability`. The helper checks the operator's opt-out itself before starting a package upgrade or a reboot. It opens requests without following symbolic links, only accepts small files of the directory's owner and creates results as new files.

Where systemd cannot grant ambient capabilities (before 229, such as CentOS 7) the package installs `/etc/systemd/system/serverchief.service` from `/usr/share/serverchief/serverchief-root.service`, and under SysV the init script runs the daemon as root: it then runs the same actions in-process, and `/var/lib/serverchief` stays owned by root so the update and package upgrade helpers trust the requests the daemon writes. An update from a version that ran as root (before 0.2.0) finishes as root and then switches to the `serverchief` user on its own (`serverchief internal-unprivilege`). Every heartbeat reports the mode and the available helper actions, and the backend records a change of mode.

Because the daemon can read every file, a symbolic link inside a hosting user's tree is only followed when it stays inside that tree, and files are read up to a size cap.

## Code layout

```
main.go          signal handling, embedded fallback CA roots
cmd/             cobra CLI: version, collect, sample, setup, run, disable, enable, capabilities, and the hidden
                 internal-helper, internal-update, internal-upgrade and internal-unprivilege commands
collector/       Collector interface, registry and the built-in collectors, plus the package system for the daemon's metadata refresh
pkglock/         read-only package-manager lock inspection (F_GETLK, PID files)
trust/           trusted roots: the system pool plus the embedded Mozilla bundle
helper/          the root helper: a fixed list of argument-free actions requested through files
optout/          the operator's opt-out of remote actions (disabled-capabilities.json)
updater/         agent self-updates: capability, request/state/result files, the internal-update helper
upgrader/        package upgrades for upgrade_packages: capability, request/state/result files, the internal-upgrade helper
execlog/         the output of what a command ran, for command results and live output
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
- **Delivery:** the daemon schedules each dataset on its own interval with ±10% jitter and uploads only the datasets whose hash differs from the one the backend acknowledged. A failed delivery leaves the state untouched; how it is retried follows the [protocol's error table](protocol/README.md). A `resync` (from a heartbeat, an inventory answer or a `resync` command) makes the agent forget that dataset's hash and payload so it goes out in full right away. If the backend answers with a higher `seq` than the agent sent (state lost or reinstalled), the agent continues from there and resends.
- **Init systems:** the package ships both `serverchief.service` and `/etc/init.d/serverchief` and depends on neither init system. The maintainer scripts check for `/run/systemd/system` at runtime. The SysV script uses `start-stop-daemon` when available and otherwise falls back to `setsid` with a pid file (RHEL-likes).
- **TLS:** old distributions ship stale CA bundles. The agent's HTTP client and the realtime websocket therefore trust the system roots plus the embedded Mozilla bundle from `golang.org/x/crypto/x509roots/fallback`, plus the `--ca-file` CA when one was given at setup.
- **Protocol:** every request carries `ServerChief-Protocol-Revision: 1.3.0`, `User-Agent: serverchief-agent/<version> (linux; <arch>)` (followed by ` server/<id>` once enrolled, on the realtime connection too) and, except `POST /enroll`, `Authorization: Bearer <credential>`. Bodies above 8 KiB are gzipped. Errors are decoded from `application/problem+json` into `agent.ProblemError`. The channel-auth request only ever goes to the enrolled endpoint's origin. The protocol types are in `agent/types.go` and the tests replay the [spec](protocol/README.md)'s fixtures from `docs/protocol/fixtures/`.
