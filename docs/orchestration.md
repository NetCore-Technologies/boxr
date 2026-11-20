# Container orchestration

Use `boxr wait` to synchronize scripts with container lifecycle transitions.

```bash
# Wait for startup readiness
boxr wait --condition running --timeout 10s web

# Wait for completion and preserve the existing exit-code output
boxr wait --condition stopped --timeout 30m web

# Wait for several containers and return machine-readable results
boxr wait --condition running --json api worker
```

The default condition is `stopped`, as in earlier Boxr versions. The new `running` condition is useful as a readiness gate: it succeeds as soon as the persisted record enters the running state, but fails if the process exits before that transition. The default poll interval is 100 ms; values below 10 ms are rejected. A timeout of zero means no timeout.

With `--json`, successful waits are returned as an array with each container ID, requested condition, resulting state, optional exit code and elapsed milliseconds. When one of several waits fails, successful results are still reported and the command exits nonzero.
