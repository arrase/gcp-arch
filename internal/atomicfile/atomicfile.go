package atomicfile

import (
	"bufio"
	"fmt"
	"io"
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

// Write writes data to path atomically via a sibling temporary file:
// MkdirAll(dirPerm) -> CreateTemp -> Write -> Sync -> Chmod(filePerm) -> Close -> Rename.
func Write(path string, data []byte, filePerm, dirPerm os.FileMode) error {
	return writeFunc(path, filePerm, dirPerm, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	}, defaultCreateTemp)
}

// WriteFunc streams content produced by fn to path atomically via a buffered sibling temporary file.
func WriteFunc(path string, filePerm, dirPerm os.FileMode, fn func(io.Writer) error) error {
	return writeFunc(path, filePerm, dirPerm, func(w io.Writer) error {
		bw := bufio.NewWriterSize(w, 64*1024)
		if err := fn(bw); err != nil {
			return err
		}
		return bw.Flush()
	}, defaultCreateTemp)
}

func defaultCreateTemp(dir, pattern string) (tempFile, error) {
	return os.CreateTemp(dir, pattern)
}

func writeFunc(
	path string,
	filePerm, dirPerm os.FileMode,
	fn func(io.Writer) error,
	createTemp func(dir, pattern string) (tempFile, error),
) error {
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

	if err := fn(tmp); err != nil {
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
