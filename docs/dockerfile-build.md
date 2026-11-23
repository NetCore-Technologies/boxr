# Dockerfile builds

Boxr can build a bounded Dockerfile subset, including multi-stage builds and artifact copies from earlier stages. `RUN` commands execute in a disposable staged rootfs through Boxr's namespace runtime, not directly on the host:

- Multiple `FROM` stages using `scratch`, a previously declared stage, or a base image already pulled into Boxr's local image store. Stage aliases are supported.
- Local-context `COPY` and `ADD` (including JSON form and `COPY .`), with paths kept inside the build context. Local ADD files/directories are copied; plain and gzip tar archives are automatically extracted.
- `COPY --from=SOURCE` from an earlier named or numbered stage, or from a verified external image already present in the local store; external images are never pulled implicitly. `COPY --chown=UID:GID` applies numeric ownership to copied files, directories and symlinks without following links. Named users/groups and `ADD --chown` are explicitly rejected. `COPY --chmod=MODE` supports bounded octal file permissions (000–0777). Setuid, setgid, sticky and symbolic modes are rejected.
- `RUN` in shell or exec form, JSON-array `SHELL` selection for shell-form `RUN`, `CMD` and `ENTRYPOINT`, inherited from verified base images and earlier stages, plus `WORKDIR`, `ENV`, `LABEL`, `USER`, Dockerfile `VOLUME` declarations (shell and JSON forms), Dockerfile `EXPOSE` declarations with protocol defaults and bounded port ranges, `STOPSIGNAL` declarations using supported Linux signals, and shell/exec forms of `CMD` and `ENTRYPOINT`. Declared volumes are preserved as image metadata and become anonymous persistent volumes by default when a container is created; an explicit mount at the same target takes precedence.
- Bounded `.dockerignore` filtering. Negation patterns and advanced Docker ignore semantics are rejected.

```dockerfile
FROM scratch
WORKDIR /opt/app
COPY . .
ENV APP_MODE=production
LABEL org.example.component=api
USER 1000:1000
ENTRYPOINT ["/opt/app/server"]
CMD ["--listen", ":8080"]
```

Build with Docker's familiar `-f`/`--file` selector:

```sh
boxr image build --tag example/api:dev -f Dockerfile .
```

A local base must be pulled before building:

```sh
boxr image pull alpine:3.22
boxr image build --tag example/api:dev -f Dockerfile .
```

`ADD` supports local files, directories and plain/gzip tar archives only. Remote URL/Git ADD sources, build secrets, cache mounts, remote contexts, `RUN --mount`, and advanced Dockerfile frontend features are not implemented. They fail explicitly. Stage bases must be `scratch`, a previously declared stage, or an image already present in the local store. Dockerfile RUN executes in Boxr's namespaces and staged rootfs, but Boxr is not yet a BuildKit replacement. The builder currently flattens the result into Boxr's canonical local image layer, so it does not preserve source layer history or Docker build cache metadata.

This is a compatibility subset, not a complete Dockerfile frontend or BuildKit replacement.

## Multi-stage builds

Use named or numeric earlier stages to keep build tools out of the final image:

```dockerfile
# Assume the statically linked binary is available in the build context.
FROM alpine:3.22 AS build
WORKDIR /opt/build
COPY server ./server

FROM scratch AS runtime
COPY --from=build /opt/build/server /usr/local/bin/server
ENTRYPOINT ["/usr/local/bin/server"]
```

Only prior stages can be referenced. Stage filesystems are held in a temporary workspace; intermediate stage tags are not published.

## External image COPY

A Dockerfile can copy files from a separately tagged image without creating another build stage:

```dockerfile
FROM scratch
COPY --from=registry.example/tools:1.2 /usr/bin/compiler /usr/local/bin/compiler
```

The source image must already exist in Boxr's verified local image store. Run `boxr image pull registry.example/tools:1.2` first. Builds never perform an implicit network pull. Boxr checks the source image's integrity and Linux platform, unpacks it into the private build workspace once per reference, and removes that workspace after the build. A missing image, corrupt image, or platform mismatch aborts the build before the final tag is published.

## COPY permissions

Use `COPY --chmod=MODE` to set permissions on copied files without changing the build context:

```dockerfile
FROM scratch
COPY --chmod=0755 scripts/entrypoint.sh /usr/local/bin/entrypoint
COPY --chmod=0640 config/ /etc/myapp/
```

Modes use three- or four-digit octal notation and are limited to ordinary permission bits. Setuid, setgid, sticky and symbolic modes are rejected as a security boundary. Symlinks are never followed when applying the mode, and host source permissions are left unchanged.

## ADD archive handling

Local ADD sources are resolved only inside the build context. Plain `.tar` and gzip-compressed `.tar.gz`/`.tgz` archives are unpacked into a private staging directory before their contents are copied into the image. Extraction is bounded by rootfs file-count and uncompressed-byte limits and rejects traversal, unsafe links, duplicate entries and special files. A failed extraction aborts the build without publishing the image. Remote URL and Git ADD sources are deliberately rejected; fetch them explicitly into the build context first.

## Build arguments

Use repeatable `--build-arg NAME=VALUE` flags to override declared `ARG` defaults:

```sh
boxr image build --tag example/api:dev --build-arg APP_VERSION=1.4.2 --build-arg TARGET=linux/amd64 -f Dockerfile .
```

Arguments become available only when declared with `ARG`. An argument declared before `FROM` can parameterize the base reference; declare it again after `FROM` to make it available to later `RUN` steps. Build arguments are not written into the resulting image's `ENV` metadata. Unknown or unused command-line arguments are rejected to catch spelling mistakes.

## RUN isolation and networking

Each `RUN` executes against the staged image rootfs, after preceding `COPY` and `WORKDIR` steps. Build commands run in Boxr's PID, UTS, mount, IPC and user namespaces, with a private network namespace by default, a clean environment, all Linux capabilities dropped, `no_new_privileges`, and the built-in baseline seccomp profile. Dockerfile `ARG` and `ENV` values are passed to that instruction; host environment variables are not inherited.

The default network is `none`:

```sh
boxr image build --tag example/api:offline -f Dockerfile .
```

Network access is a deliberate opt-in. `--network host` exposes the host network to build commands:

```sh
boxr image build --tag example/api:online --network host -f Dockerfile .
```

`USER` metadata is preserved in the resulting image, but `RUN` after a non-root `USER` is rejected for now; build commands currently execute as root inside a user namespace. A base image with a non-root default user is subject to the same restriction unless the Dockerfile switches back to root before `RUN`.


Dockerfile `HEALTHCHECK` supports exec-form and shell-form probes, bounded interval/timeout/start-period/start-interval settings, retry limits, and `NONE` to disable an inherited image healthcheck.