# xboard-node

Node backend for [Xboard](https://github.com/cedar2025/Xboard). Supports `sing-box` / `xray-core` dual kernels.

This is an independently maintained fork of [cedar2025/xboard-node](https://github.com/cedar2025/xboard-node). Development, releases, and updates are maintained in [kirakirai8023/Xboard-Node](https://github.com/kirakirai8023/Xboard-Node).

> **Disclaimer**: This project is for educational and learning purposes only.

## Features

- Protocols: V2Ray family, Trojan, Shadowsocks, Hysteria2, TUIC, AnyTLS
- Sync: WebSocket push + REST polling dual channel
- User controls: speed limit, device limit, alive-IP tracking, hot update
- Deploy modes: node mode, machine mode, standalone mode
- Multi-instance: single process binding multiple panels / nodes

## Install

Release downloads and container images come from this fork. Publish a tagged release and a GHCR image before using the binary or container installation commands below; see [Fork releases](#fork-releases). You can build locally with `make build` in the meantime.

### Docker

```bash
docker run -d --restart=always --network=host \
  -e apiHost=https://panel.com -e apiKey=TOKEN -e nodeID=1 \
  ghcr.io/kirakirai8023/xboard-node:latest
```

### Docker Compose

```bash
git clone --depth 1 https://github.com/kirakirai8023/Xboard-Node.git
cd Xboard-Node
mkdir -p config
cp config.yml.example config/config.yml
vim config/config.yml   # set panel.url / token / node_id
docker compose up -d
```

### Installer (Linux systemd)

```bash
# Node mode
curl -fsSL https://raw.githubusercontent.com/kirakirai8023/Xboard-Node/dev/install.sh | \
  sudo bash -s -- --mode node --panel https://panel.example.com --token TOKEN --node-id 1

# Machine mode
curl -fsSL https://raw.githubusercontent.com/kirakirai8023/Xboard-Node/dev/install.sh | \
  sudo bash -s -- --mode machine --panel https://panel.example.com --token TOKEN --machine-id 1
```

## xbctl

Run `xbctl` after installation for help. Common commands:

```bash
xbctl list                          # list all instances
xbctl status                        # running status
xbctl bind add-node --panel URL --token TOKEN --node-id 1
xbctl bind add-machine --panel URL --token TOKEN --machine-id 1
xbctl bind remove-node --panel URL --node-id 1
xbctl service restart
```

## Configuration

Legacy single-panel config is fully compatible. Appending bindings auto-migrates to `instances` format. See `config.yml.example`.

## Extensions

- Custom routes: [docs-custom-routes.md](docs-custom-routes.md)
- Custom outbounds: [docs-custom-outbounds.md](docs-custom-outbounds.md)
- DNS providers (ACME DNS-01): [docs-dns-providers.md](docs-dns-providers.md)

## Development and Contributing

Read the [Repository Guidelines](AGENTS.md) for the project layout, build and test commands, coding conventions, and contribution requirements. See the [changelog](CHANGELOG.md) for release notes.

The Go module path is `github.com/kirakirai8023/Xboard-Node`. Keep its spelling and capitalization consistent in imports. Development stays in this fork; upstream pull requests are not part of the workflow.

Use **Go 1.27.1**, matching the CI and Docker build environments. Run these commands on Linux or WSL:

```bash
make build             # Build xboard-node and xbctl
make test              # Run internal package tests with the race detector
make test-integration  # Run SOCKS/VLESS traffic tests in an isolated environment
make build-all         # Cross-compile for Linux amd64 and arm64
```

Traffic integration tests use the `integration` build tag and run in a separate CI step. Run them on an isolated host, since a local Xray/TUN setup can intercept test traffic. Regular `make test` runs do not start these proxy listeners. The integration test configuration allows Xray private-network egress only to the local test target.

The project uses the official [sing-box v1.14.4](https://github.com/SagerNet/sing-box/releases/tag/v1.14.4) and [Xray v26.10.29](https://github.com/XTLS/Xray-core/releases/tag/v26.10.29) modules without fork replacements. Xray is pinned to release commit `0afec66085a85191e9dd2a4a6646c7c9a94688d1`, which Go resolves to `v1.260327.1-0.20261029083850-0afec66085a8`. The version prefix follows Go's module versioning rules; the pinned source is the October release.

Upstream sing-box does not expose the fork's APIs for updating users and routes at runtime. Changes to credentials, TLS, or routes replace the embedded instance and disconnect active sessions while preserving cumulative traffic counters. Speed and device limit changes keep existing connections open. Mieru is unavailable upstream, so it is no longer listed as a supported inbound or outbound protocol.

Xray per-user rate limiting uses the local dispatcher and shared token buckets. The dispatcher normalizes SOCKS/HTTP user identifiers for traffic accounting and disables splice for managed users to keep traffic on the rate-limiting path.

`govultr/v3` is pinned to `v3.20.0`, the version supported by `libdns/vultr/v2 v2.0.4`. Newer SDK versions remove types that this provider still requires.

## Fork Releases

The installer and `xbctl upgrade` download `xboard-node-linux-{amd64,arm64}` and `xbctl-linux-{amd64,arm64}` from this fork's GitHub Releases. They do not fall back to upstream binaries. Create a `v*` tag and publish a stable release through CI to make the default `latest` downloads available. Development builds use the `dev` release; select them explicitly with `--version dev`.

CI publishes images to `ghcr.io/kirakirai8023/xboard-node`, deriving the lowercase image name from `GITHUB_REPOSITORY`. Enable GitHub Actions for the fork and ensure the published package is public before using the unauthenticated Docker examples. The Compose file lives alongside the source on `dev`, so deployment does not depend on the separate `compose` branch.

The Xboard panel link, official kernel modules, and third-party GeoIP/GeoSite download sources retain their upstream locations.

## License

This is a mixed-license project. Inherited source files and changes to those files remain **MPL-2.0**. Independently authored new source files introduced with the licensing-policy commit or later use **AGPL-3.0-only**, identified by their SPDX headers. Files containing copied or adapted MPL-covered code remain MPL-2.0 regardless of when they were added.

See [LICENSE.md](LICENSE.md) for the inherited baseline, file-level scope, combined-program obligations, and dependency notices. Full license texts are in [LICENSES/](LICENSES/). This is not a blanket choice of either license for every file.
