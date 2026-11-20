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
| POST | `/containers/{id}/kill` | Deliver a validated Docker signal to a verified running process |
| DELETE | `/containers/{id}` | Remove a created or stopped container record and its log |
| GET | `/events` | Persisted container lifecycle events |
| GET | `/images/json` | OCI image tags and metadata |

Boxr persists combined stdout/stderr output in one log file; the compatibility endpoint currently emits that combined stream as Docker's stdout frame rather than inventing a separate stderr stream. Container network address/DNS metadata is not yet available through inspect. Stats require a live Linux cgroup v2 container. Unsupported API versions and unsupported methods are rejected.

**This is not a Docker Engine replacement yet.** Container create/start/stop/restart, attach/exec upgrades, archive copy, image pull/push/build, volume/network mutation APIs, Docker plugins, swarm and TCP listeners are not implemented by this API layer. Kill signals and removal of created/stopped containers are implemented; forced removal of running containers and anonymous-volume deletion are explicitly rejected. The CLI's read commands are not evidence of full Docker CLI compatibility. Boxr's native CLI remains available for creating and controlling containers. The server does not expose an unauthenticated TCP endpoint.

The goal of this initial API milestone is to provide a secure local control socket and a truthful compatibility boundary. Docker CLI workloads that need mutation operations still require Docker Engine until those endpoints are implemented and tested.
