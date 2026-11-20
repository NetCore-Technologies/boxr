# Dockerfile builds

Boxr can build a bounded, single-stage Dockerfile subset. `RUN` commands execute in a disposable staged rootfs through Boxr's namespace runtime, not directly on the host:

- One `FROM` stage, using `scratch` or a base image already pulled into Boxr's local image store.
- Local-context `COPY` (including JSON form and `COPY .`), with paths kept inside the build context.
- `RUN` in shell or exec form, `WORKDIR`, `ENV`, `LABEL`, `USER`, and shell/exec forms of `CMD` and `ENTRYPOINT`.
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

`ADD`, multi-stage `FROM`, `COPY --from`, build secrets, cache mounts, remote contexts, `RUN --mount`, and advanced Dockerfile frontend features are not implemented. They fail explicitly. Dockerfile RUN executes in Boxr's namespaces and staged rootfs, but Boxr is not yet a BuildKit replacement. The builder currently flattens the result into Boxr's canonical local image layer, so it does not preserve source layer history or Docker build cache metadata.

This is a compatibility subset, not a complete Dockerfile frontend or BuildKit replacement.

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
