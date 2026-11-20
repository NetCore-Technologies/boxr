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
      "readiness": {
        "httpUrl": "http://127.0.0.1:8080/healthz",
        "intervalMs": 250,
        "timeoutMs": 500,
        "attempts": 40,
        "expectedStatus": 200
      },
      "dependsOn": ["database"]
    }
  }
}
```

Each service uses the existing `container.Options` validation rules. Define either an OCI image reference, a command, or both. The command is an argument array, not a shell string. Run `boxr deploy pull FILE.json` to prefetch all unique image references before startup; `--platform linux/ARCH[/VARIANT]` selects the target and `--authfile PATH` supplies Docker/Podman registry credentials. Already available local images are verified and skipped only when their OS, architecture and any explicitly requested variant match the target platform; equivalent Docker Hub shorthand/canonical references are deduplicated. `deploy up` checks cached images again against the host Linux architecture before starting services. A service can list up to 64 dependencies; a deployment can contain up to 64 services. The manifest and command data are bounded to avoid unbounded input.

## Deployment image prefetch

```sh
boxr deploy pull boxr.deploy.json
boxr deploy pull --platform linux/arm64 --authfile ~/.docker/config.json boxr.deploy.json
```

The pull command walks services in deterministic order, skips services without image references, verifies images already in the local OCI store and downloads each unique missing registry reference once. It reuses the normal Boxr registry credential resolution and OCI/Docker layer verification path. A failed pull stops the prefetch operation before any service is started; successfully imported earlier images remain cached and can be reused on the next attempt. `deploy up` resolves each service image to the actual local tag and performs another integrity check before creating a service process.
## Read deployment logs

```sh
boxr deploy logs --tail 100 sample-app
boxr deploy logs --follow sample-app
boxr deploy logs sample-app api
```

Stack logs are printed in dependency order with a `[service]` prefix on each line. `--tail` is applied independently to each service; `--tail 0` prints each recorded log in full. `--follow` continues polling the selected service log files until all selected services stop, drains their final output, and exits on Ctrl+C or SIGTERM. Supply a service name to restrict output to a single service. Logs contain application output written by the container; review application logging carefully before sharing it.
## Wait for a deployment

```sh
boxr deploy wait --condition running sample-app
boxr deploy wait --condition stopped --timeout 60s sample-app
boxr deploy wait --condition running --timeout 45s --interval 250ms --json sample-app
```

The default condition is `running` with a 30-second deadline. A running wait succeeds only when every recorded service is running; it fails immediately if a service exits or fails before the entire stack is ready. A stopped wait succeeds when no recorded service is running. A timeout of `0` waits without a deadline. JSON output contains service names, container names, IDs, statuses and timing, but no commands, environment values, labels or host paths.
## Persistent named volumes

Create volumes before deploying services:

```sh
boxr volume create database-data
boxr volume create uploads
boxr deploy validate boxr.deploy.json
boxr deploy up boxr.deploy.json
```

A service declares named mounts as `volumes: [{"name":"database-data","target":"/var/lib/database"}]`; set `readOnly: true` for read-only mounts. A service must configure a rootfs to receive named volumes. The manifest validator checks volume names, absolute targets, traversal, duplicate targets and conflicts with bind/tmpfs mounts. Before starting any service, `deploy up` verifies that every referenced volume exists in Boxr's owner-private volume store, then resolves it to the host path and passes it through the existing safe bind-mount runtime. Missing volumes fail the deployment before the first container record is created. Volume data persists across `deploy down` and service recreation; removing a deployment does not remove named volumes.

## Runtime lifecycle

```sh
boxr deploy up boxr.deploy.json
boxr deploy ps sample-app
boxr deploy wait --condition running sample-app
boxr deploy down sample-app
boxr deploy down --time 20s --remove sample-app
```

`deploy up` runs in the foreground, creates a private temporary run config for each service, and starts services using the existing `boxr run` runtime. Services whose dependencies are satisfied form deterministic startup waves and are launched concurrently. A service without a readiness probe is considered started when its runtime record reaches `running`. A service with a probe must also pass that application-level check before the next dependency wave is allowed to start. TCP probes connect to an explicit loopback IP and port; HTTP probes issue a GET to a loopback IP URL and require the configured status (default `200`). HTTP redirects and environment proxy settings are not followed. Targets must use literal loopback IP addresses, contain no URL credentials, query or fragment, and require host networking; `networkMode: "none"` and the `restricted` or `untrusted` security profiles cannot be combined with a probe. Probe interval, per-attempt timeout, attempt count and expected status are bounded and validated before any workload starts. The probe target is omitted from plan output. A readiness probe gates startup only: it is not a recurring liveness check and does not restart an unhealthy service after startup. A service exit is fail-fast: the supervisor stops the remaining services in reverse startup order and exits with the service's status where available. Ctrl+C or SIGTERM requests graceful shutdown; services that exceed the stop timeout are killed.

Each service receives a stable stack-scoped container name and reserved metadata labels so `deploy ps` and `deploy down` only operate on that deployment. `deploy ps --json` omits command text, environment values, labels and host paths. Existing records with the same generated names must be removed with `deploy down --remove` before reusing a stack name. `deploy down` keeps completed records and logs by default; use `--remove` to delete them.

The supervisor is attached and must remain running to monitor unexpected service exits. Detached mode, restart policies, service DNS, bridge networking, port publishing, secret mounts, cross-host scheduling and rolling updates are not implemented by this milestone. Dependencies guarantee start order and, when configured, a one-time readiness gate; they do not provide recurring liveness monitoring.

## Deterministic ordering

Boxr uses a stable topological sort and exposes both the flattened execution order and parallel start groups. Services in the same group have no unmet dependencies and are ordered lexically. This makes repeated plans for the same file identical and allows independent services to start concurrently. A cycle is rejected before a plan is returned.

## Secret handling

A plan reports the number of command arguments, environment entries and labels without including their values or command text. It does not include host rootfs paths. JSON output is suitable for CI logs and deployment review without exposing the most common configuration secrets.

## Scope and limitations

This release provides validation, planning and an attached single-host lifecycle supervisor. It does not claim Compose compatibility or provide detached mode, restart policies, health checks, service DNS, bridge networking, port publishing, secret mounts, rolling updates or cross-host scheduling. A validated plan is not proof that its host resources or rootfs paths exist. Run workloads with `boxr run` after reviewing the plan.
