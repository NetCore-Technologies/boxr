# Contributing to boxr

Thanks for helping improve boxr. Runtime changes can affect host security, so keep changes focused and test both success and rollback/error paths.

## Development setup

- Recommended toolchain: Go 1.27.4; CI also verifies the Go 1.26 release line. `go.mod` retains a Go 1.23 language compatibility floor.
- Linux is required for namespace/cgroup integration work.
- cgroup v2 with delegated controllers is required for privileged resource-limit integration tests.

## Before opening a pull request

```bash
make lint
make test
make cross-build
```

For changes to rootfs archive handling, include malicious archive fixtures covering traversal, symlinks, duplicate names, special files and resource limits. For state changes, verify file permissions, event ordering, cleanup and environment-secret redaction. Default unit tests should not require privilege; integration tests must detect missing prerequisites and skip with a clear reason.

## Pull request expectations

- Explain user-visible behaviour and compatibility impact.
- Update README, SECURITY and `docs/how-it-works.md` when guarantees change.
- Distinguish implemented behaviour from future plans.
- Do not claim security isolation that the implementation does not provide.
- Avoid dependencies unless the standard library is insufficient.
- Keep CI green and include reproducible validation commands.

## Commit style

Use concise imperative messages, for example `feat(state): add container lifecycle records` or `test(rootfs): cover digest stability`.
