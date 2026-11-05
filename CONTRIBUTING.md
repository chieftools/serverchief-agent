# Contributing

Thanks for your interest in the Server Chief Agent. Bug reports and pull requests are welcome. For anything larger than a small fix, open an issue first so we can agree on the approach before you spend time on it.

Report security vulnerabilities privately, as described in [SECURITY.md](.github/SECURITY.md), and not in a public issue.

Before you change behaviour, read [How the agent works](docs/architecture.md). If your change touches what goes over the wire, also read the [agent protocol](docs/protocol/README.md).

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

CI runs the same checks, as well as `shellcheck` on the scripts in `bin/` and `packaging/`, and a smoke test of `serverchief collect` on old and current distributions.

`bin/build-deb.sh` and `bin/build-rpm.sh` also accept `SKIP_BUILD=1` to package a binary built earlier.

Collectors read the host only through `collector.Host`, so most changes can be tested against fixtures in `collector/testdata/` without root or a real server. Use example domains (`example.com`, `.test`) and documentation addresses (`192.0.2.0/24`, `2001:db8::/32`) in fixtures.

The agent never installs, upgrades or removes packages outside its update and upgrade helpers. `TestPackageCommandAllowList` and `TestUpdateCommandAllowList` pin every package-manager command it can run, and `TestNoMutatingPackageVerbsInSource` fails on a mutating verb anywhere else. Expect review to be strict about changes that add one.

### Shorter timings

`SERVERCHIEF_DEBUG_INTERVALS=1 serverchief run` shortens the package metadata refresh and package activity timings (poll 2 s, idle 10 s, refresh every minute, lock retry 20 s, lock patience 2 minutes), so they can be watched in a throwaway container. It is not a supported setting.

### Test builds for agent updates

For end-to-end tests of agent updates in throwaway containers, `TEST_BUILD_TAGS` adds test-only hooks to `bin/build.sh`: `serverchief_e2e_never_ready` (the daemon never writes its readiness marker, so an update to it rolls back) and `serverchief_e2e_short_lock_wait` (the helper gives up on a held lock after 30 s). Never set it for a release.

## Testing against a local backend

This section is for the Server Chief team, who run the control plane locally. Contributors without it can test with `serverchief collect` and `serverchief sample`, which need no backend.

The Server Chief app runs locally at `https://server.chief.test` under Laravel Valet, whose CA is `~/.config/valet/CA/LaravelValetCASelfSigned.pem`. `setup` takes `--endpoint` (or `$SERVERCHIEF_ENDPOINT`) for the local backend and `--ca-file` (or `$SERVERCHIEF_CA_FILE`) to trust its CA; the CA's path is stored in `identity.json`, so `serverchief run` trusts it too.

`bin/dev-docker.sh` runs enrolled development agents in containers and rebuilds them in place; run it without arguments for its usage. To do the same by hand, create a server in the local app, copy its token and enroll a container. Under OrbStack, containers reach the Mac's Valet through `host.orb.internal`; `--add-host server.chief.test:host-gateway` maps the app's hostname to that address, or pass the IPv4 address of `host.orb.internal` explicitly:

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

The container has no init system, so setup prints the manual-start hint and `serverchief run` runs in the foreground. The realtime host (`socket.chieftools.cloud`) is public and needs no extra mapping. Without a container, run both commands as root on a Linux VM with the same `/etc/hosts` entry.

## Changing the protocol

The [agent protocol](docs/protocol/README.md) and its [fixtures](docs/protocol/fixtures/) live in this repository. The agent's tests replay the fixtures from there, and the Server Chief control plane keeps a copy for its own tests.

Additive changes bump the minor revision (`ProtocolRevision` in `agent/types.go`) and are listed at the top of the spec with the agent version that introduced them; incompatible changes need `/agent/v2`. Update the spec, the fixtures and the agent in the same change, then copy the fixtures to the control plane.

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
