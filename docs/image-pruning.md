# Image pruning and reclaimable storage

Boxr verifies every tagged image before pruning unreferenced OCI blobs. Review the exact candidates and byte total first:

```bash
boxr image prune --dry-run --json
boxr image prune --force --json
```

The report includes a list of digest/size pairs, the blob count, and total reclaimable bytes. Dry-run does not delete anything. Actual deletion remains gated by the explicit `--force` option. The original human-readable output remains available.
