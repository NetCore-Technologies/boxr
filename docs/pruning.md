# Safe container cleanup

Preview which completed container records qualify for removal before changing anything:

```bash
boxr prune --dry-run
boxr prune --older-than 24h --dry-run --json
boxr prune --older-than 7d
```

The dry-run report includes candidate IDs, names, lifecycle states, the cutoff timestamp and the selected age threshold. Running and never-started containers are never candidates. JSON mode is suitable for automation and does not change the command's default output when omitted.

The original `boxr prune` behaviour still performs cleanup. Use `--dry-run` to inspect candidates first; dry-run mode never removes records or their logs.
