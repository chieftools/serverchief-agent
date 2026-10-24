# Changelog

All notable changes to the Server Chief Agent are documented in this file.

Changes are grouped by release and type, with the newest release first. Release
dates are available from the linked GitHub releases. Add future changes to the
Unreleased section, then move them into a versioned section before tagging a
release.

## [Unreleased]

### Added

- Added inventory of the operating system, hardware, network, DNS resolvers, packages, package repositories, cloud metadata, virtual hosts and certificates. Each dataset is hashed, and only datasets that changed are reported to Server Chief, in full or as a delta. See [Datasets](README.md#datasets).
- Added `serverchief setup <token>` to enroll a server, and a daemon for systemd and SysV init that runs the commands Server Chief sends, such as refreshing the inventory or the configuration. See [Set up](README.md#set-up) and [Run the daemon](README.md#run-the-daemon).
- Added self-updates from the Server Chief package repository. Only the `serverchief` package is installed, and an update that does not report ready is rolled back. See [Agent updates](README.md#agent-updates).
- Added signed apt and yum/dnf repositories, with an `install.sh` that configures them under the names the updater expects. See [Releasing](README.md#releasing).

[Unreleased]: https://github.com/chieftools/serverchief-agent/commits/main
