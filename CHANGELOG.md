# Changelog

## [Unreleased]

### Security

- Upgrade `github.com/go-jose/go-jose/v4` from v4.1.4 to v4.1.6 to fix RSA JWK parsing denial of service ([GHSA-mgcc-cww5-fp2q](https://github.com/go-jose/go-jose/security/advisories/GHSA-mgcc-cww5-fp2q)), deeply nested JSON parsing crashes ([GHSA-vvm8-gf78-28g4](https://github.com/go-jose/go-jose/security/advisories/GHSA-vvm8-gf78-28g4)), and CBC-HMAC processing panics ([GHSA-4w3g-jfj2-cw2q](https://github.com/go-jose/go-jose/security/advisories/GHSA-4w3g-jfj2-cw2q)).
- Upgrade `github.com/cilium/ebpf` from v0.21.0 to v0.22.0 to fix integer overflow in BTF parsing through `LoadCollectionSpecFromReader` ([GHSA-xhgw-qwwf-pg32](https://github.com/advisories/GHSA-xhgw-qwwf-pg32), CVE-2026-10722).

### Changed

- Require Go 1.27.3 in go.mod, CI, and Docker builds to incorporate the standard-library security fixes through that release.
- Correct main-branch installation paths, xbctl binding flags, routing precedence and alternative-match semantics, kernel-specific outbound settings and limitations, and local DNS-01 configuration and credential names.
- Explain that the commented configuration example must be uncommented and populated before deployment.
- Clarify that inherited source files may lack SPDX headers without changing their license, and retain English documentation.
- Establish a file-based licensing policy: retain MPL-2.0 for inherited source and its modifications, and use AGPL-3.0-only for new independently authored source files from the licensing-policy commit onward.
- Record upstream commit `0a29338e1f102a462363ce3527417029f89bab28` as the inherited baseline and preserve existing third-party licensing and attribution.
- Adopt `github.com/kirakirai8023/Xboard-Node` as the Go module path and update internal imports for independent fork development.
- Point installer downloads, CLI upgrades, service documentation, and installation examples to this fork without an upstream binary fallback.
- Derive GHCR publishing tags from the lowercase repository name and use the fork's container image in deployment examples.
- Document the fork-only contribution workflow and the release publishing requirements for install and upgrade commands.
- Update direct and transitive dependencies and remove the sing-box and Xray fork replacements.
- Use official sing-box v1.14.4 and pin Xray v26.10.29 to release commit `0afec66085a85191e9dd2a4a6646c7c9a94688d1` through a Go pseudo-version.
- Adapt the sing-box lifecycle to upstream APIs: rebuild instances for credential or configuration changes, preserve cumulative traffic counters, keep connections open for limit-only updates, and attempt to restore the previous configuration if startup fails.
- Move Xray per-user rate limiting into the local dispatcher, sharing token buckets across each user's connections and preserving the concrete pipe reader types required by mux/XUDP.
- Remove Mieru from the advertised sing-box inbound and outbound protocol lists.
- Pin the Vultr SDK to v3.20.0 for compatibility with the libdns provider, which relies on types removed in newer SDK releases.
- Normalize SOCKS/HTTP user identifiers for Xray traffic accounting and disable splice for managed users so traffic passes through the rate limiter.
- Use the same SS2022 key conversion for initial configuration and live user updates, matching Xray's new byte-based account format.
- Maintain the README, extension documentation, and changelog in idiomatic English.

### Added

- Add full MPL-2.0 and AGPL-3.0 license texts, SPDX source headers, and a licensing overview that identifies the initial AGPL-only files.
- Include project license notices in release assets and container images.
- Include a Compose file with the source on main so deployment uses the fork's image without relying on the legacy `compose` branch.
- Add SOCKS/VLESS integration tests for both official kernels, covering bidirectional traffic, limit updates, and traffic accounting. Enable them with the `integration` build tag and a dedicated CI step in an isolated environment.
- Add in-memory Xray tests for pipe handling, limiter updates, and cancellation.
- Add `AGENTS.md` with guidance on the project structure, development commands, coding conventions, tests, commits, and configuration security.
- Link the contributor guide and changelog from the README.

### Fixed

- Accept explicit empty WebSocket user snapshots, stop listeners when all users are revoked, and restart with the newly authorized users after a full or incremental update. Preserve pending users when configuration has not arrived yet.
- Remove old credentials when a user's UUID changes and apply delta credential replacements without stopping the kernel between removal and addition. Extend real-traffic tests to verify that both kernels reject revoked credentials.
- Close Xray listeners and cancel sessions synchronously on Stop, and close superseded listeners before draining old instances, so revocation does not leave the old node accepting traffic.
- Remove authenticated URLs and raw WebSocket payloads from debug logs to avoid exposing panel tokens, user credentials, DNS credentials, and private keys.
- Redact URLs in control-plane transport errors and omit raw REST error-response bodies that could echo credentials.
- Prevent pull-request builds from publishing container images and keep dev pushes from overwriting the production `latest` tag.
- Close the README installation example's code block so subsequent sections render correctly.
