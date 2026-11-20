# Local 50-commit history

This archive contains a reconstructed, feature-sized Git history created on 2026-11-20 from the verified Boxr worktree.

Each commit is non-empty and adds implementation, tests, documentation, CI/release automation, or website assets. These commits were created locally for this archive; they are **not** historical upstream commits and have **not** been pushed to GitHub. This repository has no configured remote and its root commit is not based on the live `NetCore-Technologies/boxr` `main` branch. Do not force-push this archive to replace upstream history.

To publish the same source changes into an authenticated upstream checkout, review and use `scripts/push-to-main.sh` from that checkout after transplanting the files. It creates a smaller, grouped upstream commit series; it does not replay this reconstructed 50-commit archive history.

## Commit plan

01. `build: establish Go module and local development targets` — `.gitignore`, `go.mod`, `LICENSE`, `Makefile`, `.devcontainer/devcontainer.json`
02. `feat(cli): add command dispatch and regression tests` — `cmd/boxr/main.go`, `cmd/boxr/main_test.go`
03. `feat(runtime): add namespace runner and platform adapters` — `internal/container/run.go`, `internal/container/run_linux_test.go`, `internal/container/run_other.go`, `internal/container/overlay_linux_test.go`
04. `feat(runtime): validate container option model` — `internal/container/options.go`, `internal/container/options_test.go`
05. `feat(cgroups): control CPU memory and process resources` — `internal/container/cgroup.go`, `internal/container/cgroup_test.go`
06. `feat(security): add capability drops and mount controls` — `internal/container/capabilities.go`, `internal/container/mounts.go`
07. `feat(seccomp): add base and architecture-specific policies` — `internal/container/seccomp_linux.go`, `internal/container/seccomp_linux_amd64.go`, `internal/container/seccomp_linux_arm64.go`
08. `test(seccomp): cover unsupported architectures and platform fallbacks` — `internal/container/seccomp_linux_test.go`, `internal/container/seccomp_linux_unsupported.go`, `internal/container/seccomp_other.go`
09. `feat(doctor): diagnose Linux host and cgroup capabilities` — `internal/container/diagnostics.go`, `internal/container/doctor_linux.go`, `internal/container/doctor_other.go`
10. `test(doctor): cover host diagnostics and warning behavior` — `internal/container/doctor_linux_test.go`
11. `feat(config): parse versioned run configurations` — `internal/config/config.go`, `internal/config/config_test.go`
12. `feat(rootfs): implement safe archive pack and extraction` — `internal/rootfs/archive.go`
13. `test(rootfs): cover archive safety and extraction boundaries` — `internal/rootfs/archive_test.go`
14. `feat(rootfs): add deterministic filesystem digests` — `internal/rootfs/digest.go`
15. `test(rootfs): verify digest and integrity operations` — `internal/rootfs/digest_test.go`
16. `feat(rootfs): audit filesystem permission and symlink risks` — `internal/rootfs/audit.go`
17. `test(rootfs): cover filesystem audit findings` — `internal/rootfs/audit_test.go`, `internal/rootfs/audit_linux_test.go`
18. `feat(state): persist container lifecycle records` — `internal/state/store.go`
19. `test(state): validate state store isolation and metadata` — `internal/state/store_test.go`
20. `feat(state): persist and stream workload logs` — `internal/state/logs.go`
21. `test(state): cover log streaming and secret-safe records` — `internal/state/logs_test.go`
22. `feat(state): forward stop and termination signals` — `internal/state/signals_linux.go`, `internal/state/signals_other.go`
23. `test(state): verify signal forwarding behavior` — `internal/state/signals_linux_test.go`
24. `feat(state): capture exit status and runtime statistics` — `internal/state/exit_linux.go`, `internal/state/exit_other.go`, `internal/state/stats_linux.go`, `internal/state/stats_other.go`
25. `test(state): cover exit status and platform fallbacks` — `internal/state/exit_linux_test.go`
26. `feat(storage): implement named persistent volumes` — `internal/volume/volume.go`
27. `test(storage): cover volume lifecycle and backup handling` — `internal/volume/volume_test.go`
28. `feat(images): add local OCI image layout store` — `internal/image/store.go`
29. `test(images): verify local OCI image store` — `internal/image/store_test.go`
30. `feat(images): build rootfs images from safe recipes` — `internal/image/build.go`
31. `test(images): validate build recipes and ignore rules` — `internal/image/build_test.go`
32. `feat(images): audit local image metadata and rootfs` — `internal/image/audit.go`
33. `test(images): cover image audit integrity and findings` — `internal/image/audit_test.go`
34. `feat(images): compare image filesystems and pin digests` — `internal/image/diff.go`, `internal/image/lock.go`
35. `test(images): detect image tag drift with lock files` — `internal/image/lock_test.go`
36. `feat(images): generate image filesystem inventory reports` — `internal/image/sbom.go`
37. `test(images): verify image inventory and reproducibility` — `internal/image/sbom_test.go`
38. `feat(registry): load Docker and Podman authentication files` — `internal/registry/auth.go`
39. `test(registry): cover auth parsing and bearer token exchange` — `internal/registry/auth_test.go`
40. `feat(registry): pull and push verified OCI images` — `internal/registry/registry.go`
41. `test(registry): validate descriptors platforms and digest checks` — `internal/registry/registry_test.go`
42. `docs: explain runtime design and Go toolchain policy` — `docs/how-it-works.md`, `docs/TOOLCHAIN.md`
43. `docs: publish generated feature capability matrix` — `docs/FEATURE_MATRIX.md`
44. `docs: document user workflows and supported features` — `README.md`
45. `docs: define security policy and contribution process` — `SECURITY.md`, `CONTRIBUTING.md`
46. `ci: add Go tests vet and cross-platform build workflow` — `.github/workflows/ci.yml`
47. `security: automate CodeQL dependency and vulnerability checks` — `.github/workflows/security.yml`, `.github/dependabot.yml`
48. `release: publish multi-platform assets and installers` — `.github/workflows/release.yml`, `install.sh`, `install.ps1`
49. `site: add Boxr website and GitHub Pages deployment` — `.github/workflows/pages.yml`, `website/index.html`, `website/assets/boxr-mark.svg`, `website/assets/site.css`, `website/assets/site.js`
50. `build: add guarded upstream publishing helper and history notes` — `scripts/push-to-main.sh`
