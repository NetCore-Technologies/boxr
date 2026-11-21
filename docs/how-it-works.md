# How boxr Works

This document describes the runtime currently implemented in the repository. It distinguishes available controls from features that remain out of scope.

## Run lifecycle

When a user runs `boxr run --memory 256m --cpus 1 --name worker -- /bin/sh`, the host-side CLI:

1. Loads and validates command-line flags and any strict versioned JSON config. `boxr run --dry-run` stops here after printing a redacted execution plan; it does not create a state record or start a workload.
2. Creates a cryptographically random 12-character local container ID and reserves the requested name in the state record.
3. Starts the same executable as a helper with PID, UTS, mount and IPC namespaces. `--network none` adds a network namespace; `--userns` adds a user namespace.
4. Configures the requested UID/GID map when user namespaces are enabled.
5. Records the helper's host PID and waits for it to become ready.
6. Creates a cgroup v2 child and applies requested controls before releasing the helper.
7. The helper configures hostname, private mount propagation, rootfs mounts/chroot, network loopback, capability drops and no-new-privileges, then forks the workload.
8. The helper forwards signals, waits for the workload and reaps children. The parent removes the cgroup and updates local state/events.

The startup barrier means the workload does not execute before configured cgroup limits have been attached.

## Namespace behaviour

| Namespace | Flag | Behaviour |
|---|---|---|
| PID | `CLONE_NEWPID` | Workload is supervised by a PID-1 helper in a private PID namespace |
| UTS | `CLONE_NEWUTS` | Optional private hostname |
| Mount | `CLONE_NEWNS` | Private mount table and recursive private propagation |
| IPC | `CLONE_NEWIPC` | Isolated System V IPC and POSIX message queues |
| Network | `CLONE_NEWNET` | Opt-in private network namespace with loopback enabled; no veth or external network |
| User | `CLONE_NEWUSER` | Opt-in mapping of the invoking host UID/GID to root in the namespace |

## Root filesystem and mounts

When `--rootfs` is set, boxr optionally mounts an OverlayFS merged view using the rootfs as `lowerdir` and caller-selected persistent `upper`/`work` directories outside the image tree. A private advisory lock prevents concurrent use of the same overlay storage. It then bind-mounts the selected directory or merged view, applies requested bind/tmpfs/proc mounts, applies the optional read-only remount, changes into the root and calls `chroot`. Mount propagation is made private first so mount events are not propagated back to the host.

Bind mount target creation checks for path traversal and symlink escapes. Rootfs archives are extracted to a staging directory and renamed into place only after validation. The extractor rejects special files, duplicate paths, unsafe symlinks and configured size/entry-limit violations.

`boxr rootfs digest` hashes sorted tree paths, object types, permission bits, symlink targets and regular-file contents. It intentionally excludes owners, mtimes and inode numbers. `boxr rootfs diff` compares the same portable tree representation. OverlayFS changes remain in the configured upper directory; the source rootfs is not modified. The overlay mount itself requires a Linux kernel with OverlayFS support and appropriate mount privileges. `boxr rootfs audit PATH` can flag setuid/setgid objects, world-writable paths, special files and lexically escaping relative symlinks before a rootfs is run. `boxr image audit NAME:TAG` unpacks a local OCI image through the safe extractor and runs the same audit on the resulting rootfs. It never follows symlink targets; it is a metadata preflight check, not a malware scanner.

## cgroup v2

Resource controls are opt-in. Supported files include:

- Memory: `memory.max`, `memory.high`, `memory.low`, `memory.min`, `memory.swap.max`, `memory.oom.group`
- CPU: `cpu.max`, `cpu.weight`
- I/O: `io.weight`
- Task count: `pids.max`
- Membership: `cgroup.procs`

The runtime checks controller availability and delegation before creating the child cgroup. It rolls back a partially configured cgroup on setup errors. `boxr stats` reads cgroup memory, CPU, PID and I/O data while that cgroup exists.

## Local state and events

`boxr volume clone SOURCE TARGET` creates a consistent archive snapshot of an unused source volume and restores it as a new, independent volume. Cloning is refused while a registered active container has that source mounted. `boxr volume du` reports a point-in-time logical usage snapshot. It counts regular-file bytes and symlink target lengths without following symlinks; sockets and other special entries are reported separately. Files may change during a walk if an active container is writing to the volume.

Named volumes can be exported and restored with gzip-tar archives. Restore uses the rootfs extractor's path, duplicate-entry, symlink and size checks, stages the data beside the destination, and only publishes the volume after extraction succeeds.

The host-side CLI stores state under `$XDG_STATE_HOME/boxr/containers` or `~/.local/state/boxr/containers`; `BOXR_STATE_DIR` overrides the location. State directories are mode `0700`, JSON records are mode `0600`, and environment values are removed before persistence. Records contain ID, optional name, PID, command, labels, non-secret options, timestamps and exit status. The append-only event journal records create/start/die/remove/prune actions.

The current runtime is foreground/synchronous. `ps`, `inspect`, `events`, `logs`, `stop`, `kill`, `wait`, `rm`, `prune` and `stats` make its lifecycle visible. Events can be filtered by action, container name/ID prefix and a relative or RFC3339 time threshold before the result limit is applied. `ps --last N` selects the newest N records (including completed ones), `--quiet` emits IDs for scripting, and `--no-trunc` preserves full command text. Image listing accepts AND-combined reference-prefix, OS and architecture filters. A second CLI process can signal a running helper through its recorded host PID; there is no daemon or detached start service.

## Current limitations

- `--network none` is loopback-only. There is no veth pair, bridge, NAT, DNS resolver setup or network policy.
- OverlayFS is available as a writable rootfs layer. The local single-layer Boxr OCI-layout store supports OCI Distribution pull/push for Boxr-format images; arbitrary multi-layer OCI/Docker application is not implemented.
- The fixed `baseline` seccomp denylist is opt-in; there is no configurable allowlist, device filter or LSM policy manager.
- Capability reduction and user namespace mappings are best-effort host-kernel-dependent controls; they do not imply a hardened sandbox.
- Container execution is Linux-only. Windows/macOS builds are portable CLI builds for help/version/diagnostics.

Do not run untrusted workloads. See [SECURITY.md](../SECURITY.md).

## Safe image builds

`boxr image build --tag NAME:TAG [--file CONTEXT/Boxrfile.json] [CONTEXT]` reads a bounded, strict JSON recipe with a `scratch` base and explicit `COPY` instructions. It never invokes a shell or runs build commands. Files copied from the context must be regular files or directories; source symlinks are rejected, nested symlinks are lexically checked against the image root, and special files are rejected. Parent directories for file destinations are created automatically.

A context-level `.boxrignore` is optional, bounded to 64 KiB, and must be a regular non-symlink file. It supports comments, blank lines, basename and relative-path globs, and directory-subtree exclusions. Negation patterns and parent traversal are rejected so an excluded directory cannot be re-included by a later rule. An explicit `COPY` of an ignored source fails rather than silently producing a partial image.

Image runtime metadata (`Env`, `Entrypoint`, `Cmd`, `WorkingDir`, `User` and `Labels`) is stored in the OCI config. `boxr run --image` applies the environment, entrypoint/command, working-directory, user and labels as defaults; run configuration and CLI overrides take precedence. Numeric IDs and names from `/etc/passwd` and `/etc/group` are resolved inside the container rootfs, supplementary groups are applied before the final UID drop, and non-root IDs are rejected with the current single-ID user-namespace mapping.

## Local image layout

`boxr image import` packages a prepared rootfs as a content-addressed blob in an OCI image layout. `oci-layout` and `index.json` follow OCI Image Layout conventions; each manifest references an OCI image config and one Boxr-specific gzip-tar rootfs layer. The config records the uncompressed layer digest (`rootfs.diff_ids`). Inspection, save, load and unpack verify descriptor sizes and SHA-256 digests with bounded streaming I/O, then compare the layer's uncompressed digest before using it. Individual blobs are capped at 8 GiB and JSON descriptors at 4 MiB.

Registry transport uses the OCI Distribution API with Bearer-token challenges, scoped access tokens, Docker/Podman inline auth files, platform selection for manifest lists, preserved OS/architecture/variant metadata, content digest checks and bounded layer downloads/uploads. Use `--authfile PATH` on `image pull` or `image push`; Boxr also discovers `REGISTRY_AUTH_FILE`, `DOCKER_CONFIG/config.json` and standard home-directory auth files. External credential-helper programs are intentionally never executed. A push first verifies and copies local blobs into private temporary storage, then mounts or uploads the config and layer blobs before publishing the manifest. Upload URLs must remain on the registry origin. `image pull` and `image push` support Boxr's custom single-rootfs-layer media type only; common Docker images with multiple compressed layers and whiteout semantics are rejected explicitly. Portable `image save` archives contain a selected OCI layout reference and its reachable blobs. `image prune --dry-run` reports orphaned blobs; deletion requires the explicit `--force` option.

## Seccomp baseline profile

`--seccomp-profile baseline` enables `no_new_privileges` and installs an architecture-guarded classic BPF seccomp filter before the workload is exec'd. The current fixed denylist blocks selected namespace and mount administration, kernel module/BPF interfaces, process tracing/cross-process memory inspection, perf events, userfaultfd, reboot and key-management syscalls. Other syscalls remain allowed. A syscall ABI mismatch kills the filtered process rather than applying the wrong architecture table.

This profile is an optional defense-in-depth reduction of kernel attack surface, not a complete syscall allowlist or a hardened sandbox. It cannot substitute for correct user/mount namespaces, capability reduction, LSM policy, device filtering, kernel hardening or a real untrusted-code threat model. It is supported on Linux amd64 and arm64 only.


## Image security audit

`boxr image audit NAME:TAG` first verifies the local OCI manifest, config and layer descriptors, the compressed layer digest, and the uncompressed rootfs diff ID. It copies the verified layer into a private temporary snapshot while holding the image-store lock, rechecks the snapshot digest, then extracts it with the rootfs archive safety limits and runs the metadata audit without following symlinks. The JSON report combines image platform and descriptor metadata with filesystem findings. `--max-entries` bounds the scan; `--strict` returns non-zero for warning or high-severity findings. This is a filesystem metadata preflight, not malware detection or a guarantee that an image is safe.


## Reproducible rootfs archives

Rootfs archives are written in sorted path order with normalized gzip and tar timestamps, zero UID/GID metadata, and no host account names. Equivalent filesystem trees therefore produce byte-identical archive bytes even when their source files were created or modified at different times. This makes rootfs layer digests stable across rebuilds when file content, paths, object types, and permission bits are unchanged.
