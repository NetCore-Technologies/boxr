# Boxr support bundles

Use `boxr support-bundle --output PATH` to create a compact ZIP archive for bug reports.

## Contents

The archive contains `report.json` (schema version, generation time, Boxr build metadata, Go runtime/OS/architecture, and read-only `doctor` check names/statuses) and `README.txt` (privacy summary and sharing reminder).

Diagnostic details are deliberately excluded because they may contain host paths or other machine-specific values. The archive does not include environment variables or values, command arguments, container labels, config files, container logs, container state, or filesystem paths.

## Safe output handling

The output directory must already exist and must be a real directory rather than a symlink. Symlink and special-file destinations are rejected. Existing regular files are not replaced unless `--force` is supplied. The archive and both entries use mode `0600`. The command prints archive byte size and SHA-256 digest; use `--json` for machine-readable metadata.

Review `report.json` before sharing the archive. It contains diagnostic names and statuses, which may reveal which capabilities are available on the machine.
