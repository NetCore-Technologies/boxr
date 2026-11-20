package rootfs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const DefaultAuditMaxEntries int64 = 1_000_000

type AuditFinding struct {
	Path     string `json:"path"`
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

type AuditReport struct {
	Root           string         `json:"root"`
	EntriesScanned int64          `json:"entriesScanned"`
	HighCount      int            `json:"highCount"`
	WarningCount   int            `json:"warningCount"`
	InfoCount      int            `json:"infoCount"`
	Findings       []AuditFinding `json:"findings"`
}

// Audit checks filesystem metadata without following symlinks. It is a
// preflight aid, not a malware scanner or a guarantee that a rootfs is safe.
func Audit(root string) (AuditReport, error) {
	return AuditWithLimit(root, DefaultAuditMaxEntries)
}

// AuditWithLimit bounds the number of filesystem entries inspected. A limit
// must be positive; the scan fails rather than silently returning a partial
// report when the limit is exceeded.
func AuditWithLimit(root string, maxEntries int64) (AuditReport, error) {
	var report AuditReport
	if maxEntries <= 0 {
		return report, fmt.Errorf("rootfs audit entry limit must be positive")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return report, fmt.Errorf("resolve rootfs path: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return report, fmt.Errorf("inspect rootfs path: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return report, fmt.Errorf("rootfs audit target must be a real directory, not a symlink")
	}
	report.Root = absolute
	report.Findings = make([]AuditFinding, 0)
	add := func(relative, code, severity, message string) {
		report.Findings = append(report.Findings, AuditFinding{Path: filepath.ToSlash(relative), Code: code, Severity: severity, Message: message})
		switch severity {
		case "high":
			report.HighCount++
		case "warning":
			report.WarningCount++
		case "info":
			report.InfoCount++
		}
	}
	walkErr := filepath.WalkDir(absolute, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == absolute {
			return nil
		}
		if report.EntriesScanned >= maxEntries {
			return fmt.Errorf("rootfs audit exceeded the %d-entry limit", maxEntries)
		}
		report.EntriesScanned++
		relative, err := filepath.Rel(absolute, path)
		if err != nil {
			return err
		}
		modeInfo, err := os.Lstat(path)
		if err != nil {
			return err
		}
		mode := modeInfo.Mode()
		if mode&os.ModeSetuid != 0 {
			add(relative, "setuid", "warning", "setuid executable or file can change effective identity")
		}
		if mode&os.ModeSetgid != 0 {
			add(relative, "setgid", "warning", "setgid executable or file can change effective group")
		}
		if mode&os.ModeSymlink == 0 && mode.Perm()&0002 != 0 {
			if mode.IsDir() && mode&os.ModeSticky != 0 {
				add(relative, "world-writable-sticky-directory", "info", "world-writable directory has the sticky bit")
			} else {
				add(relative, "world-writable", "warning", "path is writable by every local user")
			}
		}
		if mode&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if !filepath.IsAbs(target) {
				resolved := filepath.Clean(filepath.Join(filepath.Dir(path), target))
				relativeTarget, err := filepath.Rel(absolute, resolved)
				if err != nil {
					return err
				}
				if relativeTarget == ".." || strings.HasPrefix(relativeTarget, ".."+string(filepath.Separator)) {
					add(relative, "escaping-symlink", "high", "relative symlink resolves outside the rootfs")
					return nil
				}
			}
			// Never stat a symlink target: absolute links are interpreted relative
			// to the container root after chroot, and chained links could escape
			// even when this link's immediate target is lexically inside the tree.
			return nil
		}
		if !mode.IsDir() && !mode.IsRegular() {
			add(relative, "special-file", "high", "rootfs contains a device, FIFO, socket or other special file")
		}
		return nil
	})
	if walkErr != nil {
		return AuditReport{}, fmt.Errorf("walk rootfs for audit: %w", walkErr)
	}
	return report, nil
}

// HasFindings reports whether the audit found at least one warning or high
// severity item. Informational sticky directories do not fail strict mode.
func (r AuditReport) HasFindings() bool {
	return r.HighCount > 0 || r.WarningCount > 0
}
