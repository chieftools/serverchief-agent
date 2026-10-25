# Changelog

All notable changes to the Server Chief Agent are documented in this file.

Changes are grouped by release and type, with the newest release first. Release
dates are available from the linked GitHub releases. Add future changes to the
Unreleased section, then move them into a versioned section before tagging a
release.

## [Unreleased]

### Added

- Added host metrics. When Server Chief turns them on, the agent samples CPU, load, memory, swap, file systems, disks, network, TCP connections and uptime every minute and sends them to the Server Chief metrics gateway. Samples wait in a spool of up to 24 hours while the gateway cannot be reached. See [Metrics](README.md#metrics).
- Added `serverchief sample` to preview a metrics sample locally.
- Added live metrics. While someone watches a server's charts in Server Chief, the agent reads usage every second and sends it to the metrics gateway every 2 seconds, for at most 5 minutes per request and only while a browser is watching. Live samples are not stored, spooled or retried, and leave out file systems.

### Changed

- The agent no longer runs as root. Under systemd 229 and later it runs as the `serverchief` user with read-only access to the server (`CAP_DAC_READ_SEARCH`), and a root helper with a fixed list of actions refreshes package lists, reads Plesk sites and installs agent updates for it. Older systemd and SysV keep running the agent as root. Updating from 0.1 switches to the `serverchief` user once the update is done. See [Privileges](README.md#privileges).
- Every heartbeat reports how the agent runs, so Server Chief shows it and records a change of privileges. The protocol revision is 1.1.0.
- A symbolic link inside a hosting user's directory is only followed when it stays inside that directory, for every file the agent reads, and every file is read up to a size cap.

## [0.1.1]

### Changed

- Added the server ID to the agent's User-Agent once it is enrolled, as `serverchief-agent/<version> (linux; <arch>) server/<id>`, for both HTTP requests and the realtime connection. Logs and proxies in front of Server Chief can now tell which server sent a request.

## [0.1.0]

### Added

- Added inventory of the operating system, hardware, network, DNS resolvers, packages, package repositories, cloud metadata, virtual hosts and certificates. Each dataset is hashed, and only datasets that changed are reported to Server Chief, in full or as a delta. See [Datasets](README.md#datasets).
- Added `serverchief setup <token>` to enroll a server, and a daemon for systemd and SysV init that runs the commands Server Chief sends, such as refreshing the inventory or the configuration. See [Set up](README.md#set-up) and [Run the daemon](README.md#run-the-daemon).
- Added self-updates from the Server Chief package repository. Only the `serverchief` package is installed, and an update that does not report ready is rolled back. See [Agent updates](README.md#agent-updates).
- Added signed apt and yum/dnf repositories, with an `install.sh` that configures them under the names the updater expects. See [Releasing](README.md#releasing).

[Unreleased]: https://github.com/chieftools/serverchief-agent/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/chieftools/serverchief-agent/releases/tag/v0.1.1
[0.1.0]: https://github.com/chieftools/serverchief-agent/releases/tag/v0.1.0
