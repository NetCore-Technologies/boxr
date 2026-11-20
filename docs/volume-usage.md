# Named-volume storage usage

Measure one volume or inventory all named volumes:

```bash
boxr volume du project-cache
boxr volume du --all
boxr volume du --all --filter label=team=demo --json
```

The `--all` form reports each volume's files, directories, symlinks, other entries and logical bytes, followed by aggregate totals. Name and label filters use the same syntax as `volume ls` and `volume prune`. JSON mode returns a summary object containing the per-volume list and totals.

The existing `boxr volume du NAME` output and JSON shape are unchanged.
