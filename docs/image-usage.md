# Image storage usage

The default `boxr image du` command reports verified store-wide unique, reachable and reclaimable blob bytes.

For a per-tag view, use:

```bash
boxr image du --all
boxr image du --all --filter os=linux --filter arch=arm64 --json
```

The report lists each tag's platform and rootfs layer size, plus a store-wide unique-blob summary. `referencedLayerBytes` is a per-tag sum, so identical shared layers may be counted more than once. The nested `store.totalBytes` is the verified unique-blob total and is the correct figure for physical blob storage. Filters use the same reference, platform, digest and label syntax as `image ls`.

Without `--all`, both the original table and `--json` response shapes are unchanged.
