<div align="center">

# 📦 boxr

**Linux containers, down to the kernel.**

A local, limited Docker Engine API is available through an owner-only Unix socket: run `boxr daemon` and point a compatible client at the printed `unix://` socket. The API surface includes ping, version, info, container create/start/stop/list/inspect/logs/stats/events, image pull/push/list/inspect/remove, and local named-volume create/list/inspect/delete operations, plus validated kill signals and safe removal of created/stopped containers. Container restart, image build, digest-form Engine API pulls, Docker Engine named-volume mounts, TCP listeners and full Docker API compatibility are still missing. Engine API push publishes Boxr's verified single-layer OCI image format; it does not preserve Docker multi-layer history. Container creation currently requires a verified image already present in the local image store. See the [Engine API compatibility guide](docs/engine-api.md) for the endpoint list and current limitations.

[![CI](https://github.com/NetCore-Technologies/boxr/actions/workflows/ci.yml/badge.svg)](https://github.com/NetCore-Technologies/boxr/actions/workflows/ci.yml)
[![Security](https://github.com/NetCore-Technologies/boxr/actions/workflows/security.yml/badge.svg)](https://github.com/NetCore-Technologies/boxr/actions/workflows/security.yml)
[![Go](https://img.shields.io/badge/Go-1.25%2B-00ADD8?logo=go)](https://go.dev/)
[![License](https://img.shields.io/badge/license-MIT-green)](LICENSE)

[Project website](https://netcore-technologies.github.io/boxr/) · [Releases](https://github.com/NetCore-Technologies/boxr/releases) · [Feature matrix](docs/FEATURE_MATRIX.md)

</div>

## What is boxr?

boxr is a small, daemonless Linux container runtime built with Go, Linux namespaces and cgroup v2. It is designed to make the kernel primitives behind containers understandable while providing useful command workflows: root filesystem preparation, run configuration, named container records, resource inspection and reproducible release assets.

It is an early-stage runtime and **not a hardened security boundary or Docker replacement**. Do not run untrusted workloads with boxr.

## Capabilities

- **Namespaces and networking:** PID, UTS, mount, IPC, optional user namespace with a single UID/GID mapping, host or isolated networking, and managed IPv4 Linux bridge networks with veth attachment, per-network IPAM, gateway routes, optional per-network NAT, and TCP/UDP host port publishing with scoped DNAT rules.
- **Filesystem:** optional chroot rootfs, private mount propagation, read-only rootfs, proc, recursive bind mounts, read-only bind mounts, bounded tmpfs mounts and optional persistent OverlayFS writable layers (`--overlay-dir`).
- **Security controls:** capability drops, `no_new_privileges`, and an opt-in architecture-guarded seccomp baseline denylist. These are defense-in-depth controls, not a sandbox guarantee.
- **cgroup v2:** memory max/high/low/min/swap, CPU quota/weight, I/O weight, PID limits and group OOM policy. The blocked helper is attached before workload execution.
- **Configuration and deployment:** versioned JSON run files, strict multi-service manifests, dependency-aware parallel startup, bounded TCP/HTTP loopback readiness gates, service-specific resource and lifecycle budgets, published TCP/UDP ports, and secret-redacted plans.
- **Local lifecycle:** persistent records, foreground and detached per-container supervisors, bounded detached restart policies, `ps`, `inspect`, `stats`, `events`, `logs` (tail/follow), `stop` (graceful TERM then KILL), `kill`, `wait`, `rm`, automatic record removal and age-based pruning.
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

Use a supported Go release (Go 1.26.11 or Go 1.27.4 at the current toolchain review). The module requires Go 1.25 or newer for its patched Zstandard decoder; CI tests the Go 1.25, 1.26 and 1.27 release lines.

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

# Start a background workload that survives the launching terminal.
sudo boxr run --detach --name background-job -- /usr/bin/sleep 3600
boxr ps
boxr logs --follow background-job
sudo boxr stop --time 10s background-job
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

### Detached containers

On Linux, `boxr run --detach` (or `-d`) starts a separate Boxr supervisor in a new session. The launching CLI waits until the container's namespace helper has passed cgroup configuration and the startup barrier has been released, then prints the container ID and host PID. The supervisor retains the container lifecycle and captures output in the private state/log directory; use `boxr ps`, `boxr logs --follow`, `boxr stop` and `boxr rm` for normal management. If the startup handshake times out, Boxr does not kill an unknown in-flight supervisor: it leaves it discoverable and directs you to inspect `boxr ps --all` and the private launch log.

```bash
sudo boxr run -d --name worker -- /usr/bin/sleep 3600
boxr ps
boxr logs --follow worker
sudo boxr stop --time 10s worker
sudo boxr rm worker
```

Detached mode is a per-container supervisor, not a system daemon. It supports bounded restart policies while that supervisor remains alive:

```bash
sudo boxr run -d --restart on-failure --restart-max-retries 5 --restart-delay 2s \
  --name worker -- /usr/local/bin/worker
```

- `no` is the default and never restarts.
- `on-failure` restarts non-zero exits. `--restart-max-retries 0` means unlimited retries; positive values cap the number of attempts after the initial run.
- `always` and `unless-stopped` restart after clean or failed exits while the supervisor is active, unless the container has an explicit stop request.
- `--restart-delay` accepts 100ms through 5m; the default is 1s.

Restart policies require detached mode and cannot be combined with `--rm`. Each attempt uses the same container ID and log stream; the record exposes a `restarting` state and attempt count. `boxr stop` and terminating `boxr kill` signals cancel restart backoff and suppress future attempts. This is not reboot recovery: there is no daemon or service-manager to relaunch a supervisor after it exits or the host restarts. Linux namespace permissions and the same runtime security limitations still apply.

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
boxr exec web /bin/sh            # run another process in the container
boxr exec -it web /bin/sh          # interactive shell with a real pseudo-terminal
boxr exec -i web /app/import < data.sql
boxr wait web                    # print the final exit code
boxr stats web                  # live cgroup metrics; requires an active cgroup
boxr events --limit 50          # recent lifecycle events
boxr events --action die --since 1h --format json
boxr events --container web --action start
boxr rm web                     # remove a completed record
`boxr exec` enters the running container's existing mount, UTS, IPC, network and PID namespaces (and user namespace when enabled). It inherits the live container environment, accepts additional repeatable `--env` and `--env-file` values, and runs under the container's configured capability, no-new-privileges and seccomp restrictions. When resource limits are configured, the exec launcher joins that container's cgroup before namespace entry so the new process is covered by the same limits. Use `-i` to keep stdin open and `-t` to allocate a controlling pseudo-terminal; TTY mode forwards terminal size changes, combines stdout/stderr as a terminal stream, enters raw mode on the host terminal and restores the original settings on exit. TTY mode requires a terminal attached to stdin, stdout or stderr. Without `-i` or `-t`, stdin is closed for the executed process.

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

Boxr accepts a single-stage Dockerfile subset and the versioned `Boxrfile.json` recipe. Dockerfile `RUN` steps execute inside Boxr's PID, UTS, mount, IPC and user namespaces against a staged rootfs, with a clean environment, no capabilities, `no_new_privileges`, baseline seccomp and networking disabled by default. Use `boxr image build --network host` only when a build step explicitly needs host network access. Repeatable `--build-arg NAME=VALUE` flags override declared `ARG` defaults without persisting those values as image environment variables. Supported instructions include `FROM`, `COPY`, `WORKDIR`, `ENV`, `LABEL`, `USER`, `RUN`, `CMD` and `ENTRYPOINT`; Local `ADD` files/directories and plain/gzip tar archives are supported with bounded safe extraction; remote URL/Git ADD, multi-stage builds, build secrets, cache mounts and advanced BuildKit features still fail explicitly. Base images must already be pulled into the local image store. The JSON recipe remains available for strict, explicit `COPY` builds.

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

Boxr can import a prepared rootfs directory into a local OCI image-layout store using a Boxr-specific compressed rootfs layer media type. The store uses content-addressed SHA-256 blobs, validates descriptors and layer diff IDs, and supports portable OCI-layout archives. Rootfs tar creation normalizes timestamps and owner metadata so equivalent filesystem trees produce byte-identical layer archives across build times. Boxr also imports standard Docker save archives through `boxr image load`: plain tar and gzip-compressed tar archives are accepted, every uncompressed layer diff ID is verified, layers are applied in order with Docker whiteout and opaque-directory handling, supported runtime settings and platform metadata are preserved, and all listed repository tags are imported. Imported layered images are flattened into Boxr's native rootfs layer. OCI Distribution pulls accept standard OCI and Docker tar/tar+gzip layer media types, download and verify up to four independent layers concurrently, and still apply the verified layers in manifest order with whiteout and opaque-directory semantics before importing the resulting filesystem as Boxr's canonical single-layer image. OCI Zstandard-compressed layers are supported with bounded decompression; registry pushes publish the flattened local rootfs as a standard OCI gzip layer. See [registry image support](docs/registry-images.md) and [Docker archive import](docs/docker-archive.md).

```bash
boxr image import ./rootfs alpine:local
boxr build -t my-app:latest .                 # Dockerfile build with Docker-style -t
boxr images                                  # list local images
boxr tag my-app:latest my-app:stable          # alias without copying image data
boxr rmi my-app:latest                       # remove one local tag
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

## Docker-style pull, create, and start

The top-level image commands are shortcuts for the full image namespace. `boxr build -t NAME:TAG [CONTEXT]` uses the same verified Dockerfile builder as `boxr image build`; `boxr images`, `boxr tag`, and `boxr rmi` map to the corresponding local image-store operations:

```bash
sudo boxr pull --platform linux/amd64 docker.io/library/alpine:latest
sudo boxr push alpine:latest ghcr.io/your-org/alpine-boxr:latest
```

Create a container from an image already verified in the local store, then start it separately:

```bash
sudo boxr create --name alpine-shell alpine:latest /bin/sh
sudo boxr run alpine:latest /bin/sh   # positional local-image run form
sudo boxr ps --all
sudo boxr start alpine-shell
sudo boxr restart --time 10s alpine-shell
sudo boxr container ls --all
sudo boxr container inspect alpine-shell
sudo boxr container prune --dry-run --filter status=exited
sudo boxr container prune --force --filter status=exited
sudo boxr rename alpine-shell web-shell
sudo boxr logs --follow alpine-shell
sudo boxr stop alpine-shell
sudo boxr rm alpine-shell
```

`boxr pull` and `boxr push` use the same registry verification and authentication flows as `boxr image pull` and `boxr image push`. `boxr create` accepts the runtime configuration flags used by `boxr run` (excluding `--detach` and `--dry-run`) and the Docker-style positional form `IMAGE [COMMAND...]` (or the explicit `--image IMAGE` form). It verifies the image, resolves its default command and records a container in the `created` state without launching a workload. `boxr start ID|NAME` reuses that record and launches it through Boxr's detached supervisor; it can also restart an exited or failed image-backed container created by Boxr, clearing a previous manual-stop marker. `boxr restart [--time 10s] ID|NAME` gracefully stops a running container (escalating to SIGKILL at the bounded deadline) before starting the same container again. `boxr rm` and `boxr container rm` can also remove a container while it is still in the `created` state; add `--force` (or `-f`) to cancel a pending restart or stop a running container before removal. Running or restarting containers remain protected when force is not requested. Containers created with `--rm` are removed on exit and cannot be restarted. Pull and create are separate operations: create does not silently download a missing image. The `boxr container` namespace also exposes `create`, `run`, `start`, `restart`, `rename`, `stop`, `kill`, `wait`, `logs`, `exec`, `ls`, `ps`, `inspect`, `rm`, `stats`, and `prune`, all routed through the same native lifecycle implementation. `container prune` requires exactly one of `--dry-run` or `--force`, accepts `name`, `status`, and `label` filters, and never prunes running or restart-pending records. This is an initial native lifecycle workflow, not full Docker CLI or Engine API compatibility; unsupported runtime features remain explicitly rejected, and Linux container execution still requires the appropriate kernel and privileges.

## Multi-service deployment planning

Create and validate a declarative deployment before starting any workload. The deployment commands accept both Boxr JSON manifests and the supported subset of Docker Compose YAML (`.yaml`/`.yml`):

The same supported Compose YAML subset is available through a Docker-style entrypoint: `boxr compose validate compose.yaml`, `boxr compose plan --json compose.yaml`, `boxr compose pull compose.yaml`, `boxr compose up compose.yaml`, `boxr compose ps STACK`, and `boxr compose down STACK`. These commands share the `deploy` implementation and its explicit unsupported-field validation; they do not imply full Compose parity or detached `docker compose up -d` support.

```sh
boxr deploy validate compose.yaml
boxr deploy plan compose.yaml
boxr deploy pull compose.yaml
boxr deploy up compose.yaml
```

Compose YAML supports image-based services, command arguments, environment values/files, dependency ordering, profiles, labels, working directories, hostnames, short IPv4 TCP/UDP port mappings, pre-created external Boxr volumes, and existing host bind paths. Create named volumes with `boxr volume create NAME` before deployment; relative bind paths resolve from the Compose file directory and must already exist without symlink components. Services must explicitly choose `network_mode: none|host` or one pre-created external Boxr bridge. This is not full Compose parity: builds, automatic volume creation, volume drivers, long-form mounts, implicit bridge networks, service DNS, arbitrary healthcheck commands, TTY, privileged mode and other unsupported semantics are rejected. See [the deployment guide](docs/deployments.md) for the exact supported subset and example.


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

Deployment manifests define named services, optional local OCI images, commands, existing Boxr container options, named persistent volume mounts, `dependsOn` services and optional `profiles` for opt-in service groups. Services without a profile are always active; profile-bound services stay disabled unless one of their profiles is selected. Repeat `--profile NAME` to activate several groups together, for example `boxr deploy plan --profile diagnostics --profile frontend boxr.deploy.json` or `boxr deploy up --profile frontend boxr.deploy.json`. Add a `profiles` array to a service to opt it into one or more groups. Run `boxr deploy profiles FILE.json` to inspect always-enabled services and the services in every available profile; add `--json` for automation. The inventory is sorted and omits commands, environment values and labels. A service may set `startTimeoutMs` (10–600000) to override the stack-wide startup timeout for its running-state and readiness gate; omit it or use `0` to inherit `--start-timeout`. A service may similarly set `stopTimeoutMs` (10–600000) for its graceful shutdown period. It overrides the stack-wide `deploy down --time` value; the persisted setting applies to `deploy down`, Ctrl+C shutdown and failed-start rollback. Services without an override continue to inherit the command-line value. A service may also define up to 16 `envFiles` absolute paths; Boxr loads them in declaration order, with later files overriding earlier values and explicit `options.env` taking precedence over all files. Environment files are read during service preflight, before any service is created. Plans expose only the number of environment files, never their paths or values. Every service is validated before filtering, and an enabled service cannot depend on a disabled service; use an unprofiled base service or select the dependency's profile as well. The selected service set is used consistently by planning, image prefetch and startup. Boxr rejects missing dependencies, duplicate edges, invalid run options and cycles, then produces a stable dependency-first order, starting independent services concurrently within each dependency wave. Run `boxr deploy pull FILE.json` first to prefetch and verify each unique service image (with optional `--platform`, `--authfile` and repeatable `--profile`), and checks cached images against the requested Linux architecture; `deploy up` only starts when every selected image is available locally for the host platform. Use `boxr deploy up FILE.json` for attached, fail-fast startup; `boxr deploy ps STACK` for stack-scoped status; and `boxr deploy down STACK` for reverse-order graceful shutdown. `deploy down --remove` also removes stopped records and logs. `boxr deploy logs [--tail N] [--follow] STACK [SERVICE]` prints service-prefixed output from every service or a selected service. `boxr deploy wait --condition running|stopped STACK` waits for the whole stack to reach the requested state and supports a timeout, polling interval and secret-redacted JSON result. Named volumes refer to volumes created with `boxr volume create`; Boxr verifies every volume and mount before creating the first service. Service dependencies gate startup order; configured loopback TCP/HTTP readiness probes gate dependent waves until the application returns the expected status. Optional service `healthcheck` settings then run recurring TCP/HTTP liveness probes after every selected service has passed its startup gates. Healthchecks use bounded intervals, timeouts, retry counts and start periods; successful checks reset consecutive failures, while a failed threshold stops the attached deployment and shuts down its services using their individual `stopTimeoutMs` settings. Healthcheck targets remain loopback-only and are redacted from plan output. Healthchecks are not automatic restart policies: restart policies, service DNS, bridge networking and detached daemon operation remain future work.

A healthcheck can be declared on a service like this:

```json
{
  "healthcheck": {
    "probe": {
      "httpUrl": "http://127.0.0.1:8080/healthz",
      "timeoutMs": 500
    },
    "intervalMs": 5000,
    "retries": 3,
    "startPeriodMs": 10000
  }
}
```

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

## Managed bridge networking

Create a non-overlapping IPv4 bridge network, then attach a container to it:

```bash
sudo boxr network create --subnet 172.30.10.0/24 app-net
sudo boxr network ls
sudo boxr network inspect app-net
sudo boxr run --name app --network bridge --network-name app-net --publish 127.0.0.1:8080:80/tcp -- /bin/sh
# The bridge veth and IPAM lease are released when the container exits.
sudo boxr network rm app-net
```

Boxr creates a host Linux bridge, assigns the gateway address, allocates a unique container IPv4 address, creates a veth pair, moves one peer into the container network namespace, renames it to `eth0`, and configures the default route. Outbound NAT is set up with network-specific iptables chains unless `--no-nat` is supplied. Removing a network is refused while it has attached containers. The network metadata and address leases are stored in the private Boxr data directory.

Publish host ports with repeatable `--publish` (or `-p`) flags. The accepted form is `[HOST_IP:]HOST_PORT:CONTAINER_PORT[/tcp|udp]`; omitting the host IP binds the port on all IPv4 addresses and omitting the protocol defaults to TCP:

```bash
sudo boxr run --network bridge --network-name app-net \
  --publish 127.0.0.1:8080:80/tcp \
  --publish 5353:53/udp \
  -- /bin/sh
```

Boxr installs per-container DNAT rules for incoming traffic and host-originated connections, plus a forwarding allow rule limited to the allocated container address, protocol and destination port. Host port reservations are checked across all Boxr-managed networks; wildcard binds conflict with a specific address on the same protocol/port. The mapping metadata is stored alongside the IP lease, and its firewall rules are removed when the container leaves the network. If attachment or firewall setup fails, Boxr rolls back the port rules, veth and IP lease. Port publishing currently supports IPv4 TCP and UDP only; it does not reserve the host socket against unrelated host applications, so avoid assigning a port already used by a host service.

A running container can also join or leave a managed bridge without restarting, provided it was started in an isolated network namespace with `--network none` and without `--userns`:

```bash
sudo boxr run --name live-api --network none -- /bin/sh
sudo boxr network connect --publish 127.0.0.1:8081:80/tcp app-net live-api
sudo boxr network disconnect app-net live-api
```

Live attachment verifies that the recorded PID is the Boxr namespace helper before entering its network namespace. A container can be attached to one managed bridge at a time; Boxr rejects host-network and user-namespace containers for this operation. Dynamic attachments and their published ports are automatically detached when the container exits. When Boxr or the host exits unexpectedly, stale network leases can be reviewed and reclaimed safely:

```bash
sudo boxr network prune              # read-only report by default
sudo boxr network prune --force      # remove verified Boxr-owned stale links and leases
```

Managed veth interfaces carry a network/container ownership alias. Pruning refuses to delete an unmarked or mismatched host interface and retains its IPAM reservation for manual investigation. Live container helpers are excluded from the stale set.

Bridge networking currently provides host-local IPv4 connectivity only. It does not yet configure DNS inside containers, support IPv6 or multi-host overlays. The `--no-nat` option only omits Boxr's NAT rules; host routing and firewall policy still govern any other forwarding path. Treat this as an early runtime feature, not a hardened multi-tenant network boundary.

## Runtime requirements and security limits

- Linux kernel with support for PID, UTS, mount, IPC and network namespaces. Bridge mode needs the privileges to create bridge/veth links and configure container network namespaces.
- cgroup v2 must be mounted and writable with requested controllers delegated for resource-limit options.
- `--rootfs` is an existing directory owned/prepared by the operator. `boxr image pull` supports schema-2 OCI and Docker distribution manifests with up to 128 verified tar or gzip layers; the merged filesystem is flattened into Boxr's canonical local image layer. `boxr image push` publishes a standard OCI gzip layer from the canonical single-layer local image; original multi-layer history is not retained. For offline transfers, import and export Docker-compatible archives with `docker save`/`docker load` and `boxr image load`/`boxr image save --format docker`.
- `--network none` creates a private network namespace with loopback only; `host` is the default. Managed bridge mode is selected with `--network bridge --network-name NAME` and requires an existing network from `boxr network create`. Bridge networking is IPv4-only and currently has no IPv6, published host ports, embedded DNS/service-name discovery, multi-host overlay, or network attach/detach for already-running containers. Outbound NAT is enabled by default for managed bridges; `boxr network create --no-nat` disables Boxr's NAT rules but does not override the host's other routing/firewall policy. Bridge setup requires `ip` (iproute2), `nsenter`, `iptables`, `sysctl`, and the necessary host privileges. `--seccomp-profile baseline` enables a fixed architecture-specific denylist and automatically enables no-new-privileges; it is defense-in-depth, not a sandbox policy.
- Named volumes require `--rootfs`; they are not a host-wide container filesystem abstraction and must not be treated as encrypted storage.
- OverlayFS provides a persistent writable rootfs view. Registry pulls validate and apply each OCI/Docker layer before flattening the merged filesystem into the local OCI-layout image store; registry pushes publish Boxr's flattened image using standard OCI gzip-layer media types; arbitrary multi-layer history is not preserved. Boxr does not preserve arbitrary multi-layer graphs for push or provide Docker-daemon integration, customizable seccomp profiles, device filtering, LSM policy management, bridge-network DNS/service discovery, IPv6 networking, daemonized detached containers, or an OCI runtime-spec bundle interface.
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
