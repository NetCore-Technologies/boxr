# Dockerfile builds

Boxr can parse and build a bounded, non-executing Dockerfile subset:

- One `FROM` stage, using `scratch` or a base image already pulled into Boxr's local image store.
- Local-context `COPY` (including JSON form and `COPY .`), with paths kept inside the build context.
- `WORKDIR`, `ENV`, `LABEL`, `USER`, and shell/exec forms of `CMD` and `ENTRYPOINT`.
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

`RUN`, `ADD`, multi-stage `FROM`, `COPY --from`, build secrets, cache mounts, remote contexts, and build-time package installation are not implemented. They fail explicitly. Boxr does not run Dockerfile commands with host privileges. The builder currently flattens the result into Boxr's canonical local image layer, so it does not preserve source layer history or Docker build cache metadata.

This is a compatibility subset, not a complete Dockerfile frontend or BuildKit replacement.
