# Remote actions and agent updates

How the agent carries out what Server Chief asks of the server itself: rebooting it, installing package updates and updating the agent. The commands and their results are specified in the [agent protocol](protocol/README.md#commands); the [README](../README.md#remote-actions) covers turning remote actions off.

## The opt-out

Remote actions are available by default. Whoever runs the server can turn them off on the server itself with `serverchief disable` and on again with `serverchief enable`. The capabilities are `reboot` and `upgrade_packages`. Agent updates cannot be turned off this way; turn off automatic updates for the team or server instead. They are separate: installing package updates also brings the agent up to date first when the dashboard asks for it, so stopping every remote change to the agent takes both.

The setting lives in `/etc/serverchief/disabled-capabilities.json` (`{"version": 1, "disabled": [...]}`, `root:root 0644`), apart from `config.json`. The daemon only reads it. The one change the backend can ask for is turning a capability off: `disable_capability {"capability"}` has the root helper's `disable-capability` action add the name (`already_disabled` when it is off already, or when the file cannot be trusted, which leaves the file alone), and a heartbeat goes out right away. Nothing the backend sends turns a capability on again; only `serverchief enable` on the server does. A file that cannot be read, that does not belong to root, that its group or others can write, or that is not valid turns every remote action off. `disable` and `enable` need root and ask a running daemon to reload, so its next heartbeat reports the change right away; otherwise it is picked up within one heartbeat. A turned-off capability is left out of the heartbeat's `capabilities` and listed in `capabilities.disabled`, so the dashboard shows it as turned off on the server rather than unsupported. A command for it fails with code `disabled`, checked by the daemon and again by the root helper.

## Reboots

The heartbeat's `capabilities.reboot` is available when the root helper answers (or the daemon runs as root), the server is not an application container (Docker, Podman, Kubernetes, WSL; LXC, systemd-nspawn, OpenVZ and virtual machines can reboot) and `shutdown` exists (`systemctl reboot` on systemd without it). For `reboot {}` the daemon waits for package-manager locks (checking every 30 s, for up to 15 minutes, then `package_manager_busy`), journals the command as `handed_off`, writes `reboot-request.json` (command ID, boot ID from `/proc/sys/kernel/random/boot_id`, boot time) and has the root helper run `shutdown -r +1 "Server Chief: rebooting in 1 minute, requested from the panel"`. Its output goes to the dashboard right away as live output, sent again after a failure for up to the minute before the server goes down. On the next start a new boot ID reports `rebooted`; the same boot ID ten minutes after the request (a cancelled shutdown) reports `not_rebooted`. The result carries `data: {code, previous_boot_time, boot_time}` and the `output` of the shutdown command, followed by a `# ` line on when the server booted again, or which boot it still runs. Other codes are `disabled`, `unsupported` and `reboot_failed`; unknown arguments are rejected. A reboot also waits while a package upgrade runs.

### Only when required

`reboot {"if_required": true}` checks right before rebooting whether the server needs it, the way the `updates` dataset tells (`/run/reboot-required`, `needs-restarting -r`), and reports `not_required` instead of rebooting when it does not, or `required_unknown` when it cannot tell; it never reboots blindly. Without it, or with `false`, the server reboots regardless.

## Deadlines

`reboot` and `upgrade_packages` take an optional `not_after` (RFC 3339). The daemon does not start the command once its clock reaches it, also not while it waits for the package manager, and reports code `too_late`. Once a reboot is scheduled or an upgrade handed to its helper, the deadline no longer applies.

## Package updates

The heartbeat's `capabilities.upgrade_packages` is available when apt, dnf or yum exists, the root helper answers (or the daemon runs as root) and the helper can run detached (`systemd-run` on systemd, a double-forked `setsid` child under SysV). `upgrade_packages {"scope": "security" | "all", "restart_services": bool, "agent_version": "X.Y.Z" | null}` is validated strictly. The daemon waits for package-manager locks (up to an hour, then `package_manager_busy`), journals the command as `handed_off`, writes `upgrade-request.json` and has the root helper's `start-package-upgrade` (which checks the opt-out itself) start `serverchief internal-upgrade` as the transient unit `serverchief-upgrade`. The helper, as root and resumable from `upgrade-state.json`:

1. stops with `dpkg_interrupted` when `dpkg --audit` reports anything (it never runs `dpkg --configure -a`),
2. refreshes the package lists,
3. with `agent_version` newer than the installed agent, updates the agent first exactly like `update_agent` (Server Chief repository only, readiness, rollback) under the update ID `<command>-agent`, and continues whatever the outcome,
4. installs the scope's updates without the `serverchief` package: on apt `apt-get install -y --only-upgrade <pending updates of the scope>` (from the same `apt-get -s dist-upgrade` the `updates` dataset uses, so held and phased updates stay), on dnf/yum `upgrade -y --exclude=serverchief [--security]`; `security_unavailable` when the repositories publish no advisories. It runs non-interactively and keeps modified configuration files (`--force-confdef --force-confold`),
5. with `restart_services`, lets needrestart restart services on apt (`NEEDRESTART_MODE=a`; the package's `/etc/needrestart/conf.d/serverchief.conf` keeps the agent out), or restarts the units `needs-restarting -s` lists with `systemctl try-restart` on dnf/yum, never the agent's, D-Bus, systemd's own or login sessions; without it needrestart is suspended,
6. lists the pending updates again for the counts and the reboot flag and writes `upgrade-result.json`.

The daemon reports the result as soon as it sees it, or on its next start, with `data: {code, scope, upgraded, held_back, reboot_required, services_restarted, agent_update}` and the helper's output, then removes the files and collects `packages` and `updates`. Codes are `upgraded` and `nothing_to_upgrade` (succeeded), and `upgrade_failed`, `dpkg_interrupted`, `security_unavailable`, `package_manager_busy`, `too_late`, `disabled` and `unsupported` (failed).

### Live output

The helper appends its output to `upgrade-output.log` as it runs (private to the agent's user, started empty for each new request and kept when it resumes), with a `# ` line at each step: refreshing the package lists, updating the agent, `Installing N security updates`, restarting services. Programs' output goes there as they print it, not when they finish. While the upgrade is handed off, the daemon checks for new output every 250 ms and sends it with `POST /commands/{id}/output`, one request at a time: output that arrives meanwhile goes in the next chunk, a backlog goes out chunk after chunk, and nothing is sent while the upgrade prints nothing (`{offset, length, output}`: raw byte offsets, the text cut at a character boundary with invalid bytes replaced, at most 64 KiB), continues from the backend's `expected_offset` after a `409 offset_mismatch`, stops for that command on another rejection and retries failures with backoff. How far it got is kept in `upgrade-stream.json`, so a restarted daemon, for example after the agent updated itself, continues instead of sending everything again. Before reporting the result it sends what is left; the result still carries the end of the output (at most 1 MiB), read from the same file.

## Agent updates

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
