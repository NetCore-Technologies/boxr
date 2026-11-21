# Docker Engine API compatibility

Boxr includes an initial, intentionally limited Docker Engine HTTP API for local tooling. It exposes runtime-backed read operations plus a narrow set of safe container lifecycle mutations; unsupported operations fail explicitly. Start the daemon:

```sh
boxr daemon
```

By default it listens on `$XDG_RUNTIME_DIR/boxr/engine.sock`, or `~/.cache/boxr/run/engine.sock` when `XDG_RUNTIME_DIR` is unset. Override the path with `--host unix:///absolute/path/engine.sock`. The socket is owner-only (`0600`); the daemon refuses symlinked socket paths, unsafe socket directories, replacement of regular files, and a second live listener. For remote administration, one TCP listener is available only with mutual TLS: plaintext TCP is rejected, the server verifies client certificates against the configured CA, TLS 1.2 or newer is required, and the server private key must be owner-only.

Engine API-created containers now unpack the verified image into a private, container-owned immutable rootfs snapshot at create time. That lower snapshot is retained with the container until it is removed or pruned, even if the original local image tag is later deleted. The persistent OverlayFS writable layer is mounted over the snapshot on each start. Native CLI workloads continue to use the explicitly configured `--rootfs` policy.

Engine API-created containers are assigned a dedicated cgroup v2 child before their startup barrier opens, even when no resource limits are initially configured. This makes later Docker Engine `/containers/{id}/update` calls safe to apply without migrating a live process out of the Boxr parent cgroup. Engine API-created containers also receive a private per-container OverlayFS writable layer under the Boxr state directory. The layer is reused when a stopped container starts again, keeping filesystem changes separate from the verified image rootfs, and is removed with the container record. This persistence is limited to Engine API-created containers; native CLI workloads continue to use the explicitly configured `--overlay-dir` policy.

A Docker CLI can use that socket for the implemented operations shown below; most container lifecycle and image workflows still require native Boxr commands:

```sh
export DOCKER_HOST=unix:///absolute/path/engine.sock
docker version
docker info
docker ps --all
docker images
docker inspect CONTAINER
docker logs CONTAINER
docker stats --no-stream CONTAINER
```

For a remote Engine API listener, provision a CA, a server certificate whose SAN matches the daemon hostname/IP, and client certificates signed by that CA. Start Boxr with:

```sh
boxr daemon \
  --host tcp://0.0.0.0:2376 \
  --tlsverify \
  --tlscacert /etc/boxr/ca.pem \
  --tlscert /etc/boxr/server-cert.pem \
  --tlskey /etc/boxr/server-key.pem
```

Connect from a Docker-compatible client with its own client certificate and the CA:

```sh
docker --host tcp://boxr.example.com:2376 --tlsverify \
  --tlscacert ca.pem --tlscert cert.pem --tlskey key.pem version
```

Every client certificate trusted by the configured CA receives the full Engine API authority; there is no per-client role model yet. Treat client private keys as privileged host-administration credentials. Only one listener (Unix or mutually authenticated TCP) can be configured per daemon process.

The default API version advertised is `1.45` (minimum `1.24`). Implemented endpoints are:

| Method | Endpoint | Behavior |
|---|---|---|
| GET, HEAD | `/_ping` | Engine liveness and API version headers |
| GET | `/version` | API and Boxr build version |
| GET | `/info` | Runtime-backed host, container and image counts |
| GET | `/containers/json` | Container list with name, ID, status and label filters |
| POST | `/containers/prune` | Remove created and stopped containers using Docker-compatible `until`, `label` and `label!` filters; running and restarting containers are protected; emits the Docker `prune` event |
| GET | `/containers/{id}/json` | Container state, command, resource, mount and port metadata |
| POST | `/containers/{id}/update` | Apply live cgroup v2 Memory, MemoryReservation, MemorySwap, NanoCpus and PidsLimit changes to a running container; PidsLimit accepts 0 or -1 for unlimited |
| POST | `/containers/{id}/pause` | Freeze a verified running container using the cgroup v2 freezer and persist its paused state |
| POST | `/containers/{id}/unpause` | Thaw a verified paused container and persist its running state |
| GET | `/containers/{id}/logs` | Bounded tail or follow using Docker multiplex framing |
| GET | `/containers/{id}/stats` | cgroup v2 CPU, memory and PID samples (snapshot or stream) |
| POST | `/containers/create` | Create a container from a verified local image; `HostConfig.Binds` and a strict `HostConfig.Mounts` subset support existing host paths, pre-created Boxr volumes and basic tmpfs mounts |
| POST | `/containers/{id}/start` | Start a created container or restart an exited/failed container through the native detached supervisor |
| POST | `/containers/{id}/wait` | Wait for the `not-running` condition and return the persisted workload exit status |
| POST | `/containers/{id}/rename` | Rename a container while preserving its ID and rejecting duplicate names |
| POST | `/containers/{id}/restart` | Gracefully stop and restart a created or existing image-backed container with a bounded timeout |
| POST | `/containers/{id}/stop` | Graceful stop with a bounded timeout and SIGKILL fallback |
| POST | `/containers/{id}/kill` | Deliver a validated Docker signal to a verified running process |
| POST | `/containers/{id}/exec` | Create a bounded non-TTY exec session with command, environment, working directory and stdout/stderr attachment |
| POST | `/exec/{id}/start` | Run an exec session through Boxr's namespace-aware executor; attached output uses Docker multiplex frames or may be detached |
| GET | `/exec/{id}/json` | Inspect exec running state, process ID and exit code |
| DELETE | `/containers/{id}` | Remove a created/stopped container; `force=true` stops and removes a running or restarting container |
| GET | `/events` | Persisted container lifecycle events |
| POST | `/images/create` | Pull and verify tagged or SHA-256 digest-pinned OCI/Docker registry images with Docker auth and Linux platform selection |
| POST | `/images/{name}/tag` | Create a Docker-compatible local tag alias for a verified image, protecting in-use targets |
| GET | `/images/json` | OCI image tags and metadata |
| GET | `/images/{name}/json` | Verified image config, platform, tag, size and uncompressed layer DiffIDs |
| DELETE | `/images/{name}` | Remove an unused local image tag; refuse removal while a container references it |
| POST | `/images/{name}/push` | Push a verified local image to an OCI/Docker registry using Docker auth |
| GET | `/images/{name}/get` | Export one verified local image tag as a Docker-save tar archive |
| GET | `/images/get` | Export multiple verified local image tags into one Docker-save tar archive |
| POST | `/images/load` | Import a validated Docker-save tar archive after checking tags against container references |
| POST | `/images/prune` | Prune dangling images or (with `dangling=false`) unused tags, then reclaim unreferenced blobs |
| POST | `/build` | Build a tagged image from a bounded local tar context with Boxr's supported single-stage Dockerfile subset |
| GET | `/volumes` | List local named volumes with name, label, driver, scope and dangling filters |
| POST | `/volumes/create` | Create a private local named volume with validated labels |
| GET | `/volumes/{name}` | Inspect a named volume and its mountpoint |
| DELETE | `/volumes/{name}` | Remove a named or anonymous local volume only when no persisted container references it |
| POST | `/volumes/prune` | Prune unused anonymous volumes by default; all=true includes named volumes; label filters are supported |
| GET | `/networks` | List managed bridge networks with Docker-compatible name, driver, scope and label filters |
| POST | `/networks/create` | Create a managed IPv4 bridge network with validated IPAM, bridge and NAT settings |
| GET | `/networks/{id}` | Inspect managed bridge, subnet, gateway, NAT, IPAM and labels |
| DELETE | `/networks/{id}` | Remove a managed bridge network only when no containers are attached |
| POST | `/networks/{id}/connect` | Attach a verified running NetworkMode=none container to a managed bridge |
| POST | `/networks/{id}/disconnect` | Remove a verified managed bridge attachment and release its IPAM lease |
| POST | `/networks/prune` | Prune unused managed bridges with Docker-compatible label and until filters |

Boxr persists combined stdout/stderr output in one log file; the compatibility endpoint currently emits that combined stream as Docker's stdout frame rather than inventing a separate stderr stream. The image-create endpoint accepts Docker's `X-Registry-Auth` header or configured Docker/Podman auth files; it verifies the registry manifest, every layer digest and each uncompressed DiffID before importing into the local OCI store. The default platform is the host Linux architecture; callers may set `platform=linux/ARCH[/VARIANT]`. Immutable pulls accept `fromImage=registry/repository@sha256:...` or a `tag=sha256:...` selector. Boxr verifies the requested manifest digest and maps the immutable reference to a deterministic local tag (`repository:sha256-HEX`) so the image remains addressable in its tag-oriented local store; native create requests accept the digest reference and resolve it to the same alias. Image export uses Docker-save format; image load validates all archive tags and refuses to overwrite any tag persisted by a container. Imported multi-layer Docker images are flattened into Boxr's native single-rootfs-layer representation. The image-prune endpoint defaults to `dangling=true`; Boxr's store indexes named tags only, so the default leaves tagged images intact while reclaiming any already-orphaned blobs. `dangling=false` selects unused tags and supports Docker-compatible `until`, `label` and `label!` filters. If a persisted container references an image digest, all aliases for that digest are retained. Selected tags are removed with one atomic OCI index update, then unreferenced manifest/config/layer blobs are deleted and reported through `ImagesDeleted` and `SpaceReclaimed.\n\nThe build endpoint accepts a bounded tar context (maximum 8 GiB), one or more t tags, a relative Dockerfile path, a platform and JSON build arguments. It uses the supported single-stage Dockerfile subset documented in docs/dockerfile-build.md; build tags referenced by persisted containers are protected from replacement. Pull progress and errors are streamed as JSON records. Container create can then consume the pulled tag. The start endpoint accepts created, exited and failed Engine API containers; an explicit start clears an earlier manual-stop marker before launching the saved image and configuration. Image push exports a verified local image and publishes it with the same Docker auth header or configured auth file; the current Boxr image store exports a single canonical rootfs layer, so the API does not preserve original multi-layer build history. Container network address/DNS metadata is not yet available through inspect. The `/containers/prune` endpoint removes Boxr state records, Engine API sidecars and logs through the native state-store safeguards. `SpaceReclaimed` is currently reported as zero because Boxr does not yet account for per-container writable filesystem layers; caller-managed rootfs and OverlayFS directories are not deleted by prune. Multiple `label!` expressions follow Docker's combined-exclusion behaviour: a container is protected from pruning only when it matches every negative-label expression. Pause and unpause require a verified process in the container's dedicated cgroup v2 subtree. Paused containers remain active for listing, wait, stop, kill, and removal checks; stop/kill resume them before signal delivery, and prune never treats them as stopped.

Stats and live resource updates require a Linux cgroup v2 container with the relevant controllers delegated to Boxr. Live updates verify the recorded PID start time and Boxr container identity before touching the cgroup, validate every target control before writing, and roll back partial kernel writes or failed state persistence. The update endpoint intentionally rejects unsupported Docker resource fields rather than silently ignoring them. Unsupported API versions and unsupported methods are rejected.

**This is not a Docker Engine replacement yet.** Container create/start/stop are implemented only for a strict supported subset: create requires a verified local image, and unsupported image-user overrides, interactive TTY/stdin, healthchecks, privileged mode, devices, capability additions and unsupported HostConfig fields are rejected. Stop timeouts are bounded (including a five-minute cap for `t=-1`). Explicit start and restart are supported through the saved image/configuration and native detached supervisor. Restart sends a graceful stop first, escalates to SIGKILL after the requested bounded timeout, and refuses containers configured for automatic removal. Attach/exec upgrades, archive copy, BuildKit, multi-stage and remote-context image builds, unsupported structured mount options, network aliases/static endpoint configuration, Docker plugins, swarm and TCP listeners are not implemented by this API layer. The `/build` endpoint accepts `networkmode=none` or `host`; Docker's `default` is mapped to Boxr's isolated `none` mode, while host networking remains opt-in. Base images must already exist in the local image store. The endpoint rejects `pull=true`, `nocache=true`, `rm=false`, remote contexts, BuildKit sessions, multi-stage builds and unsupported query parameters. Temporary context and build state are always removed. Dockerfile `RUN` steps execute with Boxr's namespace, capability-drop, no-new-privileges and baseline seccomp controls.\n\nContainer create honours `Config.Entrypoint` overrides (including an explicit empty array to clear the image entrypoint) and `Config.Cmd` overrides. Image and create-request `Config.ExposedPorts` are merged and retained in image/container inspect metadata; declaring a port does not publish it on the host. Image Dockerfile `STOPSIGNAL` and create-request `Config.StopSignal` are validated, persisted and used by graceful stop before the SIGKILL fallback. It also creates anonymous persistent volumes for image or create-request `Config.Volumes` declarations, while explicit mounts at the same target take precedence. Dockerfile `VOLUME` declarations and image config metadata are preserved through local image builds, OCI storage, Docker-save export/import and image inspect. It also mounts pre-created Boxr named or anonymous volumes through Docker's `HostConfig.Binds` short syntax (`volume-name:/container/path[:ro|rw]`) and accepts a strict subset of `HostConfig.Mounts`. Structured bind mounts require an existing non-symlink absolute host path; only the default `rprivate` propagation is accepted, and host mountpoint creation and recursive mount flags are rejected. Structured volume mounts require an existing local Boxr volume. By default, Boxr copies the image directory into an empty volume during create; set `VolumeOptions.NoCopy=true` to suppress this. Copy-up is skipped for non-empty volumes and refused while another Boxr container is actively using the volume. File contents, directory structure and normal permission bits are copied through a bounded no-follow tree walk; symlink targets are preserved exactly and never followed, while special files are rejected. Ownership and setuid/setgid bits are not preserved. Structured tmpfs mounts require an explicit positive `SizeBytes` (up to 1 GiB per mount and 2 GiB total) and support `Mode`; custom `TmpfsOptions.Options` and read-only tmpfs are rejected. Duplicate and nested mount targets are rejected to avoid order-dependent mount overlays. `POST /volumes/create` creates an anonymous volume when `Name` is omitted. Docker restart policies remain separate from the manually requested restart endpoint. The wait endpoint currently supports only the default `not-running` condition; `next-exit` and `removed` are rejected. Image delete removes a tag from the OCI index; content-addressed blobs remain eligible for the separate Boxr prune operation rather than being deleted during the API request. Kill signals and removal of created/stopped containers are also implemented. Volume prune removes unreferenced anonymous volumes by default; `all=true` includes named volumes, and persisted container references are protected. The volume API supports only the local driver; driver options and third-party volume plugins are rejected. The network API supports only Boxr-managed IPv4 bridge networks; custom drivers, IPv6, internal/ingress networks, IPAM ranges and auxiliary addresses are rejected. Connect/disconnect only attach verified running containers started with network mode `none`, and endpoint-specific IP/MAC aliases are not supported. Network prune skips bridges with leases or persisted container references. Volume deletion is blocked while any persisted container record references the volume, including stopped and created containers. Container deletion with Docker's `v=1` flag removes attached anonymous volumes after the container is gone, but retains named volumes and anonymous volumes still referenced by another container. Forced removal reuses the native bounded SIGKILL and restart-cancellation logic. The CLI's read commands are not evidence of full Docker CLI compatibility. Boxr's native CLI remains available for creating and controlling containers. Exec sessions currently support non-TTY stdout/stderr execution and detach. Docker's `Detach` option is accepted on `/exec/{id}/start`, not on exec creation. Stdin attachment, TTY/PTY hijacking, TTY console sizing/detach keys, per-exec user overrides and privileged exec are rejected explicitly; session state is kept in daemon memory and is lost when the daemon restarts. Commands launch through Boxr's native exec command, which rechecks container process identity and enters its namespaces and resource cgroup. The server does not expose an unauthenticated TCP endpoint.

The goal of this initial API milestone is to provide a secure local control socket and a truthful compatibility boundary. Docker CLI workloads that need mutation operations still require Docker Engine until those endpoints are implemented and tested.


Container healthcheck configuration is inherited from image metadata unless overridden by create-request `Healthcheck`. Running probes update the bounded five-entry `State.Health.Log`, status, and failing streak; probes are cancelled with the container process.
