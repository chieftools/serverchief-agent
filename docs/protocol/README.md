# Server Chief agent protocol v1

Revision **1.3.0**. This document is the contract between the Server Chief control plane and the `serverchief` agent in this repository. Both sides test against the fixtures in [`fixtures/`](fixtures/); the agent's tests replay them from here, and the control plane keeps a copy.

Revision 1.0.0 was refined in place until the first production deploy. Since then, additive changes bump the minor revision and incompatible changes need `/agent/v2`. Both sides accept every revision of the same major.

**Changes in 1.1.0** (agent 0.2.0): the `system` dataset's `virtualization` gains `lxcfs` and the dataset gains `php_builds`; the heartbeat's `capabilities.privilege` reports how the agent runs (see *Privileges*), and `GET /config` lists the datasets enabled for the server, which its team or the server itself may turn off. `GET /config` also tells the agent whether and where to send host metrics (`metrics`), and the heartbeat's `capabilities.metrics` reports their delivery (see *Metrics*). The realtime event `metrics.live` asks for live samples while someone watches (see *Realtime*).

**Changes in 1.2.0** (agent 0.4.0): the `reboot` command reboots the server, and the heartbeat's `capabilities.reboot` tells whether the agent can (see *Reboots*). The server's operator can turn remote actions off on the server itself, and `capabilities.disabled` lists what they turned off (see *Operator opt-out*). Running `upgrade_packages` commands stream their output while they run (see *Live output*). The `upgrade_packages` command installs package updates, and `capabilities.upgrade_packages` tells whether the agent can (see *Package updates*). A command result can carry the `output` of what the command ran, which the panel keeps as its execution log (see *Commands*). The root helper gains the `reboot` and `start-package-upgrade` actions.

**Changes in 1.3.0** (agent 0.5.0): a `reboot` sends the output of scheduling it as live output right away, instead of only with its result after the server is back, and the result's `output` ends with a `# ` line on how the reboot went (see *Live output* and *Reboots*).

The design follows the Backup Chief agent protocol:

- enrollment with a one-time token, exchanged for a credential the agent generates itself
- a generation counter that fences off older enrollments
- realtime messages that only wake the agent up, while all work happens over HTTPS

## Transport

- Base URL: `https://server.chief.app/agent/v1`. Locally it is `https://server.chief.test/agent/v1`, and the agent stores the endpoint it enrolled with.
- The agent only makes outbound HTTPS requests. It never listens on a port.
- Request bodies are JSON (`Content-Type: application/json`). The agent may send `Content-Encoding: gzip`, and the server must accept it. The body limit is 16 MiB after decompression.
- Every request carries these headers:
  - `ServerChief-Protocol-Revision: 1.0.0`
  - `User-Agent: serverchief-agent/<version> (linux; <arch>)`, followed by ` server/<server_id>` once the agent is enrolled (`POST /enroll` has none). The realtime connection sends the same value. The suffix is informational, for logs and proxies; the server identifies agents by their credential only. Agents before 0.1.1 leave it out.
  - `Authorization: Bearer <credential>` on every endpoint except `POST /enroll`
- Errors are `application/problem+json` (`fixtures/problem.json`):
  - Fields: `type` (a short slug, not a URL), `title`, `status`, `detail`, plus optional `retry_after` in seconds and `errors` (validation, field to messages).
  - Status codes the agent must handle:

    | Status | `type` | Agent behaviour |
    |---|---|---|
    | 400 | `invalid_request` | Log the error; don't retry this body unchanged. |
    | 401 | `unauthenticated` or `revoked` | Stop all loops and log "re-run `serverchief setup`". Don't hammer the server. |
    | 403 | `limit_reached` | Enrollment only; the team is at its server limit. Log it and exit `setup` non-zero. |
    | 404 | `not_found` | Unknown endpoint; log it. |
    | 409 | `enrollment_conflict` | Enrollment only; see below. |
    | 413 | `payload_too_large` | Log the error; don't retry this body unchanged. |
    | 422 | `validation_failed` | Log the error; don't retry this body unchanged. |
    | 426 | `unsupported_revision` | Log it and keep heartbeating once an hour; an update can fix it. |
    | 429 or 503 | `rate_limited` / `temporarily_unavailable` | Honour `Retry-After` / `retry_after`. |
    | Other 5xx and network errors | `server_error` | Exponential backoff with jitter: 2 s doubling to a 5 min cap. |

  - Every agent response, problems included, carries `ServerChief-Protocol-Revision` with the server's revision. Agents must not branch on `title` or `detail`.

## Identity and enrollment

1. A team member creates a token. There are two kinds, and the agent can't tell them apart:
   - A **provisioning token** belongs to a team. "Add servers" in the UI creates one; the server is created when an agent enrolls with it, named after the reported hostname. It adds one server (single use) or any number of servers until it expires. It is valid for at most 24 hours and can be revoked; servers already added keep working.
   - An **enrollment token** belongs to one server. The API creates a `servers` row (status `pending`) with one, and "Reinstall agent" issues one for an existing server (see re-enrollment). It is valid for 1 hour and can be used once.
   - Format of both: `scenr_` followed by 40 random base62 characters (the server generates 34 random characters plus a 6 character base62 checksum). Tokens are looked up by hash, so neither side may reject a token on its exact length; the fixture token is illustrative and one character short. Only the SHA-256 hash is stored.
   - The UI shows the install command:
     ```
     curl -fsSL https://pkg.server.chief.app/install.sh | sudo bash && sudo serverchief setup scenr_…
     ```
2. `serverchief setup <token>` (as root):
   - The agent generates a ULID `attempt_id` and a credential: `scagt_` followed by 43 base64url characters (32 random bytes).
   - It persists both to `/etc/serverchief/setup.json` (mode 0600) **before** calling the server, so a crash or retry reuses them.
3. `POST /enroll` (no Authorization header). Request: `fixtures/enroll-request.json`.
   - `200` returns `fixtures/enroll-response.json`. The agent writes `/etc/serverchief/identity.json` with endpoint, server_id, generation and credential (`root:serverchief 0640` so the unprivileged daemon can read it, `0600` without that group), deletes `setup.json`, then enables and starts the service.
   - The server stores only `sha256(credential)` and its first 12 characters (for display). It marks the token consumed, sets the server to `active` and records the hostname, platform and agent version.
   - A provisioning token first creates the server, then enrolls it the same way. It records the `attempt_id` per server it added, and counts as used once per server.
   - **Idempotent replay:** if the same token comes back with the same `attempt_id` and the same credential hash within 10 minutes of consumption, return the same 200 response. For a provisioning token this returns the server that attempt added, so a retry never adds a second server.
   - An enrollment token with a different `attempt_id` or credential returns `409 enrollment_conflict`. For a provisioning token, the same `attempt_id` with a different credential returns `409`; a different `attempt_id` adds another server while the token has uses left.
   - A token that is expired, revoked, unknown, used up or consumed outside the replay window returns `401 unauthenticated` with detail "Enrollment token is invalid or expired".
   - If the team is over its `servers` limit, return `403 limit_reached`. Pending servers don't count; servers become active only at enrollment, and the limit check happens under a row lock.
4. **Re-enrollment:** a new token for an existing server (the "Reinstall agent" action in the UI) targets the next generation. When it is exchanged, the server increments `generation` and replaces the credential hash. The old credential is recorded in `agent_revocations` and answers `401 revoked` from then on.
   - Until the new token is exchanged, the running agent keeps working. Issuing a token invalidates any earlier unused token of that server.
   - The server forgets `last_inventory_seq` on (re-)enrollment, so a fresh install may start again at seq 1. The stored datasets are kept; the first heartbeat's `resync` tells the new agent what to send.
   - The agent includes `previous_server_id` and `previous_generation` when an `identity.json` already exists. These are informational only.
5. **Removing a server** in the UI revokes its current credential (`401 revoked`).

Authentication middleware:

- Look up the server by `sha256(bearer)`.
- Reject with `401 unauthenticated` if there is no match and `401 revoked` if the hash is in revocations.
- Record contact by setting `last_contact_at` (throttled to one write per 60 s), plus `agent_version` from the User-Agent header.
- Record the request's source address as `observed_ip` (with the same throttle), honouring the app's trusted-proxy configuration. Servers behind NAT often have no public address in `network`, so the backend uses this address for location and provider enrichment.

## Heartbeat — `POST /heartbeat`

- **Timing:** sent every `intervals.heartbeat` seconds (default 300, ±10% jitter). The first one goes out right after startup.
- **Request** (`fixtures/heartbeat-request.json`):
  - `agent_version`, `started_at`, `sent_at`
  - `hashes`: dataset name → hash of the **last payload the server acknowledged** for that dataset
  - `seq`: the last acknowledged inventory seq
  - `capabilities`: `{ "agent_update": { "available", "reason" }, "metrics": { "enabled", "last_sent_at", "spool_bytes", "error" }, "privilege": { "mode", "helper_actions", "reason" }, "reboot": { "available", "reason" }, "upgrade_packages": { "available", "reason" }, "disabled": [names] }` (see *Agent updates*, *Metrics*, *Privileges*, *Reboots*, *Package updates* and *Operator opt-out*). It is optional, and treated as unavailable when absent; `metrics` and `privilege` are absent before 1.1.0, `reboot`, `upgrade_packages` and `disabled` before 1.2.0. `reboot` and `upgrade_packages` are also absent when the operator turned them off, `disabled` then lists them.
- **Response** (`fixtures/heartbeat-response.json`):
  - `resync`: enabled datasets whose acknowledged hash differs from what the server stores, that the server has never received, or that are missing from `hashes`. The agent must send these as `mode: full` on its next inventory push, right away. The list is sorted by name.
  - `config_etag`: when it differs from the agent's cached etag, the agent fetches `GET /config`.
  - `commands_available`: `true` when the server has pending commands for this agent. The agent then calls `GET /commands` right away (see *Commands*).
- **Server status:**
  - `online` (stored as `active`) while `last_contact_at` is less than 15 minutes old. That allows three missed heartbeats. Any authenticated request counts as contact.
  - `offline` after that. A scheduled check runs every minute, flips the status and notifies team members through the existing Offline / BackOnline notifications.

## Inventory — `POST /inventory`

The request is the envelope the agent already produces (`inventory.Envelope`); see `fixtures/inventory-request.json`, which is real `serverchief collect` output from Ubuntu 16.04.

```json
{
  "seq": 42,
  "collected_at": "2026-10-22T14:00:00Z",
  "datasets": {
    "<name>": { "hash": "<sha256 hex of canonical JSON payload>", "mode": "full", "payload": { } },
    "<name>": { "hash": "<new hash>", "mode": "delta", "base_hash": "<hash the delta applies to>", "payload": { "added": [], "removed": [], "changed": [] } }
  },
  "errors": { "<name>": "collector error message" }
}
```

**Sequence numbers**

- `seq` increases monotonically and is persisted by the agent across restarts, in `/var/lib/serverchief/state.json`.
- If `seq` is less than or equal to the stored `last_inventory_seq`, the server must **not** reapply the envelope. It returns 200 with the currently stored hashes of all datasets (`accepted`), empty `resync`/`ignored` and the stored `seq`, which makes replay idempotent.
  - If the response `seq` is greater than the seq the agent sent, the agent lost its state; it should continue from response `seq` + 1 so its next envelope is applied.

**Processing each dataset**

- `mode: full`: replace the stored payload.
- `mode: delta`:
  - Apply only when `base_hash` equals the stored hash. Otherwise skip the dataset and add it to `resync`.
  - Delta datasets are keyed lists. The stored payload is the dataset's full payload object; the delta (`inventory.Delta`: `added` items, `removed` keys, `changed` items) applies to its `items` list only. Other top-level keys (for `packages`: `manager`) are kept from the base. When one of them changes, the agent sends the dataset in full instead of a delta.
  - Item keys are defined per dataset in the table below. An item's key is its key fields joined with `/`, where `manager` comes from the payload's top level (for example `dpkg/nginx/amd64`), and `removed` carries these key strings.
  - The applied list is sorted by key in byte order, exactly like the agent's `inventory.Apply`, so full payloads of delta datasets must be sorted the same way for the hashes to match.
  - A delta that removes or changes a missing key, or adds an existing key, is skipped and the dataset is added to `resync`. So is a delta for a dataset that is not a delta dataset.
- **Hash check after applying:**
  - The server recomputes the hash with the same canonical encoding as the agent: JSON with keys sorted recursively, no insignificant whitespace, UTF-8, numbers as emitted. It compares the result with `hash`.
  - On a mismatch it keeps the payload, stores the server-side hash and adds the dataset to `resync`. This self-heals.
  - Recomputing for full payloads is optional (they are trusted), but required for delta. This server does not recompute full payloads: a full payload whose hash PHP cannot reproduce would otherwise be resynced forever.
  - PHP re-encodes floats from their parsed value (`1e-07` becomes `1.0e-7`), so delta datasets should avoid floats; a mismatch self-heals through a full resync.
  - Matching Go's encoding from PHP:
    - Decode **preserving objects**: `{}` must stay `{}` and must not become `[]`.
    - Sort object keys recursively by byte order. List order is significant and must be kept.
    - Encode with `JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_PRESERVE_ZERO_FRACTION`.
    - Then escape U+2028/U+2029 as `\u2028`/`\u2029`, the way Go does. PHP's `json_encode` already does this unless `JSON_UNESCAPED_LINE_TERMINATORS` is set.
    - `fixtures/inventory-request.json` hashes must reproduce exactly; this is verified against Python `json.dumps(sort_keys=True, separators=(',', ':'), ensure_ascii=False)`.
- Unknown dataset names are ignored, and listed in `ignored` in the response. Datasets the server has not enabled for this server count as unknown, including those its team or the server turned off.
- **Processing failures:** a dataset the server accepted but cannot derive its tables from (a value a column cannot hold, a bug) is still `accepted`, so the agent does not resend it forever. Each derived step runs on its own: only that dataset's tables keep their previous state, the other datasets are processed. The failure is reported, stored as `process_error` / `process_error_at` on the dataset and shown on the server, and the stored payload is processed again every hour (`servers:reprocess-datasets`) until it succeeds.
- `errors` entries are stored per dataset as `last_error` / `last_error_at`. The previous payload is kept. Errors for unknown datasets are dropped. A later successful payload for the dataset clears the error.
- Dataset names must match `^[a-z][a-z0-9_]{0,31}$`; the whole envelope is `422 validation_failed` otherwise.

**Response** (`fixtures/inventory-response.json`):

- `accepted`: name → stored hash
- `resync`: names
- `ignored`: names
- `seq`: the server's stored `last_inventory_seq`

The agent advances its acknowledged hashes **only** for datasets listed in `accepted` with a matching hash.

**Change history**

- For every applied dataset, the server diffs the old and new payload and writes `server_changes` rows (`dataset`, `kind`, `subject`, `old`, `new`, `detected_at`).
- The first full payload is a baseline and writes no change rows.
- In 1.0.0 the field-level diffs are:
  - `system.os.version_full`, `system.kernel.release`, `system.hostname`
  - `hardware.cpu.model`, `hardware.memory.total_bytes`
  - added/removed IP addresses in `network`
  - `resolvers.nameservers`

### Datasets in 1.0.0

| Name | Mode | Key (delta datasets) | Server-side denormalisation onto `servers` |
|---|---|---|---|
| `system` | full | — | `hostname`, `fqdn`, `os_family`, `os_id`, `os_name`, `os_version` (`version_full`), `os_codename`, `kernel_release`, `arch`, `virtualization` (`role/technology`; in a container `lxcfs` tells whether /proc shows the container's own memory, CPU and load rather than the host's), `boot_time`, `php_builds` (PHP installations no package owns, `prefix` and `version` read from their `php-config`) |
| `hardware` | full | — | `cpu_model`, `cpu_cores` (physical, falling back to logical), `cpu_threads`, `memory_bytes`, `swap_bytes`, `disk_bytes` (sum of non-removable physical disks, without RAID arrays, device-mapper volumes and zvols) |
| `storage` | full | — | `storage_health` (`healthy`, `degraded` or `failed`, see the app) and `storage_unhealthy_since` |
| `network` | full | — | `server_addresses` rows: interface, address, prefix, family 4/6, scope. Primary public IPv4/IPv6 go on `servers.public_ipv4` / `public_ipv6`. |
| `resolvers` | full | — | `nameservers` JSON column |
| `packages` | delta | `manager/name/arch` | rows in `server_packages` (manager, name, version, arch, source); derived `server_software` rows (see the app) |
| `updates` | full | — | `pending_updates_count`, `security_updates_count`, `reboot_required`, `updates_checked_at` (from `cache_updated_at`); `server_packages.candidate_version` / `is_security` |
| `cloud` | full | — | `provider`, `region`, `zone`, `instance_type`, plus derived `city`, `country`, `continent` (see the app). Manual overrides win. |
| `vhosts` | delta | `source/name` | rows in `server_vhosts` |
| `certificates` | delta | `fingerprint` | rows in `server_certificates` (+ links to sites and services) |
| `repositories` | full | — | rows in `server_repositories`; `server_packages.origins` |
| `applications` | delta | `path` | rows in `server_applications` and `server_application_packages`; `applications_count` |

The field names inside each payload are defined by the agent's collectors (`collector/*.go`). The fixture shows the authoritative shape for `system`, `hardware`, `network` and `resolvers`. The server must tolerate unknown additional fields.

- `packages`: `{ "manager": "dpkg" | "rpm", "items": [{ "name", "version", "arch", "source"?, "source_version"?, "versions"? }] }`, sorted by key.
  - `source` is the source package name when it differs from `name`: dpkg `Source:`, or rpm `SOURCERPM` with version, release and `.src.rpm` stripped.
  - `source_version` is the source package version when it differs from `version`. For dpkg it comes from `Source: name (version)` (binNMUs and multi-binary sources). For rpm it is the `version-release` from `SOURCERPM` **without an epoch**: the source RPM's file name carries none and subpackages have their own (perl `4:` versus perl-Carp `0:`), so the backend compares RPM source versions without epochs on both sides. Security advisories are matched on source name and source version.
  - The rpm `version` includes the epoch as `epoch:version-release` when the epoch is non-zero.
  - An rpm name/arch with several installed versions (kernels) is one item: `version` is the highest, and `versions` lists all of them.
  - `origins` (optional, sorted) lists the `repositories` ids that offer the installed version.
    - apt: match the installed name, version and arch against the `*_Packages` index files in `/var/lib/apt/lists`. Each index file maps to a repository.
    - rpm: dnf `repoquery --installed --qf '%{name}\t%{arch}\t%{from_repo}'`, or yum `yumdb`/`yum list installed` on EL7.
    - An empty list means no configured repository offers that version: manually installed `.deb`/`.rpm` files, removed repositories, or versions superseded and gone from the mirror.
    - The field is omitted when origins could not be determined at all, e.g. there are no index files yet.
- `updates`: `{ "manager": "apt" | "yum" | "dnf", "items": [{ "name", "current_version", "candidate_version", "arch", "security" }], "security_count", "reboot_required": bool | null, "cache_updated_at": RFC 3339 | null }`.
  - `cache_updated_at` is the mtime of the package manager's metadata cache. The agent refreshes that cache itself when `package_metadata_refresh` is enabled (see *Config*). It never installs, upgrades or removes packages.
  - `reboot_required` is `null` when it cannot be determined.
  - `updates` is a full dataset, so its hash only changes when the update list, the cache timestamp or the reboot flag change.
- `cloud`: `{ "provider": string | null, "source": "metadata" | "dmi" | null, "region": string | null, "zone": string | null, "instance_type": string | null, "instance_id": string | null, "public_ipv4": string | null, "public_ipv6": string | null }`.
  - The agent reads DMI (`/sys/class/dmi/id/sys_vendor`, `product_name`, `bios_vendor`, `board_vendor`, `chassis_asset_tag`) first. It queries a cloud metadata endpoint only when DMI points to that provider, or with one short probe (connect timeout ≤ 500 ms, total ≤ 2 s) of the link-local address when DMI is inconclusive. Bare-metal servers never wait on metadata timeouts.
  - Provider slugs are `hetzner`, `aws`, `digitalocean`, `gcp`, `azure`, `ovh`, `oracle`, `vultr`, `linode`, `scaleway` and `openstack` (any other OpenStack cloud).
  - `source: "dmi"` means the provider is known from DMI only, with no metadata. All fields are `null` when nothing is detected; that is a valid result, not an error.
  - `region` and `zone` use the provider's own names (`fsn1`, `fsn1-dc14`, `eu-central-1`, `eu-central-1a`, `ams3`). The backend maps them to a city, country and continent.
  - Metadata is never stored beyond these fields. User data and credentials from metadata services must not be read. The agent therefore queries **leaf endpoints** only, never full documents that embed them (DigitalOcean `v1.json` and Oracle `/opc/v2/instance/` contain `user_data`; OpenStack `meta_data.json` can contain `admin_pass`). It uses no proxy and follows no redirects for link-local requests.
  - **Provider specifics the backend must handle:**
    - Hetzner: `region` is derived from the zone (`fsn1-dc14` → `fsn1`), because the metadata's own `region` is a network zone (`eu-central`).
    - Azure: `zone` is `1`/`2`/`3` and is only meaningful together with `region` (the location, e.g. `westeurope`).
    - OpenStack (including OVH): there is often no region, and the zone is frequently `nova`. OVH is identified from DMI or a `*.ovh.net` hostname.
    - `instance_type` is `null` where the provider doesn't expose it (Hetzner, DigitalOcean, Vultr).
- `vhosts`: `{ "items": [{ "source", "name", "aliases", "ports", "ssl", "http2", "http3", "document_root", "php": { "version", "handler" } | null, "certificate": { "subject", "issuer", "not_after", "names" } | null, "config_file", "owner", "redirect": { "status", "target", "host" } | null, "https_redirect" }] }`, a delta dataset keyed by `source/name`.
  - `source` is `apache`, `nginx`, `plesk` or `directadmin`. A domain served by both a panel and its web server appears once per source; the panel entry is the authoritative one in the UI.
  - Panel entries include the implicit `www.<name>` alias that Plesk and DirectAdmin always serve, unless `name` already starts with `www.`.
  - `name` is the primary server name in lower case. `aliases` is sorted and deduplicated, and wildcards are kept as written (`*.example.com`); nginx's `.example.com` is reported as `example.com` and `*.example.com`. A site that serves both `www.<name>` and `<name>` is named `<name>`, whichever the configuration lists first. One item covers all of a name's ports: `ports` is sorted, and `ssl` is true when any port serves TLS.
  - `http2` is true when a block of the site offers HTTP/2: nginx `http2 on` (also inherited from the `http` block) or a `listen … http2`, Apache `Protocols h2` or `h2c` in the virtual host. `http3` is true when an nginx block of the site has a `listen … quic` and no `http3 off`; a QUIC listener without a `server_name` (a catch-all socket) belongs to no site. The `Alt-Svc` header that advertises HTTP/3 to browsers is not checked. Panel entries report both as false.
  - **Redirects:** a server block or virtual host that redirects every request (nginx `return 301 …` or a catch-all `rewrite … permanent` at server level, certbot's `if ($host = …) { return 301 … }` before `return 404`, or a `location /` that only redirects beside `/.well-known/`, mail autoconfiguration and named locations; Apache `Redirect /`, a catch-all `RedirectMatch`, or a catch-all `RewriteRule` to an absolute URL whose `RewriteCond`s only test the scheme, host or port) is folded into the site of the same source it points to, because Forge, Ploi, certbot and Plesk add such blocks to every site:
    - A redirect to `https` on the requested host (`return 301 https://$host$request_uri`) adds its ports to the sites of its names and sets their `https_redirect` to true. Its wildcard names are dropped once one of its names is a site.
    - A redirect to another host the source serves (`www.example.com` → `example.com`) adds its names to that site as aliases, and its ports, when it serves the site's certificate or none. Names of a plain HTTP redirect that the site's certificate does not cover are dropped, as they never serve TLS.
    - Any other redirect, including one with a certificate of its own, stays an item with `redirect` set: `status` (301, 302, 303, 307 or 308), `target` as configured with its variables (`https://new.example.com$request_uri`), and `host`, the target's host in lower case, or `null` when the target keeps the requested host. `redirect` is `null` for sites that serve content.
  - **Implicit panel aliases:** Plesk adds `ipv4.<name>`, `ipv6.<name>` and a preview name `<name>.<address>.<preview zone>` to every site, and Plesk and DirectAdmin add `www.<name>` to subdomains. These are dropped from the items of every source for that name unless the item's certificate covers them. A Plesk subdomain is a domain with a `parentDomainId`.
  - `php.version` is `major.minor` when it can be determined: a Plesk or DirectAdmin PHP selection, a PHP-FPM socket or pool path such as `/run/php/php8.2-fpm.sock`, or a handler package. `handler` is one of `fpm`, `fastcgi`, `cgi`, `module` or `lsapi`.
  - `certificate` is parsed by the agent from the configured certificate file. Only the public certificate is read, never the key. `not_after` is RFC 3339 UTC. It also carries `fingerprint` (SHA-256 of the leaf's DER, lowercase hex), which links the site to its entry in the `certificates` dataset.
  - `owner` is the panel subscription or user (Plesk subscription, DirectAdmin user). For a site no panel manages, it is the regular user (uid 1000 or above) whose home directory holds the document root, as Forge and Ploi run each site from `/home/<user>/…`, else `null`.
  - `config_file` is the absolute path of the file that defines the vhost, without a line number, or `null` when there is none (panel entries without a generated config).
  - Vhosts named only by an IP address (Apache's fallback without `ServerName`) and nginx catch-alls (`_`, `""`) are skipped.
  - **Partial failure:** each installed source is collected independently. When a source fails but succeeded before, the agent keeps that source's previous items, so a broken config doesn't delete and re-add rows, and logs a warning. The dataset only reports an error (in `errors`) when every installed source fails.
  - Collection is read-only:
    - `apachectl -S` / `apache2ctl -S` / `httpd -S`, with the referenced config files parsed for `DocumentRoot` and certificate paths
    - nginx: parse `nginx.conf` and follow `include` globs, bounded in depth and file count. The agent never runs `nginx -T`, because its config test opens (and can create) every log file, fails with "Too many open files" on servers with hundreds of sites, and prints nothing when any include is missing. Missing include files are skipped.
    - Plesk: `plesk db` read queries against `psa`, or `plesk bin site --list` plus per-site info. Certificates come from `certificates.cert`, which holds the public PEM form-encoded (`-----BEGIN+CERTIFICATE-----%0A…`). The agent never reads `certificates.cert_file`, because that file also contains the private key.
    - DirectAdmin: `/usr/local/directadmin/data/users/*/domains.list` and the per-domain config files
  - The interval is 900 s, and the agent re-collects early when web server or panel config directories change mtime.

### `repositories`

`{ "manager": "apt"|"yum"|"dnf", "items": [ … ] }`, a full dataset. It is small, typically 5–30 items. `fixtures/inventory-repositories-request.json` shows the shape, with a `packages` payload whose `origins` refer to it.

**Item fields:**
- **`id`** is stable and **opaque** to the backend (currently 8 hex digits for the apt hash part):
  - apt: a short hash of `file`, `type`, the URI, suite and components, prefixed with the suite for readability, e.g. `bookworm-security:3f2a…`
  - rpm: the repo id from its `.repo` section
- **`file`:** the defining file, e.g. `/etc/apt/sources.list.d/php.list` or `/etc/yum.repos.d/remi.repo`.
- **`type`:** `deb`, `deb-src` or `rpm`. **`enabled`:** bool.
- **`uris`:** the configured URIs: apt `URIs`, rpm `baseurl`, or `mirrorlist`/`metalink` (marked with a `mirrorlist:`/`metalink:` prefix). Credentials in URIs (`user:pass@`) are **stripped** by the agent, as are query strings, except these non-secret parameters on `mirrorlist`/`metalink` URIs: `repo`, `arch`, `country`, `protocol`, `content`, `infra`, `release`. Those identify the repository, e.g. `metalink?repo=epel-9&arch=$basearch`.
- **Suite and naming:** `suite` and `components` (apt), or `name` (rpm).
- **`signing`:** `{ "signed_by": path|fingerprint|null, "trusted": bool, "gpgcheck": bool|null }`.
  - apt `[trusted=yes]` / `Trusted: yes` → `trusted: true`, meaning signatures are not checked.
  - rpm `gpgcheck=0` → `gpgcheck: false`.
  - apt without `signed-by` falls back to the global keyring, which is legacy but still verified: `signed_by: null`, `trusted: false`.
- **`transport`:** the scheme of the first URI: `https`, `http`, `file`, `cdrom`, `tor`, `mirror`, …
- **`packages`:** the number of installed packages whose `origins` include this repository.

**Sources, all read-only:**
- apt:
  - `/etc/apt/sources.list` and `sources.list.d/*.list` (one-line format: options in `[...]`, multiple components).
  - `*.sources` (deb822 format, multiple URIs and suites per stanza, `Enabled: no`).
  - Commented-out lines are ignored.
- yum/dnf:
  - `/etc/yum.repos.d/*.repo` (INI, `$releasever`/`$basearch` left unexpanded) plus `dnf repolist --all` / `yum repolist all -q` for the effective `enabled` state.
  - Commands run with `exit_on_lock` and a timeout.
- Vendor classification (Sury, Freexian, Ubuntu ESM, EPEL, Remi, NodeSource, Docker, MariaDB, MySQL, Plesk, PPAs, …) is done by the **backend**, from the URIs, so it can evolve without agent releases.
- **Interval:** 3600 s. Re-collected early when the sources or repo directories change, and after a metadata refresh.

### `certificates`

`{ "items": [ … ] }`, a delta dataset keyed by `fingerprint`. Each certificate appears **once** per server, however many sites or services use it. `fixtures/inventory-certificates-request.json` shows the shape, with the `vhosts` items linking to it.

**Item fields:**
- **Identity:** `fingerprint` (SHA-256 of the leaf DER, lowercase hex), `serial` (hex)
- **Subject and issuer:** `subject` `{ cn, org }`, `issuer` `{ cn, org }`
- **Validity:** `not_before`, `not_after` (RFC 3339 UTC); `names` (SAN DNS names, sorted, plus IP SANs as strings)
- **Key and signature:** `key` `{ type: rsa|ecdsa|ed25519, bits, curve }`, `signature_algorithm`
- **Trust:** `self_signed` (bool); `chain` `{ complete: bool|null, intermediates: int }`. `complete` means the leaf verifies to a trusted root using the intermediates found in the configured file or chain file. `null` means it couldn't be determined.
- **`paths`:** the certificate files where this certificate was found, sorted.
- **`used_by`:** `[{ "kind": "site"|"postfix"|"dovecot"|"exim"|"panel"|"other", "ref": string }]`. For a site, `ref` is the `vhosts` key (`source/name`); for a service, its name.
- **`managed_by`:** `{ "tool": "certbot"|"acme.sh"|"plesk"|"directadmin"|"forge"|null, "name", "plugin", "installer", "ca", "ca_url", "key_type", "renewal": "timer"|"cron"|"panel"|"none"|null, "config" }`, or `null` when unknown.
  - `name` is the certbot lineage, acme.sh domain, panel certificate name or Forge certificate ID.
  - `plugin` is the authenticator: `webroot`, `nginx`, `apache`, `standalone`, `dns-cloudflare`, `dns-domainchief`, acme.sh `dns_cf`, and so on.
  - `ca` is a normalised CA slug (`letsencrypt`, `letsencrypt-staging`, `zerossl`, `buypass`, `google`, `sectigo`) derived from `ca_url` or the issuer.
  - `renewal` says whether automation was found.
  - `config` is the renewal config file path.

**Sources, all read-only:**
- **Sites:** the certificate files referenced by `vhosts`, and Plesk's `certificates.cert` column.
- **Services:**
  - Postfix: `postconf -h smtpd_tls_cert_file smtpd_tls_chain_files`, or a `main.cf` parse
  - Dovecot: `ssl_cert = <path`, following `!include`
  - Exim: `tls_certificate`
  - Panels: Plesk's `/usr/local/psa/admin/conf/httpsd.pem`; DirectAdmin's `conf/cacert.pem`, or the `cacert` from `directadmin.conf`
- **certbot:**
  - Read `/etc/letsencrypt/renewal/*.conf` (INI): `cert`, and in `[renewalparams]` `authenticator`, `installer`, `server`, `key_type`.
  - Match it to a certificate by the fingerprint of its `cert`.
  - Renewal automation: `certbot.timer` / `snap.certbot.renew.timer` enabled, or a `certbot renew` line (also `certbot-auto` / `letsencrypt-auto`, or a scheduled `certonly`) in `/etc/crontab`, `/etc/cron.d/*`, root's crontab or an executable script in `/etc/cron.{hourly,daily,weekly,monthly}`.
- **acme.sh:**
  - Read `~root/.acme.sh/<domain>[_ecc]/<domain>.conf` (`Le_API`, `Le_Webroot`, which is a path or a `dns_*` API, `Le_Keylength`, the cert path), and `ACME_DIRECTORY` / `account.conf` defaults.
  - Renewal automation: an `acme.sh --cron` crontab line.
- **Plesk:** certificates whose `certificates.name` starts with `Lets Encrypt ` are managed by Plesk's SSL It!/Let's Encrypt extension. Renewal: `panel`.
- **Laravel Forge:** Forge installs a renewal job per Let's Encrypt certificate (`/etc/cron.d/letsencrypt-renew-domain-<domain>`, running `/root/.letsencrypt-renew/<id>`). The agent matches the `<id>` to `/etc/nginx/ssl/domains/*/<id>/server.crt` or `/etc/nginx/ssl/*/<id>/server.crt`; the job script is not read. `name` is the `<id>`, `config` the script path, renewal `cron`. Ploi uses certbot.
- **DirectAdmin:** a domain with Let's Encrypt enabled (`<domain>.conf` `letsencrypt=1`, or the presence of `<domain>.cert.creation_time` / `.san_config`) is managed by `directadmin`. Renewal: `panel`.
- Private keys are **never** read or parsed. PEM blocks other than `CERTIFICATE` are skipped while scanning, and files whose only content is a key are ignored.
- **Interval:** 3600 s. The agent re-collects early when `/etc/letsencrypt/renewal`, `/etc/letsencrypt/live`, `~root/.acme.sh`, `/etc/cron.d` or the web, mail and panel cert directories change.

### `applications`

`{ "truncated": bool, "items": [ … ] }`, a delta dataset keyed by `path`: the web applications the sites of the `vhosts` dataset serve. `fixtures/inventory-applications-request.json` shows the shape.

**Item fields:**
- **`path`:** the application root as configured, through symbolic links such as a deploy's `current/`, so a new release keeps its key.
- **`kind`:** `wordpress`, `laravel`, `symfony`, `composer` or `node`, the first that matches. **`version`:** the WordPress (`$wp_version`), `laravel/framework` or `symfony/http-kernel` version, or `null`.
- **`sites`:** the `vhosts` keys (`source/name`) of the sites it serves. **`owner`:** the first site's owner.
- **`lockfiles`:** the lockfiles read, by file name.
- **`packages`:** `[{ "ecosystem": "composer"|"npm", "name", "version", "direct", "dev" }]`, unique per ecosystem, name and version. `direct` when `composer.json` or `package.json` requires the package itself; `dev` when only development dependencies need it (`packages-dev`, npm's `dev` flag, or for `yarn.lock` and pnpm 9 not reachable from the production dependencies of `package.json`).
- **`wordpress`:** `{ "plugins": [{ "slug", "name", "version", "must_use" }], "themes": [{ "slug", "name", "version", "parent" }] }` or `null`. Read from the header files like WordPress lists them; whether a plugin or theme is active is in the database and not collected. `must_use` plugins (`mu-plugins`) are always active.
- **`truncated`:** packages, plugins or themes were left out at a limit (5000 packages per lockfile, 500 plugins and themes per install). The top-level `truncated` says applications or entries were left out (1000 applications, 100000 entries per server).

**Sources, all read-only:**
- The application root is the nearest directory at or up to 3 levels above a document root with `composer.json`, `package.json`, `artisan`, a lockfile or a WordPress core (`wp-includes/version.php` at the root or in `wp/`, `wordpress/` or Bedrock's `web/wp/`), within the hosting user's tree. A WordPress directory without `composer.json` defers to a parent with one (Bedrock). A document root without one serves no application.
- Only files at the application root are read, never below it, so the lockfiles of plugins, vendored packages, backups and other deploy releases are not: `composer.lock`, `package-lock.json`, `npm-shrinkwrap.json`, `pnpm-lock.yaml`, `yarn.lock`, and the dependency names of `composer.json` and `package.json`.
- WordPress plugins, must-use plugins and themes: the first 8 KiB of their header files (`Plugin Name`, `Theme Name`, `Version`, `Template`).
- `wp-config.php`, `.env` and other configuration files are **never** opened.
- **Interval:** 3600 s. The agent re-collects early when the web server configuration, an application's lockfiles or manifests, `wp-includes` or the plugin and theme directories change.

## Config — `GET /config`

- The agent sends `If-None-Match: <etag>` and gets `304` when nothing changed. The etag is opaque (a quoted string such as `"c1-0123456789ab"`), is repeated in the `ETag` header and is the same value as `config_etag` in the enroll and heartbeat responses.
- `200` returns `fixtures/config-response.json`:
  - `etag`
  - `intervals.heartbeat`
  - `intervals.datasets`: per-dataset seconds
  - `datasets`: the names enabled for this server. Teams can turn off collecting sites and certificates (`vhosts`, `certificates`), applications (`applications`, also off without sites) and cloud metadata (`cloud`), and a server can override its team. Turning one off removes what was stored for it, without change history entries, and publishes a new config.
  - `realtime`
  - `package_metadata_refresh`: `{ "enabled": bool, "interval": seconds }`. Absent means enabled with 21600 s (6 hours).
  - `metrics`: `{ "enabled": bool, "endpoint": url, "interval": seconds }`, or `null` when this Server Chief has no metrics gateway. Absent or `null` means no metrics (see *Metrics*).
- **Defaults:** heartbeat 300 s; `system`, `network` and `resolvers` 300 s; `hardware` 3600 s; `storage` 300 s; `packages` 3600 s (or when the package database mtime changes); `updates` 21600 s; `vhosts` 900 s (or when config changes); `cloud` 86400 s (and at agent start).

## Package metadata refresh and package activity

**Metadata refresh** (when `package_metadata_refresh.enabled`):
- Every `interval` seconds, with ±10% jitter, the agent refreshes the package manager's metadata cache:
  - `apt-get update -qq`
  - `yum makecache -q` (`yum makecache fast` on EL7)
  - `dnf makecache -q`
- It runs at the lowest CPU and IO priority (`nice -n 19`, `ionice -c3` where available), with a 10-minute timeout.
- It never runs install, upgrade, remove or any other mutating command.
- It is **skipped** for that cycle when:
  - `cache_updated_at` is already younger than `interval`, for example because apt's own daily timer refreshed it
  - any package-manager lock is held (see below)
- A failure, such as EOL mirrors or no network, is logged and retried on the next cycle with backoff. It is never reported as a dataset error, because `updates` still reports from the existing cache.
- After a successful refresh, the agent collects `updates` immediately.

**Package activity trigger:**
- The agent polls the package database every 30 s (cheap `stat` only): `/var/lib/dpkg/status`, or `/var/lib/rpm/rpmdb.sqlite` / `Packages`.
- On a change it waits for an **idle period**: no further database change for 120 s, **and** no package-manager lock held.
- Then it collects `packages` and `updates` immediately, regardless of their intervals.
- If locks are held for over 2 hours, it collects anyway on the next idle check after the database settles.

**Locks:**
- Locks are only **inspected**, never taken: `fcntl(F_GETLK)` on `/var/lib/dpkg/lock-frontend`, `/var/lib/dpkg/lock`, `/var/lib/apt/lists/lock`, `/var/cache/apt/archives/lock` and `/var/lib/rpm/.rpm.lock`, plus live-PID checks of `/var/run/yum.pid` and dnf's `*.pid` lock files.
- Collectors that could block on a lock (`yum`/`dnf check-update`) run with `exit_on_lock` (yum) and timeouts. Collection never waits on a package manager.

## Realtime — Socket Chief (Pusher protocol 7)

- `realtime` in the enroll and config responses is either `null` (polling only) or `{ "key", "host", "port", "scheme", "auth_endpoint" }`.
- The agent subscribes to the private channel `private-agent.server_<server_id>.generation_<generation>`.
- The channel is authorised with `POST {auth_endpoint}`, which is `/agent/v1/broadcasting/auth`. It takes the form fields `socket_id` and `channel_name` and requires Bearer auth. The server allows only the channel that matches the authenticated server and its current generation.
- **Events** (empty payload `{}` unless noted; they are wake-ups only):
  - `config.updated`: the agent fetches `/config` now.
  - `commands.available`: the agent calls `GET /commands` now.
  - `metrics.live` (1.1.0) with payload `{ "until": <Unix seconds> }`: someone watches the server's charts. While metrics are enabled, the agent samples every second and posts to the metrics gateway's `/v1/live` until `until`, capped at 5 minutes from receiving the event. A later event extends the window; an earlier `until` never shortens it. It stops early when the gateway reports no open streams for 10 seconds. See the metrics protocol, *Live*.
- If realtime is down, nothing breaks; heartbeats and polling cover it. This route is separate from the user broadcasting auth route.
- `auth_endpoint` is always `{endpoint}/broadcasting/auth`. It answers like any Pusher auth endpoint (`{"auth": "<key>:<signature>"}`), or `403` for any other channel.

## Commands

Commands are how the control plane asks an agent to do something. In 1.0.0 the kinds are a closed list; there is no shell or arbitrary execution.

- **Delivery:** the server sets `commands_available: true` in heartbeat responses while the server has commands that are `pending`, or `delivered` but not yet acknowledged, and not expired: exactly what `GET /commands` would return. It also sends the realtime `commands.available` event when a command is created. Without realtime, a command therefore reaches the agent within one heartbeat interval.
- `GET /commands`
  - Returns `200 { "commands": [{ "id", "kind", "args", "created_at", "expires_at" }] }`: the pending and delivered-but-unacknowledged commands for this server, oldest first, at most 20 (`fixtures/commands-response.json`).
  - The server marks returned commands `delivered`.
- `POST /commands/{id}/ack` (empty body)
  - Returns `204` and marks the command `acknowledged`. It is idempotent.
  - Returns `404 not_found` for an unknown command, or one belonging to another server.
- `POST /commands/{id}/result`
  - Body: `{ "status": "succeeded" | "failed", "detail": string | null, "finished_at": RFC 3339, "data": object | null, "output": string | null, "output_truncated": bool }` (`fixtures/command-result-request.json`, `fixtures/reboot-result-request.json`). `data` is optional and kind-specific; `update_agent` and `reboot` use it (see *Agent updates* and *Reboots*). It is at most 4 KiB.
  - `detail` is at most 1000 characters. The server truncates longer values instead of rejecting them.
  - `output` (1.2.0) is optional: the combined stdout and stderr of the programs the command ran, in order, as UTF-8 (invalid bytes replaced), with a `$ <argv>` line before each program. The panel keeps it as the command's execution log. It is at most 1 MiB: the agent keeps the end of longer output and sets `output_truncated: true`. The server keeps the end of a longer `output` too, instead of rejecting it. Commands that run nothing (`refresh_inventory`, `resync`, `refresh_config`) send none.
  - Returns `204`. It is idempotent: the first result wins, and a repeat returns `204` without changing anything. A late result for an `expired` command also returns `204` and is ignored.
  - Returns `404 not_found` for an unknown command, or one belonging to another server.
- `POST /commands/{id}/output` (1.2.0): live output of a running `upgrade_packages` command, see *Live output*.
- **Lifecycle:** `pending → delivered → acknowledged → succeeded | failed`.
  - Commands that are not acknowledged within 24 hours of creation (`expires_at`) become `expired` and are no longer returned.
  - Commands still `acknowledged` one hour after they were acknowledged, without a result, also become `expired`; three hours for `update_agent` and `upgrade_packages`, which may wait an hour for the package manager first.
  - The agent keeps a local journal (`/var/lib/serverchief/commands.json`) and never runs a command it already ran, even when the server lists it again.
- **Kinds:**
  - `refresh_inventory` `{ "datasets": [names] | null }`: collect the listed datasets (or all enabled ones) now and push whatever changed. The UI "Refresh inventory" button creates this command.
  - `resync` `{ "datasets": [names] | null }`: send the listed (or all enabled) datasets in full on the next push, even when unchanged.
  - For both, listed datasets that are not enabled are skipped. The agent reports `failed` when none of the listed datasets are enabled, otherwise `succeeded` with detail `not enabled: <names>` when some were skipped.
  - `refresh_config` `{}`: fetch `GET /config` now.
  - `update_agent` `{ "version" }`: see *Agent updates*.
  - `reboot` `{ "not_after", "if_required" }` (1.2.0): see *Reboots*.
  - `upgrade_packages` `{ "scope", "restart_services", "agent_version", "not_after" }` (1.2.0): see *Package updates*.
  - `disable_capability` `{ "capability" }` (1.2.0): turns a remote action off on the server, see *Operator opt-out*.
- **Deadline** (1.2.0): `reboot` and `upgrade_packages` take an optional `not_after` (RFC 3339, or `null` or absent for none). The agent does not start the command once its own clock reaches `not_after`, also not while it waits for the package manager: it reports `failed` with code `too_late`. Once the command started (a reboot was scheduled, an upgrade was handed to its helper) the deadline no longer applies and the command finishes. The backend uses it for work that must not happen later than planned, such as a maintenance window.
- **Unknown kinds:** the agent acknowledges them and reports `failed` with detail `unsupported command kind "<kind>"`. Newer servers can then add kinds without breaking older agents.

## Privileges

Since agent 0.2.0 the daemon does not run as root where it does not have to.

- **Unprivileged daemon:** `serverchief.service` runs as the `serverchief` system user with only `CAP_DAC_READ_SEARCH` (ambient), `NoNewPrivileges` and `ProtectSystem=strict`. It can read every file, but write only `/var/lib/serverchief` and `/var/log/serverchief`. `identity.json` is `root:serverchief 0640`.
- **Root helper:** for the few things that need root, the daemon writes `<id>.request.json` to `/var/lib/serverchief/helper/`. `serverchief-helper.path` starts `serverchief-helper.service`, a root oneshot running `serverchief internal-helper`, which runs every pending request and writes `<id>.result.json`. Requests name one of a fixed list of actions and carry no arguments: what runs is decided by the helper, never by the daemon.
  - `ping`: answers at once.
  - `refresh-metadata`: the package metadata refresh (`apt-get update`, `yum`/`dnf makecache`) at the lowest priority.
  - `plesk-sites`: the Plesk sites from the psa database, with the queries the helper builds from the detected schema, or the names from `plesk bin site --list`.
  - `apache-vhosts` (agent 0.2.1): `apachectl -S`, for when the daemon cannot run it: modules such as ModSecurity open log files for writing while the configuration is read. The daemon tries it unprivileged first.
  - `start-agent-update`: starts the update helper for the request the daemon wrote (see *Agent updates*).
  - `reboot` (1.2.0): schedules a reboot in one minute (see *Reboots*). It checks the operator opt-out itself and refuses when `reboot` is turned off.
  - `disable-capability` (1.2.0) with argument `capability`: adds a known capability to the operator opt-out, see *Operator opt-out*. It never removes one.
  - `start-package-upgrade` (1.2.0): starts the package upgrade helper for the request the daemon wrote (see *Package updates*). It checks the operator opt-out itself and refuses when `upgrade_packages` is turned off.
  - The helper refuses a request directory others can write to, opens requests without following symlinks, only accepts small files of the directory's owner, and creates results as new files it renames into place.
- **Root fallback:** systemd before 229 (CentOS 7) cannot grant ambient capabilities, and SysV has none. There the package installs `/etc/systemd/system/serverchief.service` from `/usr/share/serverchief/serverchief-root.service` (or keeps the init script) and the daemon runs as root, running the same actions in-process.
- **Updating from a root agent:** an update from a version before 0.2.0 is driven by that version's update helper, which only trusts root's files. When an update is pending and the state directory still belongs to root, the package installs the root unit as a transitional override. Once the update is settled, the daemon starts `serverchief internal-unprivilege` as a transient unit, which hands the state to the `serverchief` user, removes the override and restarts the service unprivileged.
- **Reads:** a symlink inside a hosting user's tree (`/home/*`, panel vhost directories, any directory a user other than root or the agent owns) is only followed when it ends inside that tree, and every file is read up to a size cap. The unprivileged daemon can read everything, so a configuration a hosting user controls must not point it elsewhere.
- **Heartbeat:** `capabilities.privilege` is `{ "mode": "unprivileged" | "root", "helper_actions": [names], "reason": string | null }`. An unprivileged agent pings its helper at most once an hour; when it does not answer, `helper_actions` is empty and `reason` tells why. A root agent lists every action and tells why it runs as root. The server records a change of `mode` in the change history (`agent` / `privilege`), so an escalation to root is never silent; the first report sets the baseline.

## Operator opt-out

Remote actions are available by default. Whoever runs the server can turn them off on the server itself, where neither the panel nor the agent can turn them back on.

- `serverchief disable <capability>...` turns capabilities off, `serverchief enable <capability>...` turns them on again and `serverchief capabilities` lists them with their state. They need root.
- The capabilities are `reboot` and `upgrade_packages`. Agent self-updates (`update_agent`) cannot be turned off this way, the team or server setting for automatic updates covers them. They are separate: `upgrade_packages` also brings the agent itself up to date first (see *Package updates*), so stopping every remote change to the agent needs both.
- The state lives in `/etc/serverchief/disabled-capabilities.json` (`{ "version": 1, "disabled": [names] }`, `root:root 0644`), apart from `config.json`. The agent only reads it, and nothing it receives from the backend changes it. A missing file means nothing is turned off; an unreadable or invalid file, or one that root does not own or that its group or others can write, turns every capability off, so a damaged opt-out never turns into an opt-in. Validly formed names the agent does not know are kept, so a downgrade keeps what a newer agent turned off.
- The daemon reads the file before every heartbeat. `serverchief disable` and `enable` ask a running daemon to reload, so the next heartbeat reports the change right away.
- A turned-off capability is left out of `capabilities` and listed in `capabilities.disabled`, so the panel can tell "turned off on the server" from "not supported".
- A command for a turned-off capability fails with code `disabled`. The daemon checks before it starts the command, and the root helper checks again before it acts.
- **Turning off from the panel** (1.2.0): the command `disable_capability` `{ "capability": "reboot" | "upgrade_packages" }` turns a capability off, so the panel can offer it without anyone logging in to the server. Turning a capability on again stays with `serverchief enable` on the server: no command does that, so a compromised panel or credential can only make a server do less.
  - The agent rejects unknown arguments and capability names (`failed`, detail `invalid args: …`, no `data`).
  - An already turned-off capability reports `succeeded` with code `already_disabled`. So does any capability while the file cannot be trusted: every capability already counts as off then, and writing a new list over it would turn the others on.
  - Otherwise the root helper's `disable-capability` action adds the name the way `serverchief disable` does (`root:root 0644`, written atomically), and the agent reports `succeeded` with code `disabled`. The helper reads the file again itself and only ever adds a name.
  - The daemon then reads the opt-out again and sends a heartbeat right away, so the panel sees the capability turned off.
  - The result body has `data: { "code", "capability" }` (`fixtures/disable-capability-result-request.json`). A failure of the helper reports `failed` with code `disable_failed` and the helper's error as `detail`.

## Reboots

**Capability** (1.2.0): every heartbeat includes `capabilities.reboot: { "available": bool, "reason": string | null }`, unless the operator turned it off. `available` is true when all of these hold:
- The agent can run the root helper's `reboot` action: it runs as root, or its root helper answers.
- The server is not an application container (Docker, Podman, containerd), which has no init system to reboot. System containers (LXC, systemd-nspawn) and virtual machines can reboot.
- `shutdown` (or `systemctl` on systemd) exists.

Otherwise `reason` explains why. The backend only sends `reboot` to agents that report `available: true`.

**Command `reboot` `{ "not_after": RFC 3339 | null, "if_required": bool | null }`:**
- `not_after` is optional, see *Commands*, *Deadline*.
- `if_required` is optional. With `true` the agent only reboots when the server needs it, checked right before it would reboot (after waiting for the package manager), the way the `updates` dataset tells: Debian and Ubuntu's `/run/reboot-required` (or `/var/run/reboot-required`), `needs-restarting -r` on RHEL and derivatives. It reports `succeeded` with code `not_required` when no reboot is needed, and `failed` with code `required_unknown` when it cannot tell (RHEL without `needs-restarting`): it never reboots blindly. Absent, `null` or `false` reboots regardless.
- The agent rejects arguments it does not know (`failed`, detail `invalid args: …`, no `data`), so the backend must only send arguments a later revision adds to agents that speak it.
- A second `reboot` while one waits or is handed off fails with code `reboot_failed`.
- Steps, after acknowledging:
  1. **Check.** If the operator turned `reboot` off, report `failed` with code `disabled`. If the capability is not available, report `failed` with code `unsupported` and the reason as `detail`.
  2. **Wait for the package manager.** If a package-manager lock is held (see *Locks*), wait, checking every 30 s, for up to 15 minutes. After that, report `failed` with code `package_manager_busy`: a reboot never interrupts a package manager. Reaching `not_after` first, or before the wait, reports `too_late` instead.
  3. **Persist, then reboot.** Write `/var/lib/serverchief/reboot-request.json` (command id, the current `boot_id` from `/proc/sys/kernel/random/boot_id`, the boot time, when it was requested) and journal the command as handed off. Then run the root helper's `reboot` action: `shutdown -r +1 "Server Chief: rebooting in 1 minute, requested from the panel"`, which warns logged-in users. If that fails, remove the request and report `failed` with code `reboot_failed` and the command's output. Once the reboot is scheduled, send the command's output as live output (1.3.0), sending a failed chunk again for up to the minute before the server goes down and dropping it when the panel rejects it, and send a heartbeat right away: the panel measures from that contact whether the server comes back in time, and treats a server that has not made contact again 6 minutes later (the minute of warning and 5 to boot) as a stalled reboot.
  4. **Result.** On every start, the daemon reads the request file. A different `boot_id` means the server rebooted: report `succeeded` with code `rebooted`. The same `boot_id` once the request is 10 minutes old means it did not, for example because someone ran `shutdown -c`: report `failed` with code `not_rebooted`. Either way the result's `output` is that of scheduling the reboot followed by a `# ` line on how it went (1.3.0): when the server booted again and how long after the reboot was scheduled, or which boot it still runs. The daemon then deletes the file.
- The result body has `data: { "code", "previous_boot_time", "boot_time" }` (`fixtures/reboot-result-request.json`). Besides the codes above, `too_late` reports a reboot that was not started before `not_after`, and `not_required` and `required_unknown` a reboot with `if_required` that did not happen. `boot_time` is the boot time when the result was decided, RFC 3339 or `null` when unknown.
- The result arrives after the agent started again, well within the usual hour after acknowledgement. If the command still expires, the backend treats it as `rebooted` when the server's boot time moved past the moment the agent acknowledged it.

## Package updates

**Capability** (1.2.0): every heartbeat includes `capabilities.upgrade_packages: { "available": bool, "reason": string | null }`, unless the operator turned it off. `available` is true when all of these hold:
- A supported package manager exists: apt, dnf or yum.
- The agent can run the root helper's `start-package-upgrade` action: it runs as root, or its root helper answers.
- The upgrade helper can run detached: `systemd-run` on systemd, or the SysV fallback.

Otherwise `reason` explains why. The backend only sends `upgrade_packages` to agents that report `available: true`.

**Command `upgrade_packages` `{ "scope": "security" | "all", "restart_services": bool, "agent_version": "1.2.3" | null, "not_after": RFC 3339 | null }`:**
- `scope`: `security` installs the pending updates the `updates` dataset marks as security updates, `all` installs every pending update.
- `restart_services`: whether services that use updated libraries are restarted afterwards, so the updates take effect without a reboot.
- `agent_version`: when set, the agent first updates itself to that stable version, exactly like `update_agent` (preflight, install from the Server Chief repository only, readiness, rollback). The backend sets it when the server runs an older stable agent than the newest release, regardless of the automatic agent update settings: installing package updates includes the agent's own package.
- `not_after` is optional, see *Commands*, *Deadline*. It applies until the command is handed to the upgrade helper.
- Unknown arguments, or values that do not validate, are rejected (`failed`, detail `invalid args: …`, no `data`). A second `upgrade_packages` while one runs fails with code `upgrade_failed`.
- Steps, after acknowledging:
  1. **Check.** If the operator turned `upgrade_packages` off, report `failed` with code `disabled`. If the capability is not available, report `failed` with code `unsupported` and the reason as `detail`.
  2. **Wait for the package manager.** If a package-manager lock is held (see *Locks*), wait, checking every 30 s, for up to 1 hour. After that, report `failed` with code `package_manager_busy`. Reaching `not_after` first, or before the wait, reports `too_late` instead.
  3. **Persist, then hand over.** Write `/var/lib/serverchief/upgrade-request.json` (command id and arguments), journal the command as handed off and run the root helper's `start-package-upgrade`, which starts `serverchief internal-upgrade` detached, so it survives the daemon's restart (systemd: `systemd-run --unit=serverchief-upgrade --collect`; SysV: a double-forked `setsid` child).
  4. **Upgrade helper** (root), resuming from its persisted state after a crash:
     1. If dpkg reports an interrupted install (`dpkg --audit`), stop with code `dpkg_interrupted`. The agent never runs `dpkg --configure -a` on its own.
     2. Refresh the package metadata (`apt-get update`, `dnf`/`yum makecache`).
     3. With `agent_version` newer than the installed agent, update the agent (see *Agent updates*). Whatever its outcome, continue; `data.agent_update` reports it.
     4. Install the updates of the scope, never touching the `serverchief` package:
        - apt: `apt-get install -y --only-upgrade <packages>` with the pending updates of the scope minus `serverchief`, which also installs new dependencies such as a kernel's. The packages come from an `apt-get -s dist-upgrade` simulation, so phased and held packages are not named and stay put.
        - dnf/yum: `upgrade -y --exclude=serverchief`, with `--security` for the `security` scope. Without security metadata (CentOS 7 repositories, a missing yum plugin) the `security` scope stops with code `security_unavailable`.
        - Non-interactive, keeping local configuration files: `DEBIAN_FRONTEND=noninteractive` and `-o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold`, with `-o Dpkg::Use-Pty=0` to leave dpkg's progress line out of the output.
     5. Services: with `restart_services`, apt lets needrestart restart them (`NEEDRESTART_MODE=a`); on dnf/yum the helper restarts the services `needs-restarting -s` lists with `systemctl try-restart`. The agent's own service is never restarted this way: the deb package ships `/etc/needrestart/conf.d/serverchief.conf` for needrestart, and on dnf/yum the helper also leaves D-Bus, `systemd-*`, getty and `user@` units alone. Without `restart_services`, needrestart is suspended (`NEEDRESTART_SUSPEND=1`) and nothing is restarted. Where needrestart is not installed (Debian's default), apt restarts nothing on its own and `services_restarted` is `null`.
     - Between steps the helper waits up to 15 minutes for a package-manager lock that another process took.
     6. Check whether a reboot is required, the same way the `updates` dataset does.
     7. Write `/var/lib/serverchief/upgrade-result.json`. The daemon reports it, on its next start or as soon as it sees the file, then deletes the files and collects `packages` and `updates`.
- The result body has `data: { "code", "scope", "upgraded", "held_back", "reboot_required", "services_restarted", "agent_update" }` (`fixtures/upgrade-packages-result-request.json`):
  - `code`: `upgraded` and `nothing_to_upgrade` with status `succeeded`; `upgrade_failed`, `dpkg_interrupted`, `security_unavailable`, `package_manager_busy`, `too_late`, `disabled` and `unsupported` with status `failed`.
  - `upgraded`, `held_back`: how many packages were upgraded, and how many pending updates of the scope were left (phased or kept back), `null` when unknown.
  - `reboot_required`: `true`, `false` or `null` when unknown.
  - `services_restarted`: the services restarted, at most 50 names, an empty list when none needed it, or `null` when services were not restarted or it cannot be told.
  - A failed run still reports `upgraded`, `held_back` and `reboot_required` when they could be counted, since part of the updates may have been installed. Refusals before the helper ran carry only `code` and `scope`.
  - `agent_update`: `{ "code", "previous_version", "installed_version" }` with the *Agent updates* codes, or `null` when the agent was not updated.
- `output` carries every command the helper ran with its output, the end of it when it is longer than 1 MiB.
- **Result timeout:** like `update_agent`, an acknowledged `upgrade_packages` expires 3 hours after acknowledgement.
- A `reboot` that arrives while an upgrade runs waits for it like for any package manager, and fails with `package_manager_busy` after its 15 minutes.

## Live output

While an `upgrade_packages` command runs, the agent sends its output as it grows, so the panel can show it live. A `reboot` (1.3.0) sends the output of scheduling it once it is scheduled, since its result only arrives after the server is back (see *Reboots*). Other kinds do not stream.

- `POST /commands/{id}/output`, body `{ "offset": int, "length": int, "output": string }` (`fixtures/command-output-request.json`):
  - `offset` and `length` count bytes of the agent's raw output, from its start: the chunk covers bytes `offset` to `offset + length`.
  - `output` is that part as UTF-8, with invalid bytes replaced, so it can be shorter or longer than `length` in bytes. A chunk ends at a character boundary; an incomplete character at the end waits for the next chunk.
  - `output` is at most 64 KiB.
- Answers:
  - `204` when the chunk was appended, or when it repeats bytes the panel already has (`offset + length` at most what it received), which changes nothing.
  - `409 offset_mismatch` with `expected_offset` (the bytes the panel received) when the chunk starts after that, or starts before it and goes past it. The agent continues from `expected_offset`.
  - `204` without storing anything for a command that already finished or expired.
  - `404 not_found` for an unknown command, one of another server, or one of another kind. A 1.2.0 panel answers this for a `reboot`; the agent then leaves it to the result.
- The agent sends one chunk at a time per command: it checks for new output every 250 ms, sends it once the previous chunk is answered, and sends the next chunk right away while more is waiting. Output that arrives while a chunk is on its way goes in the next one, so the chunks follow the output's pace, and nothing is sent while the command prints nothing. A chunk that fails is sent again, after at least 2 seconds; one the panel rejected with `409` continues from `expected_offset`.
- The panel keeps the end of the live output, at most 1 MiB, like the result's `output`. The result's `output` replaces it, so the stored output is always the agent's final version. When a result carries no `output`, the live output stays.
- The upgrade helper runs apart from the daemon (see *Package updates*), so it writes its output to `/var/lib/serverchief/upgrade-output.log` as it goes, with the same `$ <argv>` and `# ` lines, and the daemon sends what is new. The helper writes a `# ` line when it starts a step: refreshing the package lists, updating the agent, installing the updates, restarting services.
- Without live output (an older agent, or the chunks failed), the panel shows the output when the result arrives.

## Agent updates

The design follows the Backup Chief agent's updates. Server Chief enables automatic updates by default, with a staged rollout.

**Capability:**
- Every heartbeat includes `capabilities: { "agent_update": { "available": bool, "reason": string | null } }`.
- `available` is true only when all of these hold:
  - The agent was installed from the Server Chief package (dpkg/rpm reports the `serverchief` package with the running version).
  - The Server Chief package repository is configured: `/etc/apt/sources.list.d/serverchief.{list,sources}` or `/etc/yum.repos.d/serverchief.repo`.
  - The update helper can run (systemd, or the SysV fallback), and an unprivileged agent's root helper answers.
- Otherwise `reason` explains why, e.g. "installed from a tarball", "repository not configured" or "running in a container".
- The backend only sends `update_agent` to agents that report `available: true` with a stable version.

**Command `update_agent` `{ "version": "1.2.3" }`:**
- `version` is a stable semantic version, validated by the agent. The agent never accepts a package name, repository or shell fragment from the command.
- The command expires like any other command.
- Steps, after acknowledging:
  1. **Wait for the package manager.** If a package-manager lock is held (see *Locks*), wait, checking every 30 s, for up to 1 hour. After that, report `failed` with code `package_manager_busy`.
  2. **Preflight.**
     - Refresh metadata for the Server Chief repository only.
     - Check that both the target version and the currently installed version are available from it; the current version is needed for rollback.
     - Missing target: `target_unavailable`. Missing current version: `rollback_unavailable`, and the update is not attempted.
  3. **Persist a request, then hand over.** Write a mode-0600 request file (`/var/lib/serverchief/update-request.json`: command id, previous and target version), then hand over to a detached helper (`serverchief internal-update`) that survives the daemon's restart:
     - systemd: `systemd-run --unit=serverchief-update --collect`, a transient service
     - SysV: a double-forked `setsid` child
  4. **Install, restricted to the Server Chief repository and the `serverchief` package:**
     - apt: `apt-get install -y --only-upgrade --allow-downgrades serverchief=<version>` with `-o Dir::Etc::sourcelist=<serverchief list>` and `-o Dir::Etc::sourceparts=-`
     - dnf/yum: `install -y serverchief-<version>` with `--disablerepo='*' --enablerepo=serverchief`

     Nothing else is installed or upgraded.
  5. **Readiness.**
     - The package's postinst restarts the daemon.
     - The new daemon writes a readiness marker (`/var/lib/serverchief/update-ready.json` with command id and running version) once it has started and sent a successful heartbeat.
     - The helper waits up to 2 minutes for a marker with the target version.
  6. **Rollback.**
     - On failure, the helper installs the previous version the same way and waits for its readiness.
     - Codes: `readiness_failed_rolled_back` if that succeeds, `rollback_failed` if not, `install_failed` when the package manager failed and the previous version is still installed.
  7. **Result.**
     - The helper writes `/var/lib/serverchief/update-result.json`.
     - On its next start, the daemon reports the command result and then deletes the files.
     - The result body gains `data: { "code", "previous_version", "target_version", "installed_version" }`, with code `updated` on success.
- The helper is idempotent: a crash at any step resumes from the persisted state on the next daemon start.
- **Result timeout:** an acknowledged `update_agent` command expires **3 hours** after acknowledgement, not the usual 1 hour. That covers up to 1 hour of waiting for the package manager, the install, readiness, and a rollback with its own readiness.
  - If it still expires, the backend counts it as `install_failed`.
  - A later heartbeat reporting the target version marks it `updated` after all.
- **Failure classes:**
  - **Release failures** count towards pausing a team's rollout, and the server is skipped until it's retried manually:
    - `install_failed`
    - `readiness_failed_rolled_back`
    - `rollback_failed`
  - **Environment failures** don't count towards the pause. The server is retried automatically on a later run after 6 hours:
    - `package_manager_busy`
    - `target_unavailable` (e.g. a mirror lagging behind)
    - `rollback_unavailable`
  - `unsupported_install` makes the server ineligible until its heartbeat reports `agent_update.available: true` again.

**Backend rollout:**
- Releases are synced from GitHub releases of the agent repository. Only stable, non-draft, non-prerelease versions count.
- Teams have automatic updates **enabled by default**, and servers can opt out individually.
- A release becomes eligible for automatic rollout 24 hours after publication.
- An hourly job queues `update_agent` for at most 10 eligible servers per team per run.
- A team's rollout of a release pauses after 2 **release failures** of that release (see the failure classes above), until someone resumes it in the UI.
- A server whose update to a release failed is skipped until it's retried manually.
- Manual "Update now" (UI, REST, MCP) bypasses the 24 hours and the batching, but not the eligibility checks.

## Metrics

Host metrics go to a separate service, the metrics gateway, following the Server Chief metrics protocol (v1). This protocol only configures them and reports on them.

- `GET /config` (and the enroll response) carries `metrics`. With `enabled`, the agent samples host usage every `interval` seconds (60 by default) and sends the samples to `{endpoint}/v1/samples` with its own credential. Without it, the agent stops and drops what it had spooled.
- Teams can turn off collecting metrics and a server can override its team, like sites and cloud metadata. Turning them off removes the stored samples, and the gateway drops samples taken before that moment, so a spooled backlog cannot bring them back.
- The gateway accepts a server once the panel published its credential, a few seconds after enrollment; until then it answers `401` and the agent retries in 5 minutes.
- The heartbeat's `capabilities.metrics` reports `enabled` (whether the agent sends metrics), `last_sent_at` (the last batch the gateway stored, or `null`), `spool_bytes` (what waits on disk), `error` (the last delivery failure, `null` after a success) and `live` (`true` when the agent answers `metrics.live`), so the panel can tell when samples stop arriving and whether it can ask for live samples.

## Not yet in the protocol

- `software` on the agent side: the backend derives software from `packages`. Detecting source and panel builds by file path is planned.
- Security advisories: these are backend-only (Debian/Ubuntu/RHEL feeds matched against `packages`) and need no protocol change.
