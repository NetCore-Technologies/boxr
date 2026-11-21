# Runtime monitoring

Boxr exposes cgroup v2 counters for active containers.

## Snapshot

`boxr stats ID|NAME` returns one JSON object, preserving the original command behaviour.

## Watch

Use `--watch` to emit timestamped JSON Lines samples:

```bash
boxr stats --watch CONTAINER
boxr stats --watch --interval 500ms --count 20 CONTAINER
```

Each output line contains a UTC `sampledAt` timestamp and a `stats` object. The default sampling interval is one second; intervals shorter than 100 milliseconds are rejected. A count of zero (the default) continues until the container exits, while a positive count stops after exactly that many samples.

The stream is newline-delimited JSON rather than one enclosing JSON array, so it can be consumed incrementally by tools such as `jq` or redirected to a log file. Watch mode reports cgroup-read and output-stream failures instead of silently dropping samples.


## Table output

`boxr stats --format table ID|NAME` prints a single readable snapshot. Adding `--watch` emits a header once and then timestamped rows at the chosen interval. JSON remains the default and is unchanged.

## Docker Engine API stats

`GET /containers/{id}/stats?stream=false` returns Docker-shaped CPU, memory, PID, block-I/O and per-interface network counters from cgroup v2 and the verified container network namespace. Streaming mode emits one JSON sample per second and carries the previous CPU counters in `precpu_stats`, allowing Docker clients to calculate interval CPU usage. Docker-created containers create a Boxr cgroup even without explicit limits; Boxr enables optional telemetry controllers only when they are already delegated by the host. Unlimited memory limits fall back to host physical memory, and unavailable optional cgroup controllers are not treated as enforced limits.
