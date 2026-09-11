package fsx

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestPublication(t *testing.T) {
	dir := t.TempDir()
	for _, data := range []string{"old", "replacement"} {
		if err := WriteFileAtomic(dir, "final", []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(dir, "final")) // #nosec G304 -- paths are generated within the test temporary directory.
		if err != nil || string(got) != data {
			t.Fatalf("publication: %q %v", got, err)
		}
	}
	if err := WriteFileExclusive(dir, "exclusive", []byte("winner"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileExclusive(dir, "exclusive", []byte("loser"), 0o600); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("exclusive conflict: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "exclusive")) // #nosec G304 -- paths are generated within the test temporary directory.
	if err != nil || string(got) != "winner" {
		t.Fatalf("winner overwritten: %q %v", got, err)
	}
	if err := publish(filepath.Join(dir, "missing"), filepath.Join(dir, "final"), false); err == nil {
		t.Fatal("missing source published")
	}
	got, err = os.ReadFile(filepath.Join(dir, "final")) // #nosec G304 -- paths are generated within the test temporary directory.
	if err != nil || string(got) != "replacement" {
		t.Fatalf("failed publication lost old file: %q %v", got, err)
	}
	info, err := os.Stat(filepath.Join(dir, "final"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
}

type failingFile struct {
	stagedFile
	stage   string
	failure error
}

func (f failingFile) Write(b []byte) (int, error) {
	if f.stage == "write" {
		_, _ = f.stagedFile.Write(b[:1])
		return 1, f.failure
	}
	return f.stagedFile.Write(b)
}
func (f failingFile) Sync() error {
	if f.stage == "file sync" {
		return f.failure
	}
	return f.stagedFile.Sync()
}
func (f failingFile) Close() error {
	err := f.stagedFile.Close()
	if f.stage == "close" {
		return f.failure
	}
	return err
}

func TestFailureStages(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, stage := range []string{"write", "file sync", "close", "publication", "directory sync"} {
			t.Run(stage+map[bool]string{true: "/existing", false: "/absent"}[existing], func(t *testing.T) {
				dir := t.TempDir()
				final := filepath.Join(dir, "final")
				if existing {
					if err := os.WriteFile(final, []byte("old"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				failure := errors.New("injected failure")
				ops := defaultOperations()
				create := ops.create
				ops.create = func(dir, pattern string) (stagedFile, error) {
					f, err := create(dir, pattern)
					if err != nil {
						return nil, err
					}
					return failingFile{f, stage, failure}, nil
				}
				if stage == "publication" {
					ops.publish = func(string, string, bool) error { return failure }
				}
				if stage == "directory sync" {
					ops.syncDir = func(string) error { return failure }
				}
				err := writeFile(dir, "final", []byte("complete"), 0o600, false, ops)
				var se *StageError
				if !errors.Is(err, failure) || !errors.As(err, &se) || se.Stage != stage || se.Published != (stage == "directory sync") {
					t.Fatalf("stage classification: %v", err)
				}
				got, readErr := os.ReadFile(final) // #nosec G304 -- paths are generated within the test temporary directory.
				if stage == "directory sync" {
					if readErr != nil || string(got) != "complete" {
						t.Fatalf("published bytes: %q %v", got, readErr)
					}
				} else if existing {
					if readErr != nil || string(got) != "old" {
						t.Fatalf("old bytes: %q %v", got, readErr)
					}
				} else if !errors.Is(readErr, fs.ErrNotExist) {
					t.Fatalf("partial final: %q %v", got, readErr)
				}
				temps, err := filepath.Glob(filepath.Join(dir, ".tmp-*"))
				if err != nil || len(temps) != 0 {
					t.Fatalf("staged files leaked: %v %v", temps, err)
				}
			})
		}
	}
}

func TestExclusiveConcurrentCreators(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Go(func() { results <- WriteFileExclusive(dir, "winner", []byte("complete"), 0o600) })
	}
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else if !errors.Is(err, fs.ErrExist) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("winners=%d", winners)
	}
}

func TestPublicationOrdersDurability(t *testing.T) {
	dir := t.TempDir()
	ops := defaultOperations()
	synced := false
	publication := ops.publish
	ops.publish = func(from, to string, exclusive bool) error {
		got, err := os.ReadFile(from) // #nosec G304 -- paths are generated within the test temporary directory.
		if err != nil || string(got) != "complete" {
			t.Fatalf("incomplete staged file: %q %v", got, err)
		}
		return publication(from, to, exclusive)
	}
	ops.syncDir = func(string) error { synced = true; return nil }
	if err := writeFile(dir, "final", []byte("complete"), 0o600, false, ops); err != nil {
		t.Fatal(err)
	}
	if !synced {
		t.Fatal("directory sync not called")
	}
}

func TestDirectorySyncClassification(t *testing.T) {
	dir := t.TempDir()
	err := SyncDirectory(dir)
	if runtime.GOOS == "windows" {
		if !errors.Is(err, ErrDirectorySyncUnsupported) {
			t.Fatalf("unsupported classification: %v", err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	ops := defaultOperations()
	ops.syncDir = func(string) error { return ErrDirectorySyncUnsupported }
	err = writeFile(dir, "classification", []byte("complete"), 0o600, false, ops)
	if runtime.GOOS == "windows" {
		if err != nil {
			t.Fatal(err)
		}
	} else if !errors.Is(err, ErrDirectorySyncUnsupported) {
		t.Fatalf("Unix swallowed unsupported directory sync: %v", err)
	}
}
