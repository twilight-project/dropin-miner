package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureDir = "../../internal/trajectory/testdata/projects"

// snapshot hashes every file under dir with its mode and modification time,
// so a read that changed anything — content, permissions or a timestamp a
// rewrite would move — shows up. A directory's modification time is kept on
// purpose: it is what moves when a file is created and removed again, which
// the set of entries alone would not show.
//
// The metadata comes from os.Lstat, not from the walk's DirEntry. On Windows
// a DirEntry's Info is the parent directory's index entry, which NTFS updates
// lazily: the first snapshot saw a directory's stale time from the checkout,
// the scan opened that directory to read it, the entry caught up, and the
// second snapshot differed though nothing had been written. That failed this
// test on windows-11-arm and passed on windows-latest, which is what a lazy
// update looks like. Counting the right observable means asking the file,
// not the directory that lists it.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		sum := ""
		if !d.IsDir() {
			data, err := os.ReadFile(p) // #nosec G304,G122 -- the test's own fixture tree
			if err != nil {
				return err
			}
			h := sha256.Sum256(data)
			sum = hex.EncodeToString(h[:])
		}
		out[p] = info.Mode().String() + " " + info.ModTime().String() + " " + sum
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestScanPrintsCountsAndNoContentAndChangesNothing(t *testing.T) {
	before := snapshot(t, fixtureDir)
	if len(before) < 10 {
		t.Fatalf("the fixture tree has %d entries; this test is not looking at it", len(before))
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"scan", fixtureDir}, &stdout, &stderr); code != exitOK {
		t.Fatalf("scan exited %d: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"transcripts                               7", "searches (tool calls)                     4", "unknown_entry_type quantum-state"} {
		if !strings.Contains(out, want) {
			t.Errorf("scan output lacks %q:\n%s", want, out)
		}
	}
	// Every string a person or a model wrote in the fixtures carries this
	// marker; so do the planted account id, the attachment and the sidecar's
	// description.
	if strings.Contains(out, "ZEBRA-") || strings.Contains(stderr.String(), "ZEBRA-") {
		t.Fatalf("scan printed content:\n%s%s", out, stderr.String())
	}
	for _, leak := range []string{"req_synthetic", "toolu_", "00000000-0000-4000", "example.test", "/synthetic/"} {
		if strings.Contains(out, leak) {
			t.Errorf("scan printed %q: it prints counts, not ids, urls or paths", leak)
		}
	}
	after := snapshot(t, fixtureDir)
	if len(after) != len(before) {
		t.Fatalf("scan changed the directory it read: %d entries before, %d after", len(before), len(after))
	}
	for p, was := range before {
		if after[p] != was {
			// Both values are fixture metadata — a mode, a time, a hash of
			// synthetic bytes — and naming the field that moved is what
			// turns a recurrence of the failure above into evidence.
			t.Errorf("scan changed %s\n  before: %s\n  after:  %s", p, was, after[p])
		}
	}
}

// TestMeasurePrintsCountsAndCreatesNothing. measure takes no output path
// because it has no output to place: the report is the whole of it. The
// snapshot is what proves it — a command that reads a corpus and reports on
// it must leave the corpus exactly as it found it, and the one before this
// caught a real failure on Windows.
func TestMeasurePrintsCountsAndCreatesNothing(t *testing.T) {
	const measureFixtureDir = "../../internal/trajectory/testdata/measure"
	before := snapshot(t, measureFixtureDir)
	if len(before) < 4 {
		t.Fatalf("the fixture tree has %d entries; this test is not looking at it", len(before))
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"measure", measureFixtureDir}, &stdout, &stderr); code != exitOK {
		t.Fatalf("measure exited %d: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"what a turn holds that nobody consented to share",
		"search_result_provider_content", "turns that would produce a record         2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("measure output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ZEBRA-") || strings.Contains(stderr.String(), "ZEBRA-") {
		t.Fatalf("measure printed content:\n%s%s", out, stderr.String())
	}
	for _, leak := range []string{"req_synthetic", "toolu_", "00000000-0000-4000", "example.test", "/synthetic/"} {
		if strings.Contains(out, leak) {
			t.Errorf("measure printed %q: it prints counts, not ids, urls or paths", leak)
		}
	}
	after := snapshot(t, measureFixtureDir)
	if len(after) != len(before) {
		t.Fatalf("measure changed the directory it read: %d entries before, %d after", len(before), len(after))
	}
	for p, was := range before {
		if after[p] != was {
			t.Errorf("measure changed %s\n  before: %s\n  after:  %s", p, was, after[p])
		}
	}
}

func TestScanRefusesBadUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"scan"}, {"scan", "a", "b"}, {"emit", "a"}, {"scan", "-upload", "a"}, {"measure"}, {"measure", "a", "b"}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != exitUsage {
			t.Errorf("run(%v) = %d, want %d", args, code, exitUsage)
		}
		if stdout.Len() != 0 {
			t.Errorf("run(%v) wrote to stdout on a usage error: %q", args, stdout.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"scan", filepath.Join(t.TempDir(), "absent")}, &stdout, &stderr); code != exitError {
		t.Errorf("scan of a missing directory = %d, want %d", code, exitError)
	}
}
