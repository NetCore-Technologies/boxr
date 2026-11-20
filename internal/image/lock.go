package image

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const maxLockBytes = 64 << 10

// LockFile pins an image reference to the immutable manifest digest and
// platform metadata observed when the lock was created.
type LockFile struct {
	SchemaVersion int    `json:"schemaVersion"`
	Reference     string `json:"reference"`
	Digest        string `json:"digest"`
	OS            string `json:"os"`
	Architecture  string `json:"architecture"`
	Variant       string `json:"variant,omitempty"`
}

// CreateLock verifies an image and returns its immutable reference record.
func CreateLock(reference string) (LockFile, error) {
	verified, err := Verify(reference)
	if err != nil {
		return LockFile{}, fmt.Errorf("verify image before locking: %w", err)
	}
	return LockFile{
		SchemaVersion: 1,
		Reference:     verified.Reference,
		Digest:        verified.Digest,
		OS:            verified.OS,
		Architecture:  verified.Arch,
		Variant:       verified.Variant,
	}, nil
}

// WriteLock stores a lock atomically as a private 0600 JSON file. It refuses
// symlinked paths and non-regular destinations.
func WriteLock(path, reference string) (LockFile, error) {
	lock, err := CreateLock(reference)
	if err != nil {
		return LockFile{}, err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return LockFile{}, err
	}
	parent := filepath.Dir(absolute)
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return LockFile{}, fmt.Errorf("inspect lock directory: %w", err)
	}
	if !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return LockFile{}, fmt.Errorf("lock parent must be a real directory, not a symlink")
	}
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return LockFile{}, err
	}
	if resolvedParent != parent {
		return LockFile{}, fmt.Errorf("lock path cannot traverse symlinked directories")
	}
	if info, err := os.Lstat(absolute); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return LockFile{}, fmt.Errorf("lock destination must be a regular file, not a symlink or special file")
		}
	} else if !os.IsNotExist(err) {
		return LockFile{}, err
	}
	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return LockFile{}, err
	}
	data = append(data, '\n')
	if err := atomicWrite(absolute, data, 0600); err != nil {
		return LockFile{}, fmt.Errorf("write image lock: %w", err)
	}
	return lock, nil
}

// ReadLock parses a bounded, strict JSON image lock file.
func ReadLock(path string) (LockFile, error) {
	var lock LockFile
	info, err := os.Lstat(path)
	if err != nil {
		return lock, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > maxLockBytes {
		return lock, fmt.Errorf("image lock must be a regular non-symlink file no larger than %d bytes", maxLockBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return lock, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxLockBytes+1))
	if err != nil {
		return lock, err
	}
	if len(data) > maxLockBytes {
		return lock, fmt.Errorf("image lock exceeds %d bytes", maxLockBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&lock); err != nil {
		return lock, fmt.Errorf("decode image lock: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return lock, fmt.Errorf("image lock contains trailing JSON data")
	}
	if lock.SchemaVersion != 1 {
		return lock, fmt.Errorf("unsupported image lock schema version %d", lock.SchemaVersion)
	}
	if err := ValidateReference(lock.Reference); err != nil {
		return lock, fmt.Errorf("invalid image lock reference: %w", err)
	}
	if !hexRE.MatchString(stringsTrimSHA256(lock.Digest)) || lock.OS != "linux" || lock.Architecture == "" {
		return lock, fmt.Errorf("image lock has invalid digest or platform metadata")
	}
	return lock, nil
}

// VerifyLock compares the lock's immutable digest and platform with the
// current local image store entry. A moved tag is reported as a mismatch.
func VerifyLock(path string) (LockFile, error) {
	lock, err := ReadLock(path)
	if err != nil {
		return LockFile{}, err
	}
	image, err := Verify(lock.Reference)
	if err != nil {
		return LockFile{}, fmt.Errorf("verify locked image %q: %w", lock.Reference, err)
	}
	if image.Digest != lock.Digest || image.OS != lock.OS || image.Arch != lock.Architecture || image.Variant != lock.Variant {
		return LockFile{}, fmt.Errorf("image lock mismatch for %q: expected %s (%s/%s/%s), found %s (%s/%s/%s)", lock.Reference, lock.Digest, lock.OS, lock.Architecture, lock.Variant, image.Digest, image.OS, image.Arch, image.Variant)
	}
	return lock, nil
}

func stringsTrimSHA256(digest string) string {
	const prefix = "sha256:"
	if len(digest) >= len(prefix) && digest[:len(prefix)] == prefix {
		return digest[len(prefix):]
	}
	return ""
}
