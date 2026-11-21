# Docker Engine API compatibility

Boxr includes an initial, intentionally limited Docker Engine HTTP API for local tooling. It exposes runtime-backed read operations plus a narrow set of safe container lifecycle mutations; unsupported operations fail explicitly. Start the daemon:

```sh
boxr daemon
```

By default it listens on `$XDG_RUNTIME_DIR/boxr/engine.sock`, or `~/.cache/boxr/run/engine.sock` when `XDG_RUNTIME_DIR` is unset. Override the path with `--host unix:///absolute/path/engine.sock`. The socket is owner-only (`0600`); the daemon refuses TCP listeners, symlinked socket paths, unsafe socket directories, replacement of regular files, and a second live listener.

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

The default API version advertised is `1.45` (minimum `1.24`). Implemented endpoints are:

| Method | Endpoint | Behavior |
|---|---|---|
| GET, HEAD | `/_ping` | Engine liveness and API version headers |
| GET | `/version` | API and Boxr build version |
| GET | `/info` | Runtime-backed host, container and image counts |
| GET | `/containers/json` | Container list with name, ID, status and label filters |
| GET | `/containers/{id}/json` | Container state, command, resource, mount and port metadata |
| GET | `/containers/{id}/logs` | Bounded tail or follow using Docker multiplex framing |
| GET | `/containers/{id}/stats` | cgroup v2 CPU, memory and PID samples (snapshot or stream) |
| POST | `/containers/create` | Create a container from a verified local image; `HostConfig.Binds` supports host paths and pre-created Boxr named volumes |
| POST | `/containers/{id}/start` | Start a created container or restart an exited/failed container through the native detached supervisor |
| POST | `/containers/{id}/wait` | Wait for the `not-running` condition and return the persisted workload exit status |
| POST | `/containers/{id}/rename` | Rename a container while preserving its ID and rejecting duplicate names |
| POST | `/containers/{id}/restart` | Gracefully stop and restart a created or existing image-backed container with a bounded timeout |
| POST | `/containers/{id}/stop` | Graceful stop with a bounded timeout and SIGKILL fallback |
| POST | `/containers/{id}/kill` | Deliver a validated Docker signal to a verified running process |
| DELETE | `/containers/{id}` | Remove a created/stopped container; `force=true` stops and removes a running or restarting container |
| GET | `/events` | Persisted container lifecycle events |
| POST | `/images/create` | Pull and verify tagged OCI/Docker registry images with Docker auth and Linux platform selection |
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

Boxr persists combined stdout/stderr output in one log file; the compatibility endpoint currently emits that combined stream as Docker's stdout frame rather than inventing a separate stderr stream. The image-create endpoint accepts Docker's `X-Registry-Auth` header or configured Docker/Podman auth files; it verifies the registry manifest, every layer digest and each uncompressed DiffID before importing into the local OCI store. The default platform is the host Linux architecture; callers may set `platform=linux/ARCH[/VARIANT]`. Digest-form pulls through this API are not supported yet. Image export uses Docker-save format; image load validates all archive tags and refuses to overwrite any tag persisted by a container. Imported multi-layer Docker images are flattened into Boxr's native single-rootfs-layer representation. The image-prune endpoint defaults to `dangling=true`; Boxr's store indexes named tags only, so the default leaves tagged images intact while reclaiming any already-orphaned blobs. `dangling=false` selects unused tags and supports Docker-compatible `until`, `label` and `label!` filters. If a persisted container references an image digest, all aliases for that digest are retained. Selected tags are removed with one atomic OCI index update, then unreferenced manifest/config/layer blobs are deleted and reported through `ImagesDeleted` and `SpaceReclaimed.\n\nThe build endpoint accepts a bounded tar context (maximum 8 GiB), one or more t tags, a relative Dockerfile path, a platform and JSON build arguments. It uses the supported single-stage Dockerfile subset documented in docs/dockerfile-build.md; build tags referenced by persisted containers are protected from replacement. Pull progress and errors are streamed as JSON records. Container create can then consume the pulled tag. The start endpoint accepts created, exited and failed Engine API containers; an explicit start clears an earlier manual-stop marker before launching the saved image and configuration. Image push exports a verified local image and publishes it with the same Docker auth header or configured auth file; the current Boxr image store exports a single canonical rootfs layer, so the API does not preserve original multi-layer build history. Container network address/DNS metadata is not yet available through inspect. Stats require a live Linux cgroup v2 container. Unsupported API versions and unsupported methods are rejected.

**This is not a Docker Engine replacement yet.** Container create/start/stop are implemented only for a strict supported subset: create requires a verified local image, and unsupported image-user overrides, entrypoint overrides, interactive TTY/stdin, anonymous volumes, healthchecks, privileged mode, devices, capability additions and unsupported HostConfig fields are rejected. Stop timeouts are bounded (including a five-minute cap for `t=-1`). Explicit start and restart are supported through the saved image/configuration and native detached supervisor. Restart sends a graceful stop first, escalates to SIGKILL after the requested bounded timeout, and refuses containers configured for automatic removal. Attach/exec upgrades, archive copy, BuildKit, multi-stage and remote-context image builds, digest-based Engine API pulls, structured `HostConfig.Mounts` arrays, automatic volume creation from image `Config.Volumes`, network aliases/static endpoint configuration, Docker plugins, swarm and TCP listeners are not implemented by this API layer. The `/build` endpoint accepts `networkmode=none` or `host`; Docker's `default` is mapped to Boxr's isolated `none` mode, while host networking remains opt-in. Base images must already exist in the local image store. The endpoint rejects `pull=true`, `nocache=true`, `rm=false`, remote contexts, BuildKit sessions, multi-stage builds and unsupported query parameters. Temporary context and build state are always removed. Dockerfile `RUN` steps execute with Boxr's namespace, capability-drop, no-new-privileges and baseline seccomp controls.\n\nThe API mounts pre-created Boxr named or anonymous volumes through Docker's `HostConfig.Binds` short syntax (`volume-name:/container/path[:ro|rw]`). `POST /volumes/create` creates an anonymous volume when `Name` is omitted. Structured `HostConfig.Mounts` arrays and automatic volume creation from image `Config.Volumes` remain unsupported. Docker restart policies remain separate from the manually requested restart endpoint. The wait endpoint currently supports only the default `not-running` condition; `next-exit` and `removed` are rejected. Image delete removes a tag from the OCI index; content-addressed blobs remain eligible for the separate Boxr prune operation rather than being deleted during the API request. Kill signals and removal of created/stopped containers are also implemented. Volume prune removes unreferenced anonymous volumes by default; `all=true` includes named volumes, and persisted container references are protected. The volume API supports only the local driver; driver options and third-party volume plugins are rejected. The network API supports only Boxr-managed IPv4 bridge networks; custom drivers, IPv6, internal/ingress networks, IPAM ranges and auxiliary addresses are rejected. Connect/disconnect only attach verified running containers started with network mode `none`, and endpoint-specific IP/MAC aliases are not supported. Network prune skips bridges with leases or persisted container references. Volume deletion is blocked while any persisted container record references the volume, including stopped and created containers. Forced removal reuses the native bounded SIGKILL and restart-cancellation logic. The CLI's read commands are not evidence of full Docker CLI compatibility. Boxr's native CLI remains available for creating and controlling containers. The server does not expose an unauthenticated TCP endpoint.

The goal of this initial API milestone is to provide a secure local control socket and a truthful compatibility boundary. Docker CLI workloads that need mutation operations still require Docker Engine until those endpoints are implemented and tested.
