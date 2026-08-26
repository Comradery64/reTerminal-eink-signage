package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// FileWriter is the atomic-write mechanism behind the "file" config-persistence backend (see
// internal/configstore, which wraps this as a configstore.Store). It lives here rather than in
// configstore because "durably write the YAML this broker loads its config from" is a property of
// config itself — a future caller that just wants to persist a validated Config to disk shouldn't
// need to pull in configstore's Kubernetes-adjacent Store abstraction to do it.
type FileWriter struct {
	path string
}

// NewFileWriter probes path's parent directory for writability — create and remove a throwaway
// temp file — so a bad path (missing directory, read-only mount) fails at construction time, i.e.
// at broker startup, rather than silently at the first admin save six weeks later.
func NewFileWriter(path string) (*FileWriter, error) {
	dir := filepath.Dir(path)
	probe, err := os.CreateTemp(dir, ".configstore-probe-*")
	if err != nil {
		return nil, fmt.Errorf("config file target %q is not writable: %w", path, err)
	}
	name := probe.Name()
	probe.Close()
	os.Remove(name)
	return &FileWriter{path: path}, nil
}

// Path returns the target file path this writer durably replaces.
func (f *FileWriter) Path() string { return f.path }

// WriteAtomic durably replaces the file at f.path with data, so a reader never observes a
// truncated or half-written file even across a crash. Sequence: write a temp file in the SAME
// directory as the target (rename across filesystems is not atomic — same-directory is what makes
// the rename below atomic), fsync it, close it, rename it over the target, then open and fsync the
// parent directory too — the rename is itself a directory-entry update, and without fsyncing the
// directory a crash between rename() and the next fsync can lose the rename on some filesystems.
func (f *FileWriter) WriteAtomic(data []byte) error {
	dir := filepath.Dir(f.path)

	// Preserve the existing file's mode if there is one, else default to 0600 — this file holds
	// password hashes and, in a deployment that doesn't use ${ENV} refs, other secrets.
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(f.path); err == nil {
		mode = fi.Mode().Perm()
	}

	tmp, err := os.CreateTemp(dir, ".configstore-tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	renamed := false
	defer func() {
		// Any failure before the rename lands must not leave debris behind — a half-written temp
		// left after every failed save would eventually fill the directory with junk.
		if !renamed {
			os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("fsync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, f.path); err != nil {
		return fmt.Errorf("rename into place: %w", err)
	}
	renamed = true

	dirF, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open dir for fsync: %w", err)
	}
	defer dirF.Close()
	if err := dirF.Sync(); err != nil {
		return fmt.Errorf("fsync dir: %w", err)
	}
	return nil
}
