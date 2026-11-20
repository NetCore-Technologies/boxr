# Supported toolchain and automation versions

Last verified: **2026-11-19**.

## Go

- Minimum language/toolchain version: **Go 1.25.0** (`go.mod`); the source is compatible with the Go 1.25 language/toolchain floor.
- CI compatibility matrix: **Go 1.26.x** and **Go 1.27.x** (newer supported release lines).
- Release builds: **Go 1.27.4**.
- Current supported patch releases at verification time: Go 1.26.11 and Go 1.27.4.
- Dev Container: `mcr.microsoft.com/devcontainers/go:1.27-bookworm`.

The module keeps Go 1.25 compatibility while CI exercises newer Go release lines. This allows local builds on the installed Go 1.25 toolchain while catching regressions on current Go versions. Release builds use an explicit patch version for repeatable release inputs.

## GitHub Actions

The workflows use current major tags so compatible patch releases are picked up without editing each workflow for every patch:

| Action | Workflow reference | Verified latest release |
| --- | --- | --- |
| `actions/checkout` | `@v7` | v7.0.1 |
| `actions/setup-go` | `@v7` | v7.0.0 |
| `github/codeql-action` | `@v4` | v4.39.3 |
| `actions/configure-pages` | `@v6` | v6.0.0 |
| `actions/upload-pages-artifact` | `@v5` | v5.0.0 |
| `actions/deploy-pages` | `@v5` | v5.0.1 |
| `softprops/action-gh-release` | `@v3` | v3.0.3 |
| `actions/attest` | `@v4` | v4.2.2 |

Go vulnerability analysis uses `golang.org/x/vuln/cmd/govulncheck@v1.10.0`.

Dependabot checks GitHub Actions and Go modules weekly. This file is a dated snapshot, not a promise that these are permanently the newest releases; review upstream changelogs before changing major versions.

## Upstream references

- [Go release history](https://go.dev/doc/devel/release)
- [Go development container image](https://github.com/devcontainers/images/tree/main/src/go)
- [actions/checkout releases](https://github.com/actions/checkout/releases)
- [actions/setup-go releases](https://github.com/actions/setup-go/releases)
- [CodeQL Action releases](https://github.com/github/codeql-action/releases)
- [GitHub Pages actions](https://github.com/actions)
- [softprops/action-gh-release releases](https://github.com/softprops/action-gh-release/releases)
- [actions/attest releases](https://github.com/actions/attest/releases)
- [govulncheck versions](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck)
