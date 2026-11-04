# Changelog

All notable changes to the Server Chief Agent are documented in this file.

Changes are grouped by release and type, with the newest release first. Release
dates are available from the linked GitHub releases. Add future changes to the
Unreleased section, then move them into a versioned section before tagging a
release.

## [Unreleased]

### Added

- Server Chief can reboot the server from the dashboard with the new `reboot` command. The agent waits for a running package manager to finish, schedules the reboot a minute ahead with `shutdown -r +1` so logged-in users are warned, and reports the result with the new boot time after the server is back. Application containers such as Docker cannot be rebooted and report why.
- Server Chief can install package updates from the dashboard with the new `upgrade_packages` command: the security updates or all pending updates, never the `serverchief` package itself, keeping local configuration files. When the dashboard asks for it, the agent first updates itself the usual way, with its readiness check and rollback. Services that use updated libraries can be restarted afterwards (needrestart on Debian and Ubuntu, `needs-restarting -s` on RHEL and derivatives), so most updates take effect without a reboot, and the result says whether a reboot is still required. The upgrade runs in its own `serverchief-upgrade` unit, survives the agent restarting and reports what it ran. Its output streams to the dashboard while it runs, step by step, so an update can be followed live.
- A reboot can be asked to happen only when the server needs one (`if_required`): the agent checks right before rebooting and reports when no reboot was needed, or when it could not tell, instead of rebooting.
- Reboots and package updates can carry a deadline (`not_after`): the agent does not start them later than that, also not after waiting for a running package manager, and reports them as too late instead.
- `serverchief disable`, `serverchief enable` and `serverchief capabilities` turn remote actions (`reboot` and `upgrade_packages`) off and on again on the server itself. The setting is stored in `/etc/serverchief/disabled-capabilities.json`, and the dashboard shows a turned-off action as such.
- The dashboard can turn reboots and package updates off on a server with the new `disable_capability` command, so nobody has to log in to do it. Only `serverchief enable` on the server turns them on again.
- Command results include the output of what the command ran, so the dashboard can show it.
- The Debian package installs `/etc/needrestart/conf.d/serverchief.conf`, so needrestart never restarts the agent.
- The agent speaks revision 1.2.0 of the agent protocol.

### Fixed

- Fixed the agent collecting the sites, certificates and applications of busy Plesk servers every minute. Every request writes a site's access log under `/var/www/vhosts/system`, which the agent took for a change of the web server configuration, so it ran `apachectl -S` several times a minute. Only the generated configuration in each domain's `conf` directory counts now, without the files of Plesk's daily statistics run.
- One inventory round runs `apachectl -S` and the Plesk queries once instead of once for each of the sites, certificates and applications datasets.
- A source that keeps changing is collected at most once every 5 minutes. A first change is still collected right away.
- Fixed configuration changes of some Plesk domains going unnoticed until the next scheduled collection on servers with many domains, because their logs counted towards the limit of entries checked for changes.
- Fixed one application being listed twice when its sites use document roots linked to the same directory, such as DirectAdmin's `private_html` linked to `public_html`. These sites are now one application, reported at the directory itself rather than at the link.

## [0.3.1]

### Fixed

- Fixed CentOS 7 and other EL7 servers failing to read the Server Chief RPM repository with "Parsing primary.xml error: Start tag expected". The repository metadata was compressed with zstd, which the yum of EL7 cannot read, and is gzip compressed again from the next release on.

## [0.3.0]

### Added

- The `applications` dataset lists the web applications the sites serve: WordPress with its plugins and themes, Laravel, Symfony and other Composer or npm projects, with the packages of their lockfiles. Only the application's own lockfiles and the version headers WordPress reads are read; configuration files such as `wp-config.php` and `.env` are never opened. The backend enables it.

## [0.2.8]

### Changed

- Updated dependencies and Go to 1.27.3.

### Fixed

- `install.sh` on Ubuntu no longer stops at needrestart's "Pending kernel upgrade" dialog, which cannot be answered when the script is piped into bash. The installer now skips needrestart, as the agent's updater already did.
- The agent reconnects to realtime when its channel subscription fails, is rejected or is not confirmed within 30 seconds, and when the connection goes silent for longer than the activity timeout plus 30 seconds. Before, a rejected or unconfirmed subscription left the agent connected but unsubscribed, so it only picked up commands and config changes on its heartbeat, and a dead connection could go unnoticed for about 15 minutes. Reconnects now back off from 2 seconds up to 5 minutes until a subscription succeeds, instead of starting over at 2 seconds on every connection, so a channel authorization that keeps failing is no longer retried every few seconds.

## [0.2.7]

### Fixed

- TransIP VPSs that use TransIP's IPv6 resolvers, such as older CentOS 7 images without TransIP's package mirror, are recognised as TransIP. Only the IPv4 resolvers counted as a TransIP hint, so these reported no cloud provider.

## [0.2.6]

### Added

- TransIP VPSs from before cloud-init, which carry no seed URL, are recognised too. When a QEMU guest without a known provider uses TransIP's apt mirror or resolvers, the agent asks TransIP's NoCloud seed once and reports TransIP only when it answers with this instance.

## [0.2.5]

### Added

- TransIP VPSs report TransIP as their cloud provider with their data centre (`ams0` or `rtm0`) as region and zone, read from the NoCloud meta-data TransIP serves each VPS at `pxe.frontend.transip.nl`. The agent recognises TransIP by the seed URL in the DMI serial or, as the unprivileged user that cannot read the serial, in the datasource cloud-init recorded on first boot.

## [0.2.4]

### Fixed

- A site whose HTTPS redirect is listed before its redirect to the preferred `www.` name, as Ploi writes them, is reported once. The HTTPS redirect was reported as a second item with the site's name, which replaced the site in Server Chief.

## [0.2.3]

### Changed

- Where `/proc` is mounted with `hidepid`, such as on CloudLinux, the package adds the `serverchief` user to the group its `gid=` option names, so the agent sees every process as it does on other hosts and `needs-restarting -r` can tell whether a reboot is required.

### Fixed

- RHEL-family servers no longer report a required reboot when `needs-restarting -r` fails instead of answering. On CloudLinux, which mounts `/proc` with `hidepid`, the tool cannot read the boot time as the unprivileged agent and crashes with the same exit status as a required reboot; the reboot flag is now reported as unknown.

## [0.2.2]

### Added

- Sites report whether plain HTTP redirects to HTTPS (`https_redirect`), and a site that only redirects elsewhere reports where to (`redirect`: status, target and host).
- Sites report whether they offer HTTP/2 (`http2`) and HTTP/3 over a QUIC listener (`http3`), from nginx's `http2`, `http3` and `listen … quic` and Apache's `Protocols`.
- Certificates Laravel Forge renews are reported as managed by `forge`, from the renewal jobs Forge adds to `/etc/cron.d`.
- Sites on servers without a hosting panel report the user whose home directory holds the document root as their owner, such as the site user of Forge and Ploi.

### Changed

- Server blocks and virtual hosts that only redirect are folded into the site they point to instead of being listed as sites of their own. Forge, Ploi, certbot and Plesk add an HTTPS redirect and often a `www.` redirect to every site; these now add a port or an alias to that site. nginx's `.example.com` names are reported as `example.com` and `*.example.com`.
- A site that serves both `www.<name>` and `<name>` is reported as `<name>`, whichever its web server configuration lists first, so it matches its panel's entry. Plesk's redirect to a preferred `www.` domain folds into that site too.
- certbot renewal is also found in the scripts `run-parts` runs from `/etc/cron.hourly`, `daily`, `weekly` and `monthly`, and in jobs that run the `certbot-auto` or `letsencrypt-auto` wrappers or a scheduled `certonly`. Such certificates were reported without renewal automation.
- Plesk's `ipv4.`, `ipv6.` and preview names, and the `www.` name Plesk and DirectAdmin add to subdomains, are no longer reported as aliases unless the site's certificate covers them, so they no longer count as names the certificate misses.

## [0.2.1]

### Fixed

- Apache sites are listed again on servers whose Apache configuration opens files for writing while it is read, such as ModSecurity's `SecAuditLog`. The unprivileged agent cannot run `apachectl -S` there, so it asks the root helper, which has a new `apache-vhosts` action.

## [0.2.0]

### Added

- Added host metrics. When Server Chief turns them on, the agent samples CPU, load, memory, swap, file systems, disks, network, TCP connections and uptime every minute and sends them to the Server Chief metrics gateway. Samples wait in a spool of up to 24 hours while the gateway cannot be reached. See [Metrics](README.md#metrics).
- Added `serverchief sample` to preview a metrics sample locally.
- Added live metrics. While someone watches a server's charts in Server Chief, the agent reads usage every second and sends it to the metrics gateway every 2 seconds, for at most 5 minutes per request and only while a browser is watching. Live samples are not stored, spooled or retried, and leave out file systems.
- File system metrics count each file system once, by its device number, also when it has no device path, such as a ZFS dataset mounted in two places. Of several mounts, the one of the whole file system is reported rather than a bind mount of a directory in it.
- The `system` dataset reports whether a container has lxcfs (`virtualization.lxcfs`). Without it, a container's CPU, load and memory metrics are those of its host, which Server Chief now points out.
- The `system` dataset reports PHP builds no package manager owns, such as DirectAdmin custombuilds in `/usr/local/php82` and source builds under `/opt`, with their exact version (`php_builds`). The version is read from each build's `php-config`; no binary is run.

### Changed

- The agent no longer runs as root. Under systemd 229 and later it runs as the `serverchief` user with read-only access to the server (`CAP_DAC_READ_SEARCH`), and a root helper with a fixed list of actions refreshes package lists, reads Plesk sites and installs agent updates for it. Older systemd and SysV keep running the agent as root. Updating from 0.1 switches to the `serverchief` user once the update is done. See [Privileges](README.md#privileges).
- Every heartbeat reports how the agent runs, so Server Chief shows it and records a change of privileges. The protocol revision is 1.1.0.
- A symbolic link inside a hosting user's directory is only followed when it stays inside that directory, for every file the agent reads, and every file is read up to a size cap.
- `install.sh` falls back to HTTP for the apt repository when apt cannot reach it over HTTPS, as on Ubuntu 14.04, whose apt lacks the TLS ciphers of the package host. apt still checks the repository's signatures.

## [0.1.1]

### Changed

- Added the server ID to the agent's User-Agent once it is enrolled, as `serverchief-agent/<version> (linux; <arch>) server/<id>`, for both HTTP requests and the realtime connection. Logs and proxies in front of Server Chief can now tell which server sent a request.

## [0.1.0]

### Added

- Added inventory of the operating system, hardware, network, DNS resolvers, packages, package repositories, cloud metadata, virtual hosts and certificates. Each dataset is hashed, and only datasets that changed are reported to Server Chief, in full or as a delta. See [Datasets](README.md#datasets).
- Added `serverchief setup <token>` to enroll a server, and a daemon for systemd and SysV init that runs the commands Server Chief sends, such as refreshing the inventory or the configuration. See [Set up](README.md#set-up) and [Run the daemon](README.md#run-the-daemon).
- Added self-updates from the Server Chief package repository. Only the `serverchief` package is installed, and an update that does not report ready is rolled back. See [Agent updates](README.md#agent-updates).
- Added signed apt and yum/dnf repositories, with an `install.sh` that configures them under the names the updater expects. See [Releasing](README.md#releasing).

[Unreleased]: https://github.com/chieftools/serverchief-agent/compare/v0.3.1...HEAD
[0.3.1]: https://github.com/chieftools/serverchief-agent/releases/tag/v0.3.1
[0.3.0]: https://github.com/chieftools/serverchief-agent/releases/tag/v0.3.0
[0.2.8]: https://github.com/chieftools/serverchief-agent/releases/tag/v0.2.8
[0.2.7]: https://github.com/chieftools/serverchief-agent/releases/tag/v0.2.7
[0.2.6]: https://github.com/chieftools/serverchief-agent/releases/tag/v0.2.6
[0.2.5]: https://github.com/chieftools/serverchief-agent/releases/tag/v0.2.5
[0.2.4]: https://github.com/chieftools/serverchief-agent/releases/tag/v0.2.4
[0.2.3]: https://github.com/chieftools/serverchief-agent/releases/tag/v0.2.3
[0.2.2]: https://github.com/chieftools/serverchief-agent/releases/tag/v0.2.2
[0.2.1]: https://github.com/chieftools/serverchief-agent/releases/tag/v0.2.1
[0.2.0]: https://github.com/chieftools/serverchief-agent/releases/tag/v0.2.0
[0.1.1]: https://github.com/chieftools/serverchief-agent/releases/tag/v0.1.1
[0.1.0]: https://github.com/chieftools/serverchief-agent/releases/tag/v0.1.0
