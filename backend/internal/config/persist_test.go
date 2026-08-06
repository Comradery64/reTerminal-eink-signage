package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewFileWriterFailsOnUnwritableDir(t *testing.T) {
	if _, err := NewFileWriter(filepath.Join("/nonexistent-dir-xyz-abc", "config.yaml")); err == nil {
		t.Fatal("want an error for a directory that doesn't exist — a bad path must fail at construction, not at the first admin save")
	}
}

func TestFileWriterWriteAtomicPreservesModeAndReplacesContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("old content"), 0o640); err != nil {
		t.Fatal(err)
	}

	fw, err := NewFileWriter(path)
	if err != nil {
		t.Fatalf("NewFileWriter: %v", err)
	}
	if err := fw.WriteAtomic([]byte("new content")); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new content" {
		t.Fatalf("content = %q, want %q", got, "new content")
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v, want 0640 (preserved from the original file)", fi.Mode().Perm())
	}

	// No temp-file debris should be left behind in the directory.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.yaml" {
		t.Fatalf("directory should contain exactly config.yaml, got %v", entries)
	}
}

func TestFileWriterWriteAtomicDefaultsModeWhenFileDidNotExist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	fw, err := NewFileWriter(path)
	if err != nil {
		t.Fatalf("NewFileWriter: %v", err)
	}
	if err := fw.WriteAtomic([]byte("x")); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600 for a brand-new file (it holds password hashes)", fi.Mode().Perm())
	}
}

// TestFileWriterWriteAtomicLeavesOriginalIntactOnRenameFailure exercises the atomicity guarantee
// from a reader's point of view: if the final rename can't complete, the target must be found
// exactly as it was — never truncated or half-written. Simulated here by pointing the writer at a
// path that is actually an existing directory: every OS this broker targets refuses to rename a
// plain file onto an existing directory, so the rename step fails deterministically without any
// filesystem-fault injection.
func TestFileWriterWriteAtomicLeavesOriginalIntactOnRenameFailure(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.yaml")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}

	fw, err := NewFileWriter(target)
	if err != nil {
		t.Fatalf("NewFileWriter: %v", err)
	}
	if err := fw.WriteAtomic([]byte("new content")); err == nil {
		t.Fatal("want an error when the rename can't complete")
	}

	fi, err := os.Stat(target)
	if err != nil || !fi.IsDir() {
		t.Fatalf("target must be untouched (still the original directory): stat err=%v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "config.yaml" {
			t.Errorf("leftover temp file not cleaned up after a failed write: %s", e.Name())
		}
	}
}
