# Importing Docker save archives

Boxr imports standard Docker save archives with the existing image load command:

    docker save -o app.tar app:latest
    boxr image load app.tar

A gzip-compressed Docker save tar is accepted too:

    gzip -c app.tar > app.tar.gz
    boxr image load app.tar.gz

The loader recognizes a Docker archive by its root manifest.json, accepts plain tar or gzip-compressed tar input, and validates the complete archive before publishing image tags. Manifest and config JSON are bounded; archive entry count and uncompressed size are capped; path traversal, duplicate/conflicting paths, outer-archive symlinks and special files are rejected.

For each manifest entry, Boxr validates every repository tag, Linux platform declaration, runtime configuration and rootfs diff_ids list. It streams each layer to verify its SHA-256 digest over the uncompressed tar stream before applying the layer. Layers are applied in manifest order. Docker .wh.NAME entries remove lower-layer paths, and .wh..wh..opq entries clear the lower contents of an opaque directory before files from the current layer are applied. Safe in-root relative symlinks and hard links are retained; escaping relative symlinks and device/FIFO entries are rejected.

Supported runtime metadata—environment, entrypoint, command, working directory, user and labels—is retained. All RepoTags in the archive are imported. A failed manifest, path, config or layer validation leaves the local image index unchanged. After successful validation, the merged root filesystem is flattened into Boxr's native single rootfs layer so existing image inspect, verify, audit, unpack, run --image, save and push workflows can consume it.

This archive import support does not change the registry format supported by image pull and image push: direct OCI Distribution pulls and pushes still support Boxr's own single-rootfs-layer media type, not arbitrary multi-layer Docker/OCI registry manifests. The importer also does not preserve extended attributes, file capabilities, ownership IDs, device nodes or other special filesystem objects. Do not use archive import as a trust boundary for untrusted images; run image audit before running an imported image.
