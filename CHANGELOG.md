# Changelog

Notable changes to Server Chief agent are documented here.

## [Unreleased]

### Added

- Inventory of the operating system, hardware, network, DNS resolvers, packages, package repositories, cloud metadata, virtual hosts and certificates, reported to Server Chief as hashed full or delta datasets.
- Enrollment with `serverchief setup <token>`, a daemon for systemd and SysV init, and backend commands to refresh the inventory or configuration.
- Self-updates from the Server Chief package repository, restricted to the `serverchief` package, with readiness checks and rollback.
- Signed apt and yum/dnf repositories with an `install.sh` that configures them under the names the updater expects.
