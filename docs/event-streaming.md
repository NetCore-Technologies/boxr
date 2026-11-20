# Container event streaming

Use `boxr events --follow` to watch lifecycle events as Boxr records them.

```bash
boxr events --follow
boxr events --follow --action die --container web
boxr events --follow --format json --interval 250ms --count 25
boxr events --follow --since 10m --format json
```

Without `--since`, follow mode begins at the current end of the event log and reports only new events. With `--since`, it first emits retained matching events from that time, limited by `--limit`, then continues following.

The default poll interval is 500 milliseconds. Intervals below 100 milliseconds are rejected. A positive `--count` exits after that many matching events; zero continues until interrupted. JSON mode writes compact JSON Lines, one event per line, so downstream tools can consume events incrementally. The one-shot `boxr events` output remains an array in JSON mode.

If the event log is truncated while following, the reader resumes from the beginning of the new log. Event-source and output errors are reported to stderr.
