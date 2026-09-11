package fsx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestMoveRequiresBothDirectoryBarriers(t *testing.T) {
	for _, failAt := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("failure-%d", failAt), func(t *testing.T) {
			fromDir, toDir := t.TempDir(), t.TempDir()
			from, to := filepath.Join(fromDir, "record"), filepath.Join(toDir, "record")
			if err := os.WriteFile(from, []byte("complete evidence"), 0o600); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("sync failed")
			var dirs []string
			err := moveFile(from, to, movePublication, func(dir string) error {
				dirs = append(dirs, dir)
				if len(dirs) == failAt {
					return failure
				}
				return nil
			})
			if failAt == 0 {
				if err != nil || len(dirs) != 2 || dirs[0] != toDir || dirs[1] != fromDir {
					t.Fatalf("barriers %v error %v", dirs, err)
				}
			} else {
				var stage *StageError
				if !errors.Is(err, failure) || !errors.As(err, &stage) || !stage.Published {
					t.Fatalf("uncertainty missing: %v", err)
				}
			}
			if _, err := os.Stat(to); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(from); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("old location: %v", err)
			}
		})
	}
}

func TestDurableRemovalLeavesNoActiveName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "record.json")
	if err := WriteFileAtomic(dir, "record.json", []byte("evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveFileDurable(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("active name remains: %v", err)
	}
}
