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
| GET | `/volumes` | List local named volumes with name, label, driver, scope and dangling filters |
| POST | `/volumes/create` | Create a private local named volume with validated labels |
| GET | `/volumes/{name}` | Inspect a named volume and its mountpoint |
| DELETE | `/volumes/{name}` | Remove a named volume only when no persisted container references it |
| GET | `/networks` | List managed bridge networks with Docker-compatible name, driver, scope and label filters |
| POST | `/networks/create` | Create a managed IPv4 bridge network with validated IPAM, bridge and NAT settings |
| GET | `/networks/{id}` | Inspect managed bridge, subnet, gateway, NAT, IPAM and labels |
| DELETE | `/networks/{id}` | Remove a managed bridge network only when no containers are attached |

Boxr persists combined stdout/stderr output in one log file; the compatibility endpoint currently emits that combined stream as Docker's stdout frame rather than inventing a separate stderr stream. The image-create endpoint accepts Docker's `X-Registry-Auth` header or configured Docker/Podman auth files; it verifies the registry manifest, every layer digest and each uncompressed DiffID before importing into the local OCI store. The default platform is the host Linux architecture; callers may set `platform=linux/ARCH[/VARIANT]`. Digest-form pulls through this API are not supported yet. Pull progress and errors are streamed as JSON records. Container create can then consume the pulled tag. The start endpoint accepts created, exited and failed Engine API containers; an explicit start clears an earlier manual-stop marker before launching the saved image and configuration. Image push exports a verified local image and publishes it with the same Docker auth header or configured auth file; the current Boxr image store exports a single canonical rootfs layer, so the API does not preserve original multi-layer build history. Container network address/DNS metadata is not yet available through inspect. Stats require a live Linux cgroup v2 container. Unsupported API versions and unsupported methods are rejected.

**This is not a Docker Engine replacement yet.** Container create/start/stop are implemented only for a strict supported subset: create requires a verified local image, and unsupported image-user overrides, entrypoint overrides, interactive TTY/stdin, anonymous volumes, healthchecks, privileged mode, devices, capability additions and unsupported HostConfig fields are rejected. Stop timeouts are bounded (including a five-minute cap for `t=-1`). Explicit start and restart are supported through the saved image/configuration and native detached supervisor. Restart sends a graceful stop first, escalates to SIGKILL after the requested bounded timeout, and refuses containers configured for automatic removal. Attach/exec upgrades, archive copy, image build APIs, digest-based Engine API pulls, structured `HostConfig.Mounts` arrays, anonymous volumes and non-local volume drivers, network connect/disconnect APIs, Docker plugins, swarm and TCP listeners are not implemented by this API layer. The API mounts pre-created Boxr named volumes through Docker's `HostConfig.Binds` short syntax (`volume-name:/container/path[:ro|rw]`). Structured `HostConfig.Mounts` arrays and automatic anonymous-volume creation remain unsupported. Docker restart policies remain separate from the manually requested restart endpoint. The wait endpoint currently supports only the default `not-running` condition; `next-exit` and `removed` are rejected. Image delete removes a tag from the OCI index; content-addressed blobs remain eligible for the separate Boxr prune operation rather than being deleted during the API request. Kill signals and removal of created/stopped containers are also implemented; anonymous-volume deletion remains explicitly rejected. The named-volume API supports only the local driver; driver options and third-party volume plugins are rejected. The network API supports only Boxr-managed IPv4 bridge networks; custom drivers, IPv6, internal/ingress networks, IPAM ranges and auxiliary addresses are rejected. Volume deletion is blocked while any persisted container record references the volume, including stopped and created containers. Forced removal reuses the native bounded SIGKILL and restart-cancellation logic. The CLI's read commands are not evidence of full Docker CLI compatibility. Boxr's native CLI remains available for creating and controlling containers. The server does not expose an unauthenticated TCP endpoint.

The goal of this initial API milestone is to provide a secure local control socket and a truthful compatibility boundary. Docker CLI workloads that need mutation operations still require Docker Engine until those endpoints are implemented and tested.
