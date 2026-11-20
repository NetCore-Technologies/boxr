# Container inspection and disk usage

Use `boxr inspect ID|NAME` for the normal persisted container record. Add `--size` to include the on-disk sizes of the state record and log:

```bash
boxr inspect --size web
```

The extended JSON places the existing record under `record` and a `size` object beside it. That object contains `stateBytes`, `logBytes`, and `totalBytes`. A missing log contributes zero bytes. The size scanner uses `Lstat` and rejects symlink or special-file targets rather than following them.
