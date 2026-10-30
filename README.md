# xboard-node

Node backend for [Xboard](https://github.com/cedar2025/Xboard), with embedded official `sing-box` and `xray-core` kernels.

This is an independently maintained fork of [cedar2025/xboard-node](https://github.com/cedar2025/xboard-node). Development, releases, and updates are maintained in [kirakirai8023/Xboard-Node](https://github.com/kirakirai8023/Xboard-Node).

> This project is for educational and learning purposes only.

## Features and Limits

- Protocols: VMess, VLESS, Trojan, and Shadowsocks; sing-box also supports Hysteria, TUIC, AnyTLS, Naive, SOCKS, and HTTP inbounds. Availability depends on the kernel, build tags, and node configuration. Mieru is unavailable.
- Sync: WebSocket push and REST polling. Periodic polling pauses while WebSocket is connected, resumes after disconnect, and reconciles state when connectivity changes.
- User controls: speed limits, device limits, alive-IP tracking, and user updates. An empty full user snapshot revokes all users and stops listeners; adding users starts the kernel again.
- Deployment: node mode, machine mode with dynamic node discovery, and standalone mode without a panel. One process can bind multiple panels and nodes.
- sing-box credential, TLS, and route changes rebuild the instance and disconnect active sessions while preserving cumulative traffic. Limit-only changes keep sessions open. Xray update behavior depends on the protocol and configuration change.

Xray's daemon interface exposes VMess, VLESS, Trojan, Shadowsocks, and Hysteria v2 (configured as `protocol: hysteria` with `version: 2`). Its internal SOCKS/HTTP builders are not exposed as supported daemon protocols. Stopping either kernel closes listeners and sessions; Xray no longer defers explicit shutdown to background draining.

Xray full-instance reloads create the replacement before retiring the old instance. If a change reuses a listener address and the replacement cannot bind, the old configuration stays active; apply that change with a service restart.

## Install

The commands below require a published Release or public GHCR image from this fork; see [Fork Releases](#fork-releases). Until those are available, build locally with `make build` or `docker build -t xboard-node:local .`.

### Docker

```bash
docker run -d --restart=always --network=host \
  -e apiHost=https://panel.example.com -e apiKey=TOKEN -e nodeID=1 \
  ghcr.io/kirakirai8023/xboard-node:latest
```

This configures a single node through environment variables. Mount `/etc/xboard-node` to persist certificates and kernel caches.

### Docker Compose

```bash
git clone --depth 1 --branch main https://github.com/kirakirai8023/Xboard-Node.git
cd Xboard-Node
mkdir -p config
cp config.yml.example config/config.yml
vim config/config.yml   # uncomment the single-node panel block and set url / token / node_id
docker compose up -d
```

The root `compose.yml` mounts `./config` at `/etc/xboard-node`. The example configurations are commented out: uncomment either the single-node block or the multi-instance alternative, then fill in your credentials before starting the container.

### Installer (Linux systemd)

```bash
# Node mode
curl -fsSL https://raw.githubusercontent.com/kirakirai8023/Xboard-Node/main/install.sh | \
  sudo bash -s -- --mode node --panel https://panel.example.com --token TOKEN --node-id 1

# Machine mode
curl -fsSL https://raw.githubusercontent.com/kirakirai8023/Xboard-Node/main/install.sh | \
  sudo bash -s -- --mode machine --panel https://panel.example.com --token TOKEN --machine-id 1
```

The script comes from `main`; binary selection is controlled separately by `--version`, which defaults to `latest`. Replace `TOKEN` with the appropriate node or machine credential.

## xbctl

Run `xbctl help` after installation for the full command list:

```bash
xbctl list
xbctl status
xbctl bind add-node --panel-url URL --token TOKEN --node-id 1
xbctl bind add-machine --panel-url URL --token TOKEN --machine-id 1
xbctl bind remove-node --panel URL --node-id 1
xbctl service restart
```

Adding a binding uses `--panel-url`; removing one uses `--panel`. Binding commands modify configuration under `/etc/xboard-node` and restart the systemd service, so they require the corresponding system privileges.

## Configuration

See [config.yml.example](config.yml.example). Legacy single-panel configuration remains supported. `xbctl bind` migrates it to `instances` when adding bindings. Instances inherit top-level defaults and override them with nonzero settings; default data directories are isolated by instance.

Local certificate configuration uses `cert:`. Panel node JSON uses `cert_config`; these are different configuration locations. Machine mode uses `panel.url` with `machine.machine_id` and `machine.token` to discover nodes from the panel.

- [Custom routes](docs-custom-routes.md): precedence, match semantics, and kernel differences.
- [Custom outbounds](docs-custom-outbounds.md): protocols, native settings, and chaining limitations.
- [DNS providers](docs-dns-providers.md): ACME DNS-01 configuration and credentials.

Keep production configuration, panel tokens, DNS credentials, and private keys out of commits. Use HTTPS/WSS for the control plane. WebSocket debug logs record event names and payload sizes without dumping authentication URLs or message bodies. REST errors omit raw response bodies and redact transport URLs. Check older logs for secrets before sharing them.

## Development and Contributing

Read [AGENTS.md](AGENTS.md) for the project layout, build and test commands, coding conventions, and contribution requirements. See [CHANGELOG.md](CHANGELOG.md) for release notes.

The Go module path is `github.com/kirakirai8023/Xboard-Node`. Keep its spelling and capitalization consistent in imports. Development stays in this fork.

Use **Go 1.27.3**, matching `go.mod`, CI, and Docker builds and incorporating the earlier standard-library security fixes. Make targets require a POSIX shell, such as Linux or WSL:

```bash
make build             # Build xboard-node and xbctl
make test              # Run internal package tests with the race detector
make test-integration  # Run SOCKS/VLESS traffic tests in an isolated environment
make build-all         # Cross-compile for Linux amd64 and arm64
```

Traffic integration tests use the `integration` build tag and run in a separate CI step. Use an isolated host without Xray/TUN interception. Tests allow Xray private-network egress only to the local test target. Regular `make test` runs do not start these proxy listeners.

Dependencies pin official sing-box `v1.14.4` and Xray release commit `0afec66085a85191e9dd2a4a6646c7c9a94688d1`. Go resolves Xray to `v1.260327.1-0.20261029083850-0afec66085a8`; the prefix follows Go module versioning rules. Neither kernel uses a fork replacement.

Xray per-user rate limiting uses the local dispatcher and shared token buckets. Managed users have splice disabled so traffic passes through the limiter. `govultr/v3` is pinned to `v3.20.0`, the version supported by `libdns/vultr/v2 v2.0.4`.

## Fork Releases

The installer and `xbctl upgrade` download `xboard-node-linux-{amd64,arm64}` and `xbctl-linux-{amd64,arm64}` from this fork's GitHub Releases. A `v*` tag triggers a Release; default `latest` downloads require a release that GitHub marks as latest. Pushes to `dev` update the `dev` Release, selected explicitly with `--version dev`.

CI publishes images to `ghcr.io/kirakirai8023/xboard-node` on push events only. Main and `v*` tags update `latest`; the dev branch updates `dev`. Commit SHA and version tags are also published. Enable GitHub Actions and make the package public before using the unauthenticated Docker examples. The Compose file is included on `main`.

The Xboard panel, official kernel modules, and third-party GeoIP/GeoSite download sources retain their original locations.

## License

This is a mixed-license project. Inherited source files and their changes remain **MPL-2.0**. Independently authored new source files introduced with the licensing-policy commit or later use **AGPL-3.0-only**. Files containing copied or adapted MPL-covered code remain MPL-2.0.

See [LICENSE.md](LICENSE.md) for the inherited baseline, file-level scope, combined-program obligations, and dependency notices. Full texts are in [LICENSES/](LICENSES/). This is not a blanket choice of either license for every file.
