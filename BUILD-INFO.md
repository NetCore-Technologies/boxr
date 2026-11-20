# Build information

- Snapshot date: 2026-11-20
- Go toolchain used for verification: go1.23.2 linux/amd64
- Feature registry: 408 CLI capabilities (`dist/boxr-linux-amd64 features --json`)
- Verification: `make verify` passed in this environment (go vet, race-enabled tests, formatting check, and linux/amd64, linux/arm64, windows/amd64, darwin/amd64, darwin/arm64 cross-builds).
- Test packages: 8
- Cross-build targets: 5
- Git history: 50 non-empty local feature-sized commits, reconstructed from the staged worktree on 2026-11-20.

## Important publishing note

This ZIP includes `.git` with the reconstructed 50-commit local history. It is intentionally not connected to GitHub and does not share ancestry with the live upstream `main` branch. Do not force-push it. For upstream publication, transplant reviewed source changes into an up-to-date authenticated clone and use the guarded publishing procedure documented in `scripts/push-to-main.sh` / `LOCAL_HISTORY.md`.

## Runtime compatibility

The container runtime relies on Linux kernel namespace, mount, cgroup v2 and related facilities. Windows and macOS builds are CLI cross-builds and do not provide native Linux container isolation.
