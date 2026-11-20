# Multi-service deployment manifests

Boxr deployment manifests describe an application as a set of individually isolated services with an explicit dependency graph. The planner is deterministic and validates all services before any workload is started.

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
      "command": ["/usr/bin/database", "--foreground"],
      "options": {
        "rootfs": "/var/lib/boxr/rootfs/database",
        "readOnlyRootfs": true,
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

Each service uses the existing `container.Options` validation rules. The service command is an argument array, not a shell string. A service can list up to 64 dependencies; a deployment can contain up to 64 services. The manifest and command data are bounded to avoid unbounded input.

## Deterministic ordering

Boxr uses a stable topological sort. A service is ordered after each dependency, and independent services are ordered lexically when they are ready at the same time. This makes repeated plans for the same file identical and suitable for review and automation. A cycle is rejected before a plan is returned.

## Secret handling

A plan reports the number of command arguments, environment entries and labels without including their values or command text. It does not include host rootfs paths. JSON output is suitable for CI logs and deployment review without exposing the most common configuration secrets.

## Scope and limitations

This release adds manifest validation and planning only. It does not claim Compose compatibility or perform `up`, `down`, service restart policies, health checks, service DNS, bridge networking, port publishing, secret mounts, rolling updates or cross-host scheduling. A validated plan is not proof that its host resources or rootfs paths exist. Run workloads with `boxr run` after reviewing the plan.
