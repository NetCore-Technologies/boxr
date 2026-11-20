# Registry image support

Boxr's registry pull path accepts schema-2 OCI image manifests and Docker distribution manifests, including multi-platform indexes and manifest lists.

```sh
boxr image pull --platform linux/amd64 ghcr.io/example/service:v1
boxr image pull --platform linux/arm64 ghcr.io/example/service:v1
boxr image inspect ghcr.io/example/service:v1
boxr image verify ghcr.io/example/service:v1
```

## Layer formats

The puller accepts these layer media types:

- `application/vnd.oci.image.layer.v1.tar`
- `application/vnd.oci.image.layer.v1.tar+gzip`
- `application/vnd.docker.image.rootfs.diff.tar`
- `application/vnd.docker.image.rootfs.diff.tar.gzip`
- Boxr's existing `application/vnd.boxr.rootfs.v1.tar+gzip`

Zstandard-compressed and foreign/non-distributable layers are not currently supported. Unsupported media types fail before the layer is downloaded; Boxr never guesses the compression format from a filename.

## Download performance

Independent image layers are downloaded and integrity-checked with a bounded pool of up to four workers. This reduces pull latency for multi-layer images without creating one goroutine or network request per layer. Results are retained by manifest index, so filesystem application remains strictly ordered. A failed download or digest check cancels outstanding requests and removes all temporary layer files created by the pull.

Concurrent requests share a synchronized Bearer-token cache. A group of requests that receives an authentication challenge performs a single token refresh instead of racing to issue duplicate token requests.

## Verification and extraction

Before a pull is returned, Boxr validates manifest and config descriptors, selects the requested Linux platform, and checks that the config has one valid `rootfs.diff_ids` entry per manifest layer. Each layer download is checked against its descriptor SHA-256 digest and declared size. The uncompressed tar stream is then hashed and compared with the matching config diff ID.

Verified layers are applied in manifest order. The extractor supports OCI/Docker whiteouts and opaque-directory markers, regular files, directories, safe symlinks and safe hard links. It rejects path traversal, escaping relative symlinks, duplicate/conflicting entries, special device entries, oversized archives and invalid hard links. The resulting filesystem is imported through Boxr's local content-addressed image store; registry layer boundaries are not preserved locally.

## Resource bounds

- At most 128 layers per image.
- At most 8 GiB compressed bytes per layer.
- At most 16 GiB aggregate compressed layer bytes per image.
- At most 8 GiB uncompressed bytes per layer.
- Manifest and image config JSON remain bounded by the registry client's existing limits.

These limits bound metadata and layer downloads; they are not a substitute for adequate disk-space planning.

## Push compatibility

Registry pushes continue to publish Boxr's canonical single-rootfs-layer OCI image representation. This avoids claiming support for arbitrary multi-layer local image graphs. A pulled Docker/OCI image can still be run and re-exported using `boxr image save`.
