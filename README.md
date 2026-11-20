<div align="center">

# 📦 boxr

**Linux containers, down to the kernel.**

[![CI](https://github.com/NetCore-Technologies/boxr/actions/workflows/ci.yml/badge.svg)](https://github.com/NetCore-Technologies/boxr/actions/workflows/ci.yml)
[![Security](https://github.com/NetCore-Technologies/boxr/actions/workflows/security.yml/badge.svg)](https://github.com/NetCore-Technologies/boxr/actions/workflows/security.yml)
[![Go](https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go)](https://go.dev/)
[![License](https://img.shields.io/badge/license-MIT-green)](LICENSE)

[Project website](https://netcore-technologies.github.io/boxr/) · [Releases](https://github.com/NetCore-Technologies/boxr/releases) · [Feature matrix](docs/FEATURE_MATRIX.md)

</div>

## What is boxr?

boxr is a small, daemonless Linux container runtime built with Go, Linux namespaces and cgroup v2. It is designed to make the kernel primitives behind containers understandable while providing useful command workflows: root filesystem preparation, run configuration, named container records, resource inspection and reproducible release assets.

It is an early-stage runtime and **not a hardened security boundary or Docker replacement**. Do not run untrusted workloads with boxr.

## Capabilities

- **Namespaces:** PID, UTS, mount, IPC, optional private network namespace and optional user namespace with a single UID/GID mapping.
- **Filesystem:** optional chroot rootfs, private mount propagation, read-only rootfs, proc, recursive bind mounts, read-only bind mounts, bounded tmpfs mounts and optional persistent OverlayFS writable layers (`--overlay-dir`).
- **Security controls:** capability drops, `no_new_privileges`, and an opt-in architecture-guarded seccomp baseline denylist. These are defense-in-depth controls, not a sandbox guarantee.
- **cgroup v2:** memory max/high/low/min/swap, CPU quota/weight, I/O weight, PID limits and group OOM policy. The blocked helper is attached before workload execution.
- **Configuration and deployment:** versioned JSON run files, strict multi-service manifests, dependency-aware parallel startup, bounded TCP/HTTP loopback readiness gates and secret-redacted plans.
- **Local lifecycle:** persistent records, `ps`, `inspect`, `stats`, `events`, `logs` (tail/follow), `stop` (graceful TERM then KILL), `kill`, `wait`, `rm`, automatic record removal and age-based pruning.
- **Persistent storage:** named, owner-private volumes with labels, name/label filtering, inspection, listing, safe removal, usage protection, filtered pruning, SHA-256-described gzip-tar export/import, and `--volume NAME:/PATH[:ro|rw]` mounts (requires `--rootfs`). Export archives are atomically published and never overwrite an existing backup. OverlayFS upper/work data can also persist between runs.
- **Rootfs workflows:** pack/unpack gzip-tar archives, inspect and verify rootfs, deterministic SHA-256 tree digest and directory diff.
- **Tooling:** host diagnostics, JSON output (including `boxr features --json`), shell completions, cross-platform CLI builds, CI, vulnerability scanning, CodeQL, release checksums and GitHub Pages.

The CLI builds for Linux amd64/arm64, Windows amd64, and macOS amd64/arm64. **Only Linux supports container execution.** Windows and macOS builds are CLI-only.

## Install

The release installer downloads the correct Linux architecture, verifies the archive against the release checksum manifest, then installs it:

```bash
curl -fsSL https://raw.githubusercontent.com/NetCore-Technologies/boxr/main/install.sh -o boxr-install.sh
bash boxr-install.sh
```

Use `BOXR_VERSION=vX.Y.Z` to select a version, or `BOXR_INSTALL_DIR` to choose an installation directory. Debian/Ubuntu users can install the matching `.deb` release asset.

Windows CLI-only installer:

```powershell
irm https://raw.githubusercontent.com/NetCore-Technologies/boxr/main/install.ps1 | iex
```

## Build from source

Use a supported Go release (Go 1.26.11 or Go 1.27.4 at the current toolchain review). The module keeps a Go 1.23 language floor, while CI tests both currently supported Go release lines.

```bash
git clone https://github.com/NetCore-Technologies/boxr.git
cd boxr
make test
make build
```

Useful targets:

```bash
make test        # race-enabled tests and coverage
make vet         # go vet ./...
make lint        # gofmt check and vet
make cross-build # Linux, Windows and macOS CLI binaries
make verify      # formatting, vet, race tests and five cross-builds
make clean       # remove generated files
```

## Quick start

Container execution requires Linux and the necessary namespace permissions. Root or a delegated set of capabilities is usually required.

```bash
sudo boxr doctor

# Validate a plan without starting a process or writing a container record.
boxr run --dry-run --name preview --env APP_MODE=dev -- /bin/echo hello

sudo boxr run --hostname worker --memory 256m --cpus 1 -- /bin/sh
```

OverlayFS keeps writes in a persistent upper directory while leaving the supplied rootfs unchanged. The upper/work storage must be on a filesystem that supports OverlayFS; one directory cannot be used by two active containers at once:

```bash
sudo boxr run --rootfs /var/lib/boxr/rootfs/alpine \
  --overlay-dir /var/lib/boxr/overlays/alpine-dev --mount-proc -- /bin/sh
```

The upper layer persists across runs using the same overlay directory. It is separate from the content-addressed local image store described below.

Named volumes persist independently of containers. Prepare a root filesystem first, then mount a volume into it:

```bash
boxr volume create app-data
sudo boxr run --rootfs /var/lib/boxr/rootfs/alpine --volume app-data:/var/lib/app --mount-proc --overlay-dir /var/lib/boxr/overlays/app -- /bin/sh
```

A container can be named, labelled and given an isolated root filesystem:

```bash
sudo boxr run \
  --name web \
  --label app=frontend \
  --hostname web \
  --rootfs /var/lib/boxr/rootfs/alpine \
  --mount-proc \
  --read-only-rootfs \
  --tmpfs /tmp:64m:1777 \
  --network none \
  --userns \
  --no-new-privileges \
  --seccomp-profile baseline \
  --cap-drop NET_RAW \
  --memory 256m --cpus 1 --pids-limit 128 \
  -- /bin/sh
```

`--network none` creates a private network namespace and enables loopback; it does not configure a veth pair, bridge, NAT, DNS or outbound internet access.

## Container workflow

```bash
boxr ps                         # active/created containers
boxr ps --all                   # include completed records
boxr ps --last 5 --quiet        # IDs of the five newest records
boxr ps --all --no-trunc        # show full command strings
boxr ps --all --filter label=app=frontend
boxr inspect web                # JSON state and configuration
boxr logs --tail 100 web         # recent stdout/stderr output
boxr logs --follow web           # stream output until exit
boxr stop --time 10s web         # graceful stop, then forced kill
boxr kill --signal KILL web      # send a named signal
boxr wait web                    # print the final exit code
boxr stats web                  # live cgroup metrics; requires an active cgroup
boxr events --limit 50          # recent lifecycle events
boxr events --action die --since 1h --format json
boxr events --container web --action start
boxr rm web                     # remove a completed record
boxr prune --older-than 168h    # remove old completed records

boxr volume create --label app=web web-data
boxr volume ls
boxr volume inspect web-data
boxr volume clone web-data web-data-copy
boxr volume du --json web-data  # logical snapshot; symlinks are not followed
boxr volume export --json web-data web-data-backup.tar.gz
# Export refuses to replace an existing file; JSON includes the SHA-256 digest.
boxr volume import web-data-backup.tar.gz web-data-restored
boxr volume import web-data-backup.tar.gz web-data-restored
boxr volume ls --filter label=app=web
boxr volume rm web-data          # deletes its data; active use is blocked
boxr volume prune --force        # deletes all unused volumes and their data
```

State is stored under `$XDG_STATE_HOME/boxr/containers` or `~/.local/state/boxr/containers`; set `BOXR_STATE_DIR` to override it. Volume data is stored under `$XDG_DATA_HOME/boxr/volumes` or `~/.local/share/boxr/volumes`; set `BOXR_DATA_DIR` to override it. Volume directories use mode `0700` and metadata files use `0600`. State directories use mode `0700`, record files use `0600`, and environment values are deliberately not persisted. Command arguments and labels are persisted for inspection, so do not place secrets in command-line arguments or labels. `boxr run --rm` removes the state record after the workload exits.

## Root filesystem workflow

```bash
boxr rootfs inspect ./rootfs
boxr rootfs verify ./rootfs
boxr rootfs digest ./rootfs
boxr rootfs verify ./rootfs sha256:EXPECTED_DIGEST
boxr rootfs pack ./rootfs alpine-rootfs.tar.gz
boxr rootfs unpack alpine-rootfs.tar.gz ./unpacked-rootfs
boxr rootfs diff ./rootfs ./unpacked-rootfs
```

Extraction rejects path traversal, escaping symlinks, duplicate paths, special device files and archives beyond configured file/byte limits. Extraction is staged and atomically renamed into a new destination. File ownership and setuid/setgid metadata are not preserved as a promise of this archive format; review rootfs contents before running them with elevated privileges.

## Build a local image safely

Boxr includes a deliberately small, non-executing JSON image recipe. It supports a `scratch` base or an already-present local OCI image, explicit `COPY` operations, and runtime metadata. A local base is fully integrity-verified and platform-checked before its filesystem is safely extracted; Boxr never performs an implicit network pull during a build. Pull a registry base explicitly with `boxr image pull REGISTRY/IMAGE:TAG` first. Child recipes inherit the base environment, entrypoint, command, working directory, user and labels; supplied child values override the base (environment and labels merge by key). The result is stored as one canonical Boxr rootfs layer, so original base-layer history is not retained. Build writes resolve symlinks only when their targets remain inside the staged rootfs; container-absolute links are normalized to rootfs-relative paths, and a host symlink cannot redirect a later `COPY` or working-directory creation into the host filesystem. It does **not** execute shell commands, package managers or arbitrary build steps; use a separate build environment to produce binaries before copying them into an image.

```json
{
  "version": 1,
  "base": "scratch",
  "copy": [
    { "source": "app", "destination": "/opt/app" }
  ],
  "config": {
    "Env": ["APP_MODE=production"],
    "Entrypoint": ["/opt/app/server"],
    "Cmd": ["--listen", "0.0.0.0:8080"],
    "WorkingDir": "/opt/app",
    "Labels": { "org.example.component": "web" }
  }
}
```

Save this as `Boxrfile.json`. To build on another image already in the local store, change `base` to a local tag such as `example/runtime:1`; run `boxr image pull REGISTRY/IMAGE:TAG` first for a registry image. A missing local base is an error rather than an implicit network pull. Omitted runtime fields inherit from the base, while child environment and labels override matching keys. An optional `.boxrignore` in the context supports comments, blank lines, basename/path globs and directory exclusions; negation rules are intentionally unsupported. For example:

```text
# generated files and secrets never enter the image
.git/
node_modules/
*.env
*.key
```

Build, verify and inspect the resulting image:

```bash
boxr image build --tag example/web:dev .
# Declare the target architecture when preparing a foreign-platform rootfs.
boxr image build --platform linux/arm64/v8 --tag example/web:arm64 .
boxr image verify example/web:dev
boxr image inspect example/web:dev
boxr image audit --strict example/web:dev
boxr image diff --json example/web:v1 example/web:v2
boxr image lock example/web:dev boxr.lock.json
boxr image verify-lock boxr.lock.json
boxr image sbom example/web:dev > example-web.sbom.json
```

Use `--file PATH` to select another recipe, `--platform linux/ARCH[/VARIANT]` to declare a Linux target (including `linux/arm64/v8` and `linux/arm/v7`), `--json` for machine-readable build output, or `--quiet` to print only the tag. Platform selection declares image metadata; it does not emulate or cross-compile copied binaries, so supply files built for the selected target. Runtime `Env`, `Entrypoint`, `Cmd`, `WorkingDir` and labels are inherited by `boxr run --image`; run-config and CLI values override image defaults. `boxr image diff BEFORE:TAG AFTER:TAG` compares verified root filesystems and reports added, removed and modified paths (use `--json` for automation). `boxr image lock TAG FILE` writes a private, versioned lock file with the manifest digest and platform; `boxr image verify-lock FILE` detects if the local tag has moved since the lock was created. `boxr image sbom TAG` emits a Boxr filesystem inventory with per-file SHA-256 values, modes, symlink targets and image provenance. This is a filesystem inventory, not a package-manager dependency SBOM. Images that declare a non-empty `User` are rejected at run time until identity switching is supported, rather than silently running as the wrong user.

## Local image workflow

Boxr can import a prepared rootfs directory into a local OCI image-layout store using a Boxr-specific compressed rootfs layer media type. The store uses content-addressed SHA-256 blobs, validates descriptors and layer diff IDs, and supports portable OCI-layout archives. Rootfs tar creation normalizes timestamps and owner metadata so equivalent filesystem trees produce byte-identical layer archives across build times. Boxr also imports standard Docker save archives through `boxr image load`: plain tar and gzip-compressed tar archives are accepted, every uncompressed layer diff ID is verified, layers are applied in order with Docker whiteout and opaque-directory handling, supported runtime settings and platform metadata are preserved, and all listed repository tags are imported. Imported layered images are flattened into Boxr's native rootfs layer. OCI Distribution pulls accept standard OCI and Docker tar/tar+gzip layer media types, verify every compressed blob digest and uncompressed diff ID, and safely apply layers in order with whiteout and opaque-directory semantics before importing the resulting filesystem as Boxr's canonical single-layer image. Zstandard-compressed layers remain unsupported; registry pushes still publish Boxr's own canonical single-rootfs layer. See [registry image support](docs/registry-images.md) and [Docker archive import](docs/docker-archive.md).

```bash
boxr image import ./rootfs alpine:local
boxr image push alpine:local ghcr.io/your-org/alpine-boxr:v1
boxr image pull --platform linux/amd64 ghcr.io/your-org/alpine-boxr:v1
boxr image ls
boxr image ls --filter os=linux --filter arch=amd64
boxr image inspect alpine:local
boxr image verify alpine:local       # verifies manifest, config, layer and diff ID
boxr image du --json                 # unique, reachable and reclaimable bytes
boxr image tag alpine:local alpine:stable
boxr image save alpine:local alpine-local.oci.tar.gz
boxr image save --format docker alpine:local alpine-local.docker.tar
boxr image save --format docker alpine:local alpine:stable example/sidecar:v1 image-bundle.docker.tar
docker load --input alpine-local.docker.tar
boxr image rm alpine:local
boxr image load alpine-local.oci.tar.gz
docker save -o alpine.docker.tar alpine:latest
boxr image load alpine.docker.tar
boxr image unpack alpine:local ./rootfs-restored
boxr image prune --dry-run
boxr image prune --force
```

The store defaults to `$XDG_DATA_HOME/boxr/images` or `~/.local/share/boxr/images`; set `BOXR_IMAGE_DIR` (or `BOXR_DATA_DIR`) to override it. Registry credentials are read from `BOXR_REGISTRY_USERNAME` and `BOXR_REGISTRY_PASSWORD`; do not put passwords on the command line. `image push [--authfile PATH] LOCAL:TAG [REMOTE:TAG]` publishes a local image in Boxr's canonical single-rootfs-layer format and `image pull [--platform linux/ARCH[/VARIANT]] [--authfile PATH] REMOTE[:TAG|@sha256:DIGEST]` fetches supported schema-2 OCI or Docker images, including multi-layer images (the default platform is Linux on the host architecture). Selected OS, architecture and variant metadata are preserved in the local image config, including when pulling a foreign architecture. Docker Hub is the default registry; use a registry-qualified name for GHCR or a private registry. Immutable digest pulls are assigned a deterministic local tag. `image rm` removes a tag without deleting shared blobs. `image verify` checks the complete image descriptor graph and compressed/uncompressed layer digests. `image audit [--json] [--strict] NAME:TAG` combines full integrity verification with a bounded rootfs security scan; its report includes manifest/config/layer digests, platform metadata and filesystem findings. The verified layer is snapshotted while the image-store lock is held before scanning, so concurrent pruning cannot invalidate the audited input. `image du` audits every blob against its content-addressed filename and reports unique, reachable and reclaimable storage. `image prune` requires an explicit `--dry-run` or `--force`; the latter deletes unreferenced content-addressed blobs. `run --dry-run` validates runtime options and prints a redacted plan; environment values, label values and raw arguments are not emitted, and no container record is created.

## Multi-service deployment planning

Create and validate a declarative deployment before starting any workload:

```sh
boxr volume create db-data
boxr deploy example > boxr.deploy.json
boxr deploy validate boxr.deploy.json
boxr deploy plan boxr.deploy.json
boxr deploy plan --json boxr.deploy.json
boxr deploy profiles boxr.deploy.json
boxr deploy profiles --json boxr.deploy.json
boxr deploy pull boxr.deploy.json
boxr deploy up boxr.deploy.json
boxr deploy ps sample-app
boxr deploy logs --tail 100 sample-app
boxr deploy wait --condition running sample-app
boxr deploy down sample-app
```

Deployment manifests define named services, optional local OCI images, commands, existing Boxr container options, named persistent volume mounts, `dependsOn` services and optional `profiles` for opt-in service groups. Services without a profile are always active; profile-bound services stay disabled unless one of their profiles is selected. Repeat `--profile NAME` to activate several groups together, for example `boxr deploy plan --profile diagnostics --profile frontend boxr.deploy.json` or `boxr deploy up --profile frontend boxr.deploy.json`. Add a `profiles` array to a service to opt it into one or more groups. Run `boxr deploy profiles FILE.json` to inspect always-enabled services and the services in every available profile; add `--json` for automation. The inventory is sorted and omits commands, environment values and labels. A service may set `startTimeoutMs` (10–600000) to override the stack-wide startup timeout for its running-state and readiness gate; omit it or use `0` to inherit `--start-timeout`. A service may similarly set `stopTimeoutMs` (10–600000) for its graceful shutdown period. It overrides the stack-wide `deploy down --time` value; the persisted setting applies to `deploy down`, Ctrl+C shutdown and failed-start rollback. Services without an override continue to inherit the command-line value. A service may also define up to 16 `envFiles` absolute paths; Boxr loads them in declaration order, with later files overriding earlier values and explicit `options.env` taking precedence over all files. Environment files are read during service preflight, before any service is created. Plans expose only the number of environment files, never their paths or values. Every service is validated before filtering, and an enabled service cannot depend on a disabled service; use an unprofiled base service or select the dependency's profile as well. The selected service set is used consistently by planning, image prefetch and startup. Boxr rejects missing dependencies, duplicate edges, invalid run options and cycles, then produces a stable dependency-first order, starting independent services concurrently within each dependency wave. Run `boxr deploy pull FILE.json` first to prefetch and verify each unique service image (with optional `--platform`, `--authfile` and repeatable `--profile`), and checks cached images against the requested Linux architecture; `deploy up` only starts when every selected image is available locally for the host platform. Use `boxr deploy up FILE.json` for attached, fail-fast startup; `boxr deploy ps STACK` for stack-scoped status; and `boxr deploy down STACK` for reverse-order graceful shutdown. `deploy down --remove` also removes stopped records and logs. `boxr deploy logs [--tail N] [--follow] STACK [SERVICE]` prints service-prefixed output from every service or a selected service. `boxr deploy wait --condition running|stopped STACK` waits for the whole stack to reach the requested state and supports a timeout, polling interval and secret-redacted JSON result. Named volumes refer to volumes created with `boxr volume create`; Boxr verifies every volume and mount before creating the first service. Service dependencies gate startup order; configured loopback TCP/HTTP probes additionally gate dependent waves until the application returns the expected status. Probes run once during startup; recurring liveness checks, restart policies, service DNS, bridge networking and detached daemon operation remain future work.

## Run configuration

Generate an example config and validate it before execution:

```bash
boxr config example > boxr.run.json
boxr config validate boxr.run.json
sudo boxr run --config boxr.run.json
```

CLI `--env` values override env-file values, which override values from the config. Duplicate keys collapse to a single final value.

## Host diagnostics and inspection

```bash
boxr info --json
boxr doctor --json
boxr doctor --strict
boxr version --json
boxr features
boxr features --json
boxr completion bash
```

`doctor` is read-only. `--strict` makes warnings fail the command, which is useful in automation. `events` can filter by action, container name/ID prefix, relative duration or RFC3339 timestamp; filtering is applied before `--limit`. `stats` reads cgroup v2 files and is only available while the runtime cgroup exists. `run --dry-run` prints a redacted plan without starting a process or writing a container state record.

## Runtime requirements and security limits

- Linux kernel with support for PID, UTS, mount and IPC namespaces. Network and user namespaces are optional runtime flags.
- cgroup v2 must be mounted and writable with requested controllers delegated for resource-limit options.
- `--rootfs` is an existing directory owned/prepared by the operator. `boxr image pull` supports schema-2 OCI and Docker distribution manifests with up to 128 verified tar or gzip layers; the merged filesystem is flattened into Boxr's canonical local image layer. `boxr image push` still publishes the canonical single-layer format. For offline transfers, import and export Docker-compatible archives with `docker save`/`docker load` and `boxr image load`/`boxr image save --format docker`.
- `--network none` has no network provisioning; only loopback is enabled. `host` is the default network mode. `--seccomp-profile baseline` enables a fixed architecture-specific denylist and automatically enables no-new-privileges; it is defense-in-depth, not a sandbox policy.
- Named volumes require `--rootfs`; they are not a host-wide container filesystem abstraction and must not be treated as encrypted storage.
- OverlayFS provides a persistent writable rootfs view. Registry pulls validate and apply each OCI/Docker layer before flattening the merged filesystem into the local OCI-layout image store; registry pushes still emit Boxr's canonical single-rootfs-layer representation. Boxr does not preserve arbitrary multi-layer graphs for push or provide Docker-daemon integration, customizable seccomp profiles, device filtering, LSM policy management, veth/bridge/NAT networking or daemonized detached containers.
- Host kernel bugs, namespace escape vulnerabilities, privileged mounts and workload arguments remain outside boxr's guarantees. Do not use it for hostile multi-tenant workloads.

Read [SECURITY.md](SECURITY.md) before using the runtime. See [how it works](docs/how-it-works.md), the [implemented feature matrix](docs/FEATURE_MATRIX.md), and [toolchain review](docs/TOOLCHAIN.md).

## Contributing and license

See [CONTRIBUTING.md](CONTRIBUTING.md). The project is MIT licensed; see [LICENSE](LICENSE).


## Registry authentication

Use `boxr image pull --authfile ~/.docker/config.json REGISTRY/IMAGE:TAG` or the equivalent `boxr image push --authfile PATH LOCAL:TAG REGISTRY/IMAGE:TAG`. Boxr supports inline credentials and tokens in Docker `config.json` and Podman `auth.json`, and checks `REGISTRY_AUTH_FILE`, `DOCKER_CONFIG`, and the standard home-directory locations. External Docker credential-helper executables are not launched by Boxr. Environment credentials `BOXR_REGISTRY_USERNAME` and `BOXR_REGISTRY_PASSWORD` take precedence.

## Security and resource presets

`boxr run` supports mandatory security presets and resource defaults:

```bash
# Apply a practical hardening baseline (seccomp, no-new-privileges, NET_RAW dropped)
boxr run --security-profile baseline -- /bin/sh

# Disable container networking, use a private user namespace and drop high-risk capabilities
boxr run --security-profile restricted -- /bin/sh

# Require a rootfs or image and enforce a read-only rootfs plus the restricted controls
boxr run --security-profile untrusted --image local/alpine:latest -- /bin/sh

# Fill unset CPU, memory, PID and weight limits; explicit flags still take precedence
boxr run --resource-profile small -- /bin/sh
```

Security profiles are applied after configuration and command-line parsing, so a weaker flag cannot override a mandatory profile setting. `untrusted` is still not a hardened security boundary; kernel configuration and host policy remain important.

## Operational storage and configuration tooling

```bash
boxr system df --json
boxr config schema > boxr-run-config.schema.json
boxr image ls --filter variant=v8 --filter label=team=platform
```

`system df` reports verified image-store bytes, named-volume logical usage and container state/log usage without following symlinks. The JSON Schema is generated by `boxr config schema` for the strict version 1 run-config format.

Create a privacy-preserving support archive with `boxr support-bundle --output boxr-support.zip`. It contains build/runtime metadata (including the host kernel release and cgroup v2 availability) plus diagnostic names/statuses; it excludes environment values, command arguments, labels, host paths, configuration files, logs, and container state. See [the support bundle guide](docs/support-bundles.md).


## Live container statistics

Read a single resource snapshot or stream timestamped JSON Lines samples:

```bash
boxr stats CONTAINER
boxr stats --watch CONTAINER
boxr stats --watch --interval 2s --count 10 CONTAINER
```

Watch mode emits compact JSON Lines by default with a UTC `sampledAt` timestamp and the existing statistics payload under `stats`; use `--format table` for a readable timestamped table. The default interval is one second (minimum 100 ms). A count of zero continues until the container exits; a positive count is useful for bounded scripts. The original one-shot JSON output remains unchanged. See [runtime monitoring](docs/runtime-monitoring.md).


## Follow container events

Stream new lifecycle events as they are recorded:

```bash
boxr events --follow
boxr events --follow --action die --container web
boxr events --follow --format json --interval 250ms --count 25
boxr events --follow --since 10m --format json
```

Follow mode starts at the current end of the event log unless `--since` is supplied for a retained-history catch-up. JSON output is one event per line. The default poll interval is 500 ms (minimum 100 ms); a positive `--count` stops after that many matching events. See [event streaming](docs/event-streaming.md).


## Wait for container readiness or completion

Use `wait` as an orchestration gate:

```bash
boxr wait --condition running --timeout 10s web
boxr wait --condition stopped --timeout 30m web
boxr wait --condition running --json api worker
```

The default condition remains `stopped`, preserving the existing exit-code output. `--condition running` waits for startup, but fails if the container exits before becoming ready. Polling defaults to 100 ms (minimum 10 ms), and `--json` returns the container ID, resulting status, exit code and elapsed milliseconds. See [container orchestration](docs/orchestration.md).


## Preview container cleanup

Preview eligible records before deleting them:

```bash
boxr prune --dry-run
boxr prune --older-than 24h --dry-run --json
boxr prune --older-than 7d
```

Dry-run mode reports candidate IDs, names, states and the selected age threshold without changing container records or logs. Running and never-started containers are excluded. The existing mutating prune behaviour remains the default. See [safe cleanup](docs/pruning.md).


## Preview named-volume cleanup

```bash
boxr volume prune --dry-run
boxr volume prune --dry-run --json --filter label=team=demo
boxr volume prune --force
```

Dry-run mode lists eligible volumes and identifies volumes protected by active containers. It accepts the same name and label filters as the destructive operation, but does not require `--force` and never removes data. Actual cleanup still requires explicit `--force`. See [volume cleanup](docs/pruning.md).


## Inspect container disk usage

```bash
boxr inspect CONTAINER
boxr inspect --size CONTAINER
```

The optional size report includes separate state-record and log byte counts plus their total. It uses no-follow file metadata checks and treats a missing log as zero bytes; symlink and special-file targets are rejected. The original inspect JSON remains unchanged unless `--size` is supplied.


For a human-readable snapshot, use `boxr stats --format table CONTAINER`. The same format is available with `--watch`; the table header is emitted once followed by one timestamped row per sample.


## Inventory all named volumes

```bash
boxr volume du --all
boxr volume du --all --json
boxr volume du --all --filter label=team=demo --json
```

The all-volume view reports per-volume file, directory, symlink and byte counts plus aggregate totals. Name and label filters let scripts target a subset; the original single-volume `boxr volume du NAME` command remains unchanged. See [volume storage usage](docs/volume-usage.md).


## Inspect storage by image tag

```bash
boxr image du --all
boxr image du --all --json
boxr image du --all --filter os=linux --filter arch=arm64 --json
```

The all-image view lists each tag's rootfs layer bytes and platform, alongside the verified store-wide unique-blob totals. Referenced layer bytes are summed per tag, so the same shared layer may appear more than once; use the unique store total for physical blob storage. The original `boxr image du` summary remains unchanged. See [image storage usage](docs/image-usage.md).


## Review image cleanup before deletion

```bash
boxr image prune --dry-run --json
boxr image prune --force --json
```

The JSON report lists every unreferenced blob digest and size, plus aggregate reclaimable bytes. Dry-run mode does not remove data; destructive cleanup still requires explicit `--force`. See [image pruning](docs/image-pruning.md).
