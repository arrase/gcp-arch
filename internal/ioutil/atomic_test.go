package ioutil

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFileAtomicCreatesDirsAndPermissions(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "nested", "dir", "secret.snap")
	payload := []byte("snapshot-payload-v2")

	if err := WriteFileAtomic(target, payload, 0o600, 0o700); err != nil {
		t.Fatalf("WriteFileAtomic error: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("content mismatch: got %q, want %q", got, payload)
	}

	fi, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("file perm = %o, want 0600", fi.Mode().Perm())
	}

	dirInfo, err := os.Stat(filepath.Dir(target))
	if err != nil {
		t.Fatalf("stat parent dir: %v", err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("dir perm = %o, want 0700", dirInfo.Mode().Perm())
	}

	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "secret.snap" {
		t.Fatalf("unexpected temp files left behind: %+v", entries)
	}
}

func TestWriteFileAtomicOverwritesExistingFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "diagram.svg")

	if err := WriteFileAtomic(target, []byte("initial"), 0o600, 0o700); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := WriteFileAtomic(target, []byte("<svg/>"), 0o644, 0o755); err != nil {
		t.Fatalf("second write: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read overwritten file: %v", err)
	}
	if string(got) != "<svg/>" {
		t.Fatalf("overwritten content = %q, want <svg/>", got)
	}
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat overwritten file: %v", err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("overwritten perm = %o, want 0644", fi.Mode().Perm())
	}
}

func TestWriteFileAtomicCleansTempOnRenameFailure(t *testing.T) {
	dir := t.TempDir()
	// Make target an existing non-empty directory so os.Rename fails.
	targetDir := filepath.Join(dir, "existing-dir")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := WriteFileAtomic(targetDir, []byte("should-fail"), 0o600, 0o700)
	if err == nil {
		t.Fatal("expected error renaming file over existing directory")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".gcp-arch-") {
			t.Fatalf("temp file %q was not cleaned up after rename failure", e.Name())
		}
	}
}

func TestWriteFileAtomicMkdirFailure(t *testing.T) {
	dir := t.TempDir()
	blockingFile := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blockingFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := WriteFileAtomic(filepath.Join(blockingFile, "child.snap"), []byte("data"), 0o600, 0o700)
	if err == nil {
		t.Fatal("expected error when parent path is a regular file")
	}
}

func TestWriteFileAtomicCreateTempFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses read-only directory permissions")
	}
	dir := filepath.Join(t.TempDir(), "readonly")
	if err := os.MkdirAll(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if err := WriteFileAtomic(filepath.Join(dir, "file.snap"), []byte("data"), 0o600, 0o700); err == nil {
		t.Fatal("expected error creating temp file in read-only directory")
	}
}

type failingTempFile struct {
	*os.File
	writeErr error
	syncErr  error
	chmodErr error
	closeErr error
	closed   bool
}

func (f *failingTempFile) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.File.Write(p)
}

func (f *failingTempFile) Sync() error {
	if f.syncErr != nil {
		return f.syncErr
	}
	return f.File.Sync()
}

func (f *failingTempFile) Chmod(mode os.FileMode) error {
	if f.chmodErr != nil {
		return f.chmodErr
	}
	return f.File.Chmod(mode)
}

func (f *failingTempFile) Close() error {
	f.closed = true
	realErr := f.File.Close()
	if f.closeErr != nil {
		return f.closeErr
	}
	return realErr
}

func TestWriteFileAtomicTempFileFailureCleanup(t *testing.T) {
	cases := []struct {
		name      string
		setupErr  func(f *failingTempFile)
		wantError string
	}{
		{
			name:      "write failure closes and removes temp file",
			setupErr:  func(f *failingTempFile) { f.writeErr = errors.New("disk full") },
			wantError: "write temp file",
		},
		{
			name:      "sync failure closes and removes temp file",
			setupErr:  func(f *failingTempFile) { f.syncErr = errors.New("io error") },
			wantError: "sync temp file",
		},
		{
			name:      "chmod failure closes and removes temp file",
			setupErr:  func(f *failingTempFile) { f.chmodErr = errors.New("perm denied") },
			wantError: "chmod 600 on temp file",
		},
		{
			name:      "close failure removes temp file",
			setupErr:  func(f *failingTempFile) { f.closeErr = errors.New("close io error") },
			wantError: "close temp file",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "facts.json")
			var created *failingTempFile

			err := writeFileAtomic(target, []byte("payload"), 0o600, 0o700, func(d, pattern string) (tempFile, error) {
				realFile, err := os.CreateTemp(d, pattern)
				if err != nil {
					return nil, err
				}
				created = &failingTempFile{File: realFile}
				tc.setupErr(created)
				return created, nil
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error = %v, want substring %q", err, tc.wantError)
			}
			if created == nil || !created.closed {
				t.Fatalf("expected temp file to be closed on failure")
			}
			if _, statErr := os.Stat(created.Name()); !os.IsNotExist(statErr) {
				t.Fatalf("expected temp file %q to be removed, stat err = %v", created.Name(), statErr)
			}
			if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
				t.Fatalf("target file %q should not exist after failure", target)
			}
		})
	}
}
