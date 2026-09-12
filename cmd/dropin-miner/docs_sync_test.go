package main

// Documentation that has to agree with other documentation, checked the
// way code is.
//
// npm/README.md is not a summary of README.md, or a copy someone remembers
// to refresh — it IS README.md, plus the one section that only makes sense
// on npm. It had drifted by five releases before anyone noticed, because
// nothing was watching: the page npm serves described a binary that no
// longer existed, while the repository's own front page was current. A
// prose rule ("keep them in sync") is the thing that failed. Byte equality
// is the thing that cannot.

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// npmWrapperSuffix is the ONLY text npm/README.md may carry beyond the
// top-level README, appended verbatim after it. It describes the npm
// package itself — the download, the checksum check, the two escape
// hatches — which is true of the wrapper and of nothing else in the
// repository, so it has no place on the front page.
//
// Held here as a constant, not read out of npm/README.md, on purpose: a
// suffix derived from the file under test would make this assertion
// unfalsifiable. Changing the wrapper section means changing this
// constant and the file together, which is the review the section is
// owed.
const npmWrapperSuffix = "\n## The npm wrapper\n\n" +
	"This package downloads the release binary for your platform on install, " +
	"verifies it against the release checksums, and forwards every argument to it. " +
	"The package version is the release tag it fetches.\n\n" +
	"- `DROPIN_MINER_BINARY=/path` use a binary already on the machine\n" +
	"- `DROPIN_MINER_SKIP_DOWNLOAD=1` install the wrapper without fetching\n"

func TestNPMReadmeMirrorsRootREADME(t *testing.T) {
	root := moduleRoot(t)

	top, err := os.ReadFile(filepath.Join(root, "README.md")) // #nosec G304 -- a fixed path under this module's own root
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	npm, err := os.ReadFile(filepath.Join(root, "npm", "README.md")) // #nosec G304 -- a fixed path under this module's own root
	if err != nil {
		t.Fatalf("read npm/README.md: %v", err)
	}

	want := make([]byte, 0, len(top)+len(npmWrapperSuffix))
	want = append(want, top...)
	want = append(want, npmWrapperSuffix...)

	if bytes.Equal(npm, want) {
		return
	}

	// Say where, not just that. A diff of two 16 KB files in a test log is
	// unreadable; the first differing byte, with its line number and both
	// sides' context, is what a person actually needs.
	line, col, gotCtx, wantCtx := firstDifference(npm, want)
	t.Fatalf("npm/README.md is not README.md + the wrapper suffix.\n"+
		"first difference at line %d, column %d:\n"+
		"  npm/README.md:  %q\n"+
		"  expected:       %q\n"+
		"(npm/README.md is %d bytes, expected %d)\n"+
		"Fix by regenerating it: the top-level README verbatim, then npmWrapperSuffix.",
		line, col, gotCtx, wantCtx, len(npm), len(want))
}

// firstDifference locates the first byte at which got and want diverge and
// returns its 1-based line and column plus a bounded excerpt of each side
// from that point.
func firstDifference(got, want []byte) (line, col int, gotCtx, wantCtx string) {
	i := 0
	for i < len(got) && i < len(want) && got[i] == want[i] {
		i++
	}
	line, col = 1, 1
	for _, b := range got[:i] {
		if b == '\n' {
			line++
			col = 1
			continue
		}
		col++
	}
	return line, col, excerptFrom(got, i), excerptFrom(want, i)
}

func excerptFrom(b []byte, i int) string {
	const window = 60
	if i >= len(b) {
		return "<end of file>"
	}
	end := i + window
	if end > len(b) {
		end = len(b)
	}
	return string(b[i:end])
}
