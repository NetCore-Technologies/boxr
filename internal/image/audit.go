package image

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/manit6752025/boxr/internal/rootfs"
)

// AuditReport combines verified OCI metadata with a bounded filesystem audit.
// IntegrityVerified is true only after the manifest, config, compressed layer,
// and uncompressed rootfs diff ID have all been checked.
type AuditReport struct {
	Image             Image              `json:"image"`
	ManifestDigest    string             `json:"manifestDigest"`
	ConfigDigest      string             `json:"configDigest"`
	LayerDigests      []string           `json:"layerDigests"`
	LayerCount        int                `json:"layerCount"`
	LayerMediaTypes   []string           `json:"layerMediaTypes"`
	IntegrityVerified bool               `json:"integrityVerified"`
	RootFS            rootfs.AuditReport `json:"rootfs"`
}

// Audit verifies the complete local image graph and then audits the extracted
// rootfs. The layer is copied to a private snapshot while the image-store lock
// is held so concurrent tag removal or pruning cannot invalidate the scan.
func Audit(reference string, maxEntries int64) (AuditReport, error) {
	var report AuditReport
	if maxEntries <= 0 {
		return report, fmt.Errorf("image audit entry limit must be positive")
	}
	if err := ValidateReference(reference); err != nil {
		return report, err
	}

	snapshotDir, err := os.MkdirTemp("", "boxr-image-audit-snapshot-*")
	if err != nil {
		return report, fmt.Errorf("create image audit snapshot: %w", err)
	}
	defer os.RemoveAll(snapshotDir)
	snapshotPath := filepath.Join(snapshotDir, "layer.tar.gz")

	storeMu.Lock()
	dir, err := ensureStore()
	if err != nil {
		storeMu.Unlock()
		return report, err
	}
	index, err := loadIndex(dir)
	if err != nil {
		storeMu.Unlock()
		return report, err
	}
	var selected *Descriptor
	for i := range index.Manifests {
		if index.Manifests[i].Annotations["org.opencontainers.image.ref.name"] == reference {
			copy := index.Manifests[i]
			selected = &copy
			break
		}
	}
	if selected == nil {
		storeMu.Unlock()
		return report, fmt.Errorf("image %q not found", reference)
	}
	item, err := inspectDescriptor(dir, reference, *selected)
	if err != nil {
		storeMu.Unlock()
		return report, fmt.Errorf("verify image integrity: %w", err)
	}
	manifest, err := loadManifest(dir, *selected)
	if err != nil {
		storeMu.Unlock()
		return report, fmt.Errorf("load image manifest: %w", err)
	}
	layerPath, err := verifyBlob(dir, manifest.Layers[0])
	if err != nil {
		storeMu.Unlock()
		return report, fmt.Errorf("verify image layer: %w", err)
	}
	if err := copyFileAtomic(layerPath, snapshotPath, 0600); err != nil {
		storeMu.Unlock()
		return report, fmt.Errorf("snapshot image layer: %w", err)
	}
	snapshotDigest, snapshotSize, err := digestFile(snapshotPath, maxBlobBytes)
	storeMu.Unlock()
	if err != nil {
		return report, fmt.Errorf("verify image audit snapshot: %w", err)
	}
	if snapshotDigest != manifest.Layers[0].Digest || snapshotSize != manifest.Layers[0].Size {
		return report, fmt.Errorf("image layer changed while creating audit snapshot")
	}

	rootPath := filepath.Join(snapshotDir, "rootfs")
	if _, err := rootfs.ExtractTarGz(snapshotPath, rootPath, rootfs.Limits{}); err != nil {
		return report, fmt.Errorf("extract image rootfs for audit: %w", err)
	}
	filesystem, err := rootfs.AuditWithLimit(rootPath, maxEntries)
	if err != nil {
		return report, fmt.Errorf("audit image rootfs: %w", err)
	}
	filesystem.Root = reference

	report = AuditReport{
		Image:             item,
		ManifestDigest:    selected.Digest,
		ConfigDigest:      manifest.Config.Digest,
		LayerDigests:      make([]string, 0, len(manifest.Layers)),
		LayerCount:        len(manifest.Layers),
		LayerMediaTypes:   make([]string, 0, len(manifest.Layers)),
		IntegrityVerified: true,
		RootFS:            filesystem,
	}
	for _, layer := range manifest.Layers {
		report.LayerDigests = append(report.LayerDigests, layer.Digest)
		report.LayerMediaTypes = append(report.LayerMediaTypes, layer.MediaType)
	}
	return report, nil
}

// HasFindings reports whether the rootfs portion contains warning or high
// severity findings. Informational findings do not fail strict mode.
func (r AuditReport) HasFindings() bool {
	return r.RootFS.HasFindings()
}
