package ioutil

import (
	"fmt"
	"os"
	"path/filepath"
)

type tempFile interface {
	Name() string
	Write([]byte) (int, error)
	Sync() error
	Chmod(os.FileMode) error
	Close() error
}

// WriteFileAtomic writes data to path atomically via a sibling temporary file:
// MkdirAll(dirPerm) -> CreateTemp -> Write -> Sync -> Chmod(filePerm) -> Close -> Rename.
func WriteFileAtomic(path string, data []byte, filePerm, dirPerm os.FileMode) error {
	return writeFileAtomic(path, data, filePerm, dirPerm, func(dir, pattern string) (tempFile, error) {
		return os.CreateTemp(dir, pattern)
	})
}

func writeFileAtomic(path string, data []byte, filePerm, dirPerm os.FileMode, createTemp func(dir, pattern string) (tempFile, error)) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("create directory %q: %w", dir, err)
	}

	tmp, err := createTemp(dir, ".gcp-arch-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file in %q: %w", dir, err)
	}
	tmpPath := tmp.Name()
	closed := false
	committed := false
	defer func() {
		if !closed {
			_ = tmp.Close()
		}
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp file %q: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp file %q: %w", tmpPath, err)
	}
	if err := tmp.Chmod(filePerm); err != nil {
		return fmt.Errorf("chmod %o on temp file %q: %w", filePerm, tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		closed = true
		return fmt.Errorf("close temp file %q: %w", tmpPath, err)
	}
	closed = true

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename temp file to %q: %w", path, err)
	}
	committed = true
	return nil
}
