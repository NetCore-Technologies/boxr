# Security policy

## Security posture

boxr is an early-stage Linux container runtime. It composes namespaces, mounts, cgroup v2 controls, a small PID-1 supervisor, optional chroot rootfs, private network/user namespaces, capability controls and an optional fixed seccomp syscall denylist. These mechanisms reduce ambient visibility and resource use, but **boxr is not a hardened security boundary**.

Do not use boxr to run untrusted code, hostile multi-tenant workloads, or workloads that require Docker/containerd-level isolation guarantees. The runtime supports an optional OverlayFS writable rootfs layer and uses a symlink-resistant advisory lock for each configured upper/work directory. Its local OCI-layout image store handles a single Boxr-specific rootfs layer and can pull/push that format through OCI Distribution registries with digest verification. It does not apply arbitrary multi-layer Docker/OCI images or whiteout semantics. Registry credentials supplied through `BOXR_REGISTRY_USERNAME` and `BOXR_REGISTRY_PASSWORD` or inline Docker/Podman auth files are sent only to the registry or a trusted token host; use protected environment injection on shared systems. Auth files must be regular non-symlink files no larger than 1 MiB. Boxr never executes external credential-helper programs. The optional `baseline` seccomp profile is a fixed denylist for selected kernel administration, tracing and cross-process operations; it is not a configurable allowlist, does not filter devices, and must not be treated as a security boundary. The runtime does not currently provide device filtering, veth/bridge/NAT provisioning, arbitrary OCI layer application, LSM policy management, or a daemonized isolation supervisor. `--network none` creates a network namespace with loopback only; it does not create a connected network.

The local container state store uses a private directory and `0600` records. Environment variable values are not written to state. Command arguments, labels and selected runtime configuration are saved for inspection; do not place secrets in command-line arguments or labels.

Rootfs extraction rejects path traversal, unsafe symlinks, duplicate entries, special files and oversized archives. `boxr rootfs audit PATH` and `boxr image audit NAME:TAG` provide a metadata preflight for setuid/setgid objects, world-writable paths, special files and relative symlink escapes; the audit does not follow symlink targets and is not a malware scanner. Review rootfs content before running it with elevated privileges.

## Reporting a vulnerability

Please do not open a public issue containing exploit details. Use GitHub's private vulnerability reporting for this repository, or contact maintainers through the private security contact listed in the repository's GitHub Security tab.

Include the affected version/commit, Linux distribution and kernel version, required privileges, reproduction steps and potential impact. Do not include real secrets or personal data.

## Response goals

Maintainers will acknowledge reports as soon as practical, assess severity and affected versions, and coordinate a fix and disclosure timeline with the reporter. These are goals, not a guaranteed service-level agreement.


## Registry credential files

Boxr reads Docker/Podman auth files as data. It does not execute Docker credential-helper binaries or shell commands referenced by those files. Auth files must be regular non-symlink files no larger than 1 MiB. Avoid sharing auth files, and revoke credentials if an auth file is exposed.
