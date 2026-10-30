# Repository Guidelines

This independently maintained fork uses `github.com/kirakirai8023/Xboard-Node` as its Go module path. Target this repository for development, releases, and updates; do not submit upstream pull requests. Keep external dependencies and attribution links at their original sources.

## Project Structure & Module Organization

- `cmd/xboard-node/` contains the node daemon; `cmd/xbctl/` contains the administration CLI.
- `internal/service/` coordinates nodes; `internal/controlplane/` and `internal/panel/` handle local configuration and panel communication.
- `internal/kernel/` defines the shared kernel interface, with `singbox/` and `xray/` implementations.
- Other internal packages cover configuration, models, certificates/DNS providers, limits, traffic tracking, and monitoring.
- Tests live beside implementation files as `*_test.go`. Configuration examples and extension documentation live at the repository root; CI is in `.github/workflows/ci.yml`.

## Build, Test, and Development Commands

Use Go 1.27.3 as declared in `go.mod`. Make targets assume a POSIX shell, such as Linux or WSL.

- `make build`: build `xboard-node` and `xbctl` with the configured feature tags.
- `make test`: run `go test -v -race -count=1 ./internal/...`, matching CI.
- `make test-integration`: run SOCKS/VLESS traffic tests on an isolated host without Xray/TUN interception.
- `make build-all`: cross-compile both binaries for Linux amd64 and arm64.
- `make docker`: build the local Docker image.

Copy `config.yml.example` to `config.yml`, configure a development instance, then run `./xboard-node -c config.yml`.

## Coding Style & Naming Conventions

Format Go changes with `gofmt -w path/to/file.go`; use tabs and standard Go import grouping. Use lowercase package names, descriptive snake_case filenames, and PascalCase exported identifiers. Save text as UTF-8 without BOM.

Write documentation and changelog entries in idiomatic English.

Follow [LICENSE.md](LICENSE.md): inherited files and their edits retain MPL-2.0; independently authored new source files use AGPL-3.0-only. Preserve notices, add the appropriate SPDX header, and keep copied or adapted upstream code under its existing terms. Classify provenance before moving code across the license boundary.

Keep responsibilities focused, reuse shared model validation, and implement backend behavior through the existing kernel and control-plane interfaces. Use official kernel modules; pin Xray releases by their GitHub commit when release tags cannot be used as Go module versions. No dedicated linter configuration is checked in.

## Testing Guidelines

Use Go's `testing` package, table-driven cases, and `net/http/httptest` for panel interactions. Name tests `TestFeature_Scenario` and keep fixtures local using temporary directories or mock servers.

Run focused checks with `go test ./internal/config -run TestLoad -v`, then `make test`. Cover regressions and relevant kernel behavior. No numeric coverage threshold is configured.

## Commit & Pull Request Guidelines

Follow the prevalent history style: `feat:`, `fix:`, or `refactor:`, optionally scoped, for example `fix(cert): reload PEM on warm restart`. Use concise English subjects.

Describe the problem, behavior changes, linked issues, and validation results in each PR. Update the README, relevant extension docs, and changelog when behavior or configuration changes.

## Security & Configuration

Keep panel tokens, DNS credentials, private keys, and production configuration out of commits and logs. Use placeholders in examples; local `config.yml` and `.env` files are ignored.
