# Changelog

## [Unreleased]

### Changed

- Establish a file-based licensing policy: retain MPL-2.0 for inherited source and its modifications, and use AGPL-3.0-only for new independently authored source files from the licensing-policy commit onward.
- Record upstream commit `0a29338e1f102a462363ce3527417029f89bab28` as the inherited baseline and preserve existing third-party licensing and attribution.
- Adopt `github.com/kirakirai8023/Xboard-Node` as the Go module path and update internal imports for independent fork development.
- Point installer downloads, CLI upgrades, service documentation, and installation examples to this fork without an upstream binary fallback.
- Derive GHCR publishing tags from the lowercase repository name and use the fork's container image in deployment examples.
- Document the fork-only contribution workflow and the release publishing requirements for install and upgrade commands.
- Standardize development, CI, and Docker builds on Go 1.27.1.
- Update direct and transitive dependencies and remove the sing-box and Xray fork replacements.
- Use official sing-box v1.14.4 and pin Xray v26.10.29 to release commit `0afec66085a85191e9dd2a4a6646c7c9a94688d1` through a Go pseudo-version.
- Adapt the sing-box lifecycle to upstream APIs: rebuild instances for credential or configuration changes, preserve cumulative traffic counters, keep connections open for limit-only updates, and attempt to restore the previous configuration if startup fails.
- Move Xray per-user rate limiting into the local dispatcher, sharing token buckets across each user's connections and preserving the concrete pipe reader types required by mux/XUDP.
- Remove Mieru from the advertised sing-box inbound and outbound protocol lists.
- Pin the Vultr SDK to v3.20.0 for compatibility with the libdns provider, which relies on types removed in newer SDK releases.
- Normalize SOCKS/HTTP user identifiers for Xray traffic accounting and disable splice for managed users so traffic passes through the rate limiter.
- Use the same SS2022 key conversion for initial configuration and live user updates, matching Xray's new byte-based account format.
- Translate the README additions and changelog into English and make English the default for future documentation.

### Added

- Add full MPL-2.0 and AGPL-3.0 license texts, SPDX source headers, and a licensing overview that identifies the initial AGPL-only files.
- Include project license notices in release assets and container images.
- Add a Compose file on the development branch so deployment uses the fork's image without relying on the legacy `compose` branch.
- Add SOCKS/VLESS integration tests for both official kernels, covering bidirectional traffic, limit updates, and traffic accounting. Enable them with the `integration` build tag and a dedicated CI step in an isolated environment.
- Add in-memory Xray tests for pipe handling, limiter updates, and cancellation.
- Add `AGENTS.md` with guidance on the project structure, development commands, coding conventions, tests, commits, and configuration security.
- Link the contributor guide and changelog from the README.

### Fixed

- Close the README installation example's code block so subsequent sections render correctly.
