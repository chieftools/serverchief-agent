# Server Chief Agent

The Server Chief Agent is a small, static Go binary that inventories a Linux server and reports it to [Server Chief](https://server.chief.app/): the operating system, hardware, network, installed packages and pending updates, websites, certificates and web applications, plus host metrics. Server Chief uses it to show what runs on your servers, what needs updating and which certificates are about to expire.

The agent only makes outbound HTTPS connections and never listens on a port. It only sends a dataset when its contents changed.

> [!NOTE]
> The agent is pre-1.0. Its configuration and command line may still change between minor releases. See [CHANGELOG.md](CHANGELOG.md) for what changed in each release.

## Installation

Install the package from the Server Chief repository, then run the setup command Server Chief shows for the server:

```bash
curl -fsSL https://pkg.server.chief.app/install.sh | sudo bash
sudo serverchief setup <token>
```

`install.sh` adds the signed apt or yum/dnf repository and installs the `serverchief` package; it does not connect the server. `setup` exchanges the one-time token for the server's own credential and starts the `serverchief` service.

Tarballs, `.deb` and `.rpm` packages with signed checksums are also attached to each [GitHub release](https://github.com/chieftools/serverchief-agent/releases). Only the repository install can update itself.

### Supported systems

The agent is a single static binary, so it does not depend on the host's C library. It needs Linux 3.2 or later.

| Distribution | Versions | Init system |
| --- | --- | --- |
| Ubuntu | 14.04, 16.04 and later | Upstart (14.04) via SysV scripts, systemd |
| Debian | 8 and later | systemd or SysV |
| CentOS / RHEL | 7 | systemd, or SysV in containers |
| AlmaLinux, Rocky Linux, CloudLinux | 8 and later | systemd |

Architectures: `amd64` (x86_64) and `arm64` (aarch64). CentOS 6 and other 2.6.x kernels are not supported.

### Uninstalling

Remove the `serverchief` package with `apt-get remove`, `dnf remove` or `yum remove`, then delete the server in Server Chief. `apt-get purge` also removes the agent's identity, state and logs; on RPM systems remove `/etc/serverchief`, `/var/lib/serverchief` and `/var/log/serverchief` yourself.

## What the agent collects

| Dataset | Contents |
| --- | --- |
| `system` | Hostname, operating system and version, kernel, architecture, virtualization, boot time, timezone and init system |
| `hardware` | CPU, memory and swap, disks with their partitions, and mounted file systems with their size |
| `network` | Network interfaces, their addresses and the default routes |
| `resolvers` | The DNS servers, search domains and options the server uses |
| `packages` | Installed packages with their version and the repository they came from |
| `repositories` | The configured apt or yum/dnf repositories and how they are signed |
| `updates` | Pending package updates, which of them are security updates, and whether a reboot is required |
| `cloud` | The cloud provider, region, instance type and public addresses, from the provider's metadata service |
| `vhosts` | Websites served by Apache, nginx, Plesk and DirectAdmin: names, ports, document root, PHP version and certificate |
| `certificates` | Certificates used by websites and mail servers, their expiry, chain and how they are renewed |
| `applications` | Web applications behind the websites (WordPress with its plugins and themes, Laravel, Symfony, Composer and npm projects) and the packages in their lockfiles |

Server Chief chooses which datasets to collect, and your team or a single server can turn the optional ones off. Payloads hold only values that stay the same while the server does, so the agent does not report usage or uptime as inventory; those are host metrics (CPU, load, memory, disks, network and connections), sampled every minute when metrics are turned on.

The agent never reads private keys, `wp-config.php`, `.env` files, apt's `auth.conf`, the credentials in repository URLs or cloud user data. [Datasets](docs/datasets.md) lists every source each dataset reads.

To see exactly what the agent would send, run a collection locally. It needs no setup, and prints JSON:

```bash
serverchief collect --pretty                         # the datasets Server Chief always collects
serverchief collect -d packages -d vhosts --pretty   # selected datasets, including optional ones
serverchief sample --pretty                          # one metrics sample
```

Some sources are only readable by root, so run them with `sudo` to see what the service sees.

## Security and privileges

- **Unprivileged service.** The `serverchief` service runs as the `serverchief` system user, not as root. Its systemd unit grants only `CAP_DAC_READ_SEARCH`, so it can read package databases, web server configuration and certificates, but cannot write anywhere outside its own state and log directories.
- **A small root helper.** The few things that need root (refreshing the package lists, reading Plesk's sites, and the remote actions below) go through a root helper that only knows a fixed list of actions and accepts no arguments.
- **Outbound only.** The agent talks to Server Chief over HTTPS with a credential it generated itself during setup. A websocket connection only tells it to check in; everything it does goes over HTTPS.
- **Hands off your packages.** The agent never installs, upgrades or removes packages, except for updating itself and installing updates when you ask for it from the dashboard. Tests pin every package-manager command it can run.

On systems whose systemd is too old to grant capabilities (before 229, such as CentOS 7) and under SysV, the service runs as root. [How the agent works](docs/architecture.md#privileges) has the details.

## Remote actions

From the dashboard, Server Chief can ask the agent to reboot the server (`reboot`) or to install pending package updates (`upgrade_packages`). Both are available by default, and both wait for a running package manager to finish first. A reboot is scheduled a minute ahead with `shutdown`, so logged-in users are warned. Package updates keep your modified configuration files and never touch the agent's own package.

Whoever runs the server can turn remote actions off on the server itself:

```bash
sudo serverchief disable reboot upgrade_packages   # turn remote actions off
sudo serverchief enable reboot                     # turn one on again
serverchief capabilities                           # list them with their state
```

The setting is stored in `/etc/serverchief/disabled-capabilities.json`. The dashboard shows a turned-off action as such, and can turn actions off for you, but never on again: only `serverchief enable` on the server does that. [Remote actions](docs/remote-actions.md) describes how each one runs.

## Agent updates

Server Chief keeps the agent up to date: when a new version is released, it asks agents installed from the repository to update themselves, in stages. The agent installs only its own package from the Server Chief repository, checks that the new version starts and reports in, and rolls back to the previous version otherwise. Automatic updates can be turned off for a team or a single server in Server Chief.

## Configuration

The agent is configured from Server Chief, and needs no configuration on the server. To override it locally, create `/etc/serverchief/config.json`. Every key is optional, local values win over Server Chief's, and `datasets` can only narrow what Server Chief enabled:

```json
{
  "intervals": { "system": "5m", "hardware": "15m" },
  "heartbeat_interval": "5m",
  "datasets": ["system", "hardware", "network", "resolvers"],
  "package_metadata_refresh": { "enabled": true, "interval": "6h" }
}
```

`package_metadata_refresh` controls how often the agent refreshes the package lists, so pending updates stay current (every 6 hours by default, at least every hour). Reload the service with `systemctl reload serverchief` after a change.

## Commands

| Command | Does |
| --- | --- |
| `serverchief setup <token>` | Connect this server to Server Chief, or reconnect it with a new token |
| `serverchief collect` | Collect inventory locally and print it as JSON |
| `serverchief sample` | Take one metrics sample locally and print it as JSON |
| `serverchief capabilities` | List the remote actions and whether they are turned off |
| `serverchief disable <action>…` | Turn remote actions off on this server |
| `serverchief enable <action>…` | Turn remote actions on again on this server |
| `serverchief run` | Run the agent in the foreground (the service does this) |
| `serverchief version` | Show the version |

## Files and logs

| Path | Contents |
| --- | --- |
| `/etc/serverchief/identity.json` | The server's identity and credential, written by `setup` |
| `/etc/serverchief/config.json` | Optional local overrides |
| `/etc/serverchief/disabled-capabilities.json` | Remote actions turned off on this server |
| `/var/lib/serverchief/` | State: what was last reported, the configuration from Server Chief, and pending commands |
| `/var/log/serverchief/` | Logs on SysV systems, and the logs of agent updates |

On systemd, the agent logs to the journal: `journalctl -u serverchief`.

## Documentation

- [How the agent works](docs/architecture.md): enrollment, the daemon, metrics, privileges and the code layout
- [Datasets](docs/datasets.md): every dataset, its sources and when it is collected
- [Remote actions and agent updates](docs/remote-actions.md): reboots, package updates and self-updates step by step
- [Agent protocol](docs/protocol/README.md): the HTTP protocol between the agent and Server Chief

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for how to build, test and release the agent. Report security vulnerabilities privately as described in [SECURITY.md](.github/SECURITY.md).

## License

The Server Chief Agent is licensed under the Apache License 2.0. See [LICENSE](LICENSE) for the license text.
