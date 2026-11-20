# Multi-service deployment manifests

Boxr deployment manifests describe an application as a set of individually isolated services with an explicit dependency graph. The planner is deterministic and validates all services before any workload is started. The runtime supports attached service supervision, stack-scoped status and reverse-order shutdown.

## Create and inspect a plan

```sh
boxr deploy example > boxr.deploy.json
boxr deploy validate boxr.deploy.json
boxr deploy plan boxr.deploy.json
boxr deploy plan --json boxr.deploy.json
```

Version 1 is strict JSON. Unknown fields, trailing JSON values, oversized files, unsupported versions, malformed names, invalid commands, invalid container options, missing dependencies, duplicate dependency declarations, self-dependencies and dependency cycles are rejected.

## Manifest structure

```json
{
  "version": 1,
  "name": "sample-app",
  "services": {
    "database": {
      "image": "alpine:3.21",
      "command": ["/bin/sh", "-c", "while true; do sleep 3600; done"],
      "options": {
        "securityProfile": "baseline",
        "memoryLimitBytes": 536870912,
        "pidsLimit": 128
      }
    },
    "web": {
      "command": ["/usr/bin/web", "--listen", "0.0.0.0:8080"],
      "options": {
        "rootfs": "/var/lib/boxr/rootfs/web",
        "securityProfile": "baseline",
        "memoryLimitBytes": 268435456,
        "pidsLimit": 64
      },
      "dependsOn": ["database"]
    }
  }
}
```

Each service uses the existing `container.Options` validation rules. Define either an OCI image reference, a command, or both. The command is an argument array, not a shell string. Run `boxr deploy pull FILE.json` to prefetch all unique image references before startup; `--platform linux/ARCH[/VARIANT]` selects the target and `--authfile PATH` supplies Docker/Podman registry credentials. Already available local images are verified and skipped, and equivalent Docker Hub shorthand/canonical references are deduplicated. Images are checked again immediately before `deploy up`. A service can list up to 64 dependencies; a deployment can contain up to 64 services. The manifest and command data are bounded to avoid unbounded input.

## Deployment image prefetch

```sh
boxr deploy pull boxr.deploy.json
boxr deploy pull --platform linux/arm64 --authfile ~/.docker/config.json boxr.deploy.json
```

The pull command walks services in deterministic order, skips services without image references, verifies images already in the local OCI store and downloads each unique missing registry reference once. It reuses the normal Boxr registry credential resolution and OCI/Docker layer verification path. A failed pull stops the prefetch operation before any service is started; successfully imported earlier images remain cached and can be reused on the next attempt. `deploy up` resolves each service image to the actual local tag and performs another integrity check before creating a service process.
## Runtime lifecycle

```sh
boxr deploy up boxr.deploy.json
boxr deploy ps sample-app
boxr deploy down sample-app
boxr deploy down --time 20s --remove sample-app
```

`deploy up` runs in the foreground, creates a private temporary run config for each service, and starts services using the existing `boxr run` runtime. Services whose dependencies are satisfied form deterministic startup waves and are launched concurrently. Boxr waits for every service in a wave to reach the runtime's `running` state before starting the next wave. It does not claim that an application-level health check has passed. A service exit is fail-fast: the supervisor stops the remaining services in reverse startup order and exits with the service's status where available. Ctrl+C or SIGTERM requests graceful shutdown; services that exceed the stop timeout are killed.

Each service receives a stable stack-scoped container name and reserved metadata labels so `deploy ps` and `deploy down` only operate on that deployment. `deploy ps --json` omits command text, environment values, labels and host paths. Existing records with the same generated names must be removed with `deploy down --remove` before reusing a stack name. `deploy down` keeps completed records and logs by default; use `--remove` to delete them.

The supervisor is attached and must remain running to monitor unexpected service exits. Detached mode, restart policies, application health checks, service DNS, bridge networking, port publishing, cross-host scheduling and rolling updates are not implemented by this milestone. Dependencies currently guarantee start order only; they do not wait for a TCP port or application readiness.

## Deterministic ordering

Boxr uses a stable topological sort and exposes both the flattened execution order and parallel start groups. Services in the same group have no unmet dependencies and are ordered lexically. This makes repeated plans for the same file identical and allows independent services to start concurrently. A cycle is rejected before a plan is returned.

## Secret handling

A plan reports the number of command arguments, environment entries and labels without including their values or command text. It does not include host rootfs paths. JSON output is suitable for CI logs and deployment review without exposing the most common configuration secrets.

## Scope and limitations

This release provides validation, planning and an attached single-host lifecycle supervisor. It does not claim Compose compatibility or provide detached mode, restart policies, health checks, service DNS, bridge networking, port publishing, secret mounts, rolling updates or cross-host scheduling. A validated plan is not proof that its host resources or rootfs paths exist. Run workloads with `boxr run` after reviewing the plan.
