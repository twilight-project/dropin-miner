package main

// #85: what the installers leave in the temporary directory when the
// checksum does not match.
//
// The region under test is the download-and-verify slice of each script —
// from the temporary directory's creation to the binary's installation.
// These cases run that slice's **real bytes**, cut out of the real file by
// anchors that are themselves real code, with a preamble supplying the few
// variables the surrounding script would have set. Running the whole
// script is not an option: its release lookup is a hardcoded
// api.github.com URL, and a test that reaches a real host is not one this
// repo runs. Cutting by anchor is why `sliceOf` fails loudly rather than
// skipping when an anchor moves — a slice that silently stopped matching
// would test nothing and say nothing.
//
// Locating the temporary directory is where the first version of this file
// went wrong, and the correction is the interesting part. Asserting "a
// scratch directory I pointed the process at is empty afterwards" passes
// for two different reasons: because the cleanup worked, and because the
// temporary directory was never created there at all. macOS's BSD mktemp
// ignores TMPDIR when called with no template — which `mktemp -d` is — so
// on the macOS runner that assertion was vacuous, and a mutation that
// deleted install.sh's EXIT trap outright left it green.
//
// So each case now proves where the directory was before it claims it is
// gone. install.sh's `mktemp -d` goes through a shim first on PATH that
// records the real one's answer, and the case asserts that exact path no
// longer exists. install.ps1's GetTempPath does honor TEMP/TMP, so its
// preamble asserts that before the slice runs and prints a marker the case
// requires in the output — otherwise a preamble that threw would look like
// the checksum refusal it is expecting. Both also assert the stub served
// the archive, so a slice that never ran cannot pass by leaving nothing
// behind.
//
// install.sh runs on POSIX runners, install.ps1 on Windows ones, the same
// split installer_bridge_test.go already uses.

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// sliceOf returns the part of scripts/<name> between two anchors, both of
// which must appear exactly once. The anchors are lines of the script's own
// code, so an edit that moves the region fails this rather than quietly
// testing a different one.
func sliceOf(t *testing.T, script, from, to string) string {
	t.Helper()
	b, err := os.ReadFile(scriptPath(t, script)) // #nosec G304 -- this repo's own script
	if err != nil {
		t.Fatal(err)
	}
	// .gitattributes checks *.ps1 out with CRLF, so on the Windows runner
	// the same anchor that matches here would not match there. Normalizing
	// before the search is the fix; PowerShell and sh both run the LF form
	// the anchors are written against.
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	if n := strings.Count(s, from); n != 1 {
		t.Fatalf("scripts/%s: start anchor %q appears %d times, want once", script, from, n)
	}
	if n := strings.Count(s, to); n != 1 {
		t.Fatalf("scripts/%s: end anchor %q appears %d times, want once", script, to, n)
	}
	start := strings.Index(s, from) + len(from)
	end := strings.Index(s, to)
	if end <= start {
		t.Fatalf("scripts/%s: the end anchor precedes the start anchor", script)
	}
	return s[start:end]
}

// releaseStub serves one release's two files over loopback: the archive and
// checksums.txt. sum decides whether the checksum matches.
type releaseStub struct {
	srv     *httptest.Server
	name    string
	archive []byte

	mu       sync.Mutex
	archives int // requests for the archive itself
}

func (r *releaseStub) archiveRequests() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.archives
}

func newReleaseStub(t *testing.T, name string, archive []byte, correctSum bool) *releaseStub {
	t.Helper()
	sum := sha256.Sum256(archive)
	digest := hex.EncodeToString(sum[:])
	if !correctSum {
		// A hash of the right shape and the wrong value: the script must
		// reject it on the comparison, not on a parse.
		digest = strings.Repeat("0", 64)
	}
	checksums := fmt.Sprintf("%s  %s\n", digest, name)

	r := &releaseStub{name: name, archive: archive}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/checksums.txt"):
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(checksums))
		case strings.HasSuffix(req.URL.Path, "/"+name):
			r.mu.Lock()
			r.archives++
			r.mu.Unlock()
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, req)
		}
	})
	r.srv = httptest.NewServer(mux)
	t.Cleanup(r.srv.Close)
	return r
}

// tarGz is a release archive holding one executable member.
func tarGz(t *testing.T, member string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: member, Mode: 0o755, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	for _, c := range []interface{ Close() error }{tw, gz} {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

// zipOf is the same for install.ps1's .zip.
func zipOf(t *testing.T, member string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(member)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// emptyDirOrFail fails naming everything still inside dir.
func emptyDirOrFail(t *testing.T, dir, what string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		return
	}
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
		if e.IsDir() {
			inner, ierr := os.ReadDir(filepath.Join(dir, e.Name()))
			if ierr == nil {
				for _, f := range inner {
					left = append(left, filepath.Join(e.Name(), f.Name()))
				}
			}
		}
	}
	t.Fatalf("%s: the temporary directory survived, holding %v", what, left)
}

// ── install.sh ──────────────────────────────────────────────────────────

// mktempShim puts a `mktemp` first on PATH that records what the real one
// answered. `mktemp -d` with no template is exactly the call whose
// directory the test needs to name afterwards, and it is the one call BSD
// mktemp does not route through TMPDIR — so the answer is taken from the
// call itself rather than predicted from the environment.
func mktempShim(t *testing.T, dir string) (shimDir, logPath string) {
	t.Helper()
	real, err := exec.LookPath("mktemp")
	if err != nil {
		t.Fatalf("mktemp is required to test install.sh's download slice: %v", err)
	}
	shimDir = filepath.Join(dir, "shim")
	if err := os.MkdirAll(shimDir, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath = filepath.Join(dir, "mktemp.log")
	shim := "#!/bin/sh\nd=$(" + shellQuote(real) + " \"$@\") || exit $?\nprintf '%s\\n' \"$d\" >> " + shellQuote(logPath) + "\nprintf '%s\\n' \"$d\"\n"
	if err := os.WriteFile(filepath.Join(shimDir, "mktemp"), []byte(shim), 0o700); err != nil { // #nosec G306 -- an executable shim in this test's own sandbox
		t.Fatal(err)
	}
	return shimDir, logPath
}

// runInstallShSlice writes the preamble plus the real download-and-verify
// slice to a file, runs it under /bin/sh with the mktemp shim first on
// PATH, and returns the exit code, the output, and every directory mktemp
// handed the slice.
func runInstallShSlice(t *testing.T, stub *releaseStub, binDir string) (int, string, []string) {
	t.Helper()
	body := sliceOf(t, "install.sh",
		"elif [ -n \"$TAG\" ]; then\n",
		"\nelse\n  say \"No release is tagged yet")

	preamble := strings.Join([]string{
		"set -eu",
		`say(){ printf '\n==> %s\n' "$*"; }`,
		`die(){ printf '\nERROR: %s\n' "$*" >&2; exit 1; }`,
		"REPO=" + shellQuote(stub.srv.URL+"/repo"),
		`TAG="v9.9.9"`,
		"OS=" + shellQuote(runtime.GOOS),
		"ARCH=" + shellQuote(runtime.GOARCH),
		"BIN_DIR=" + shellQuote(binDir),
		"",
	}, "\n")

	dir := t.TempDir()
	shimDir, logPath := mktempShim(t, dir)
	path := filepath.Join(dir, "slice.sh")
	if err := os.WriteFile(path, []byte(preamble+body+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", path) // #nosec G204 -- a file this test just wrote
	cmd.Env = append(os.Environ(), "PATH="+shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	code, out := runScript(t, cmd)

	var made []string
	if b, err := os.ReadFile(logPath); err == nil { // #nosec G304 -- this test's own log
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if line != "" {
				made = append(made, line)
			}
		}
	}
	return code, out, made
}

// goneOrFail names the one temporary directory the slice was given and
// fails if anything of it is left. This is the assertion the first version
// of this file did not make, and the reason a mutation that removed
// install.sh's EXIT trap outright went unnoticed.
func goneOrFail(t *testing.T, made []string, what string) {
	t.Helper()
	if len(made) != 1 {
		t.Fatalf("%s: the slice called mktemp %d time(s), want exactly 1 (%v)", what, len(made), made)
	}
	if !lexists(made[0]) {
		return
	}
	entries, err := os.ReadDir(made[0])
	if err != nil {
		t.Fatalf("%s: %s survived: %v", what, made[0], err)
	}
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	t.Fatalf("%s: %s survived, holding %v", what, made[0], left)
}

func TestInstallShRemovesTheDownloadWhenTheChecksumFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is POSIX-only; install.ps1 has its own case")
	}
	name := fmt.Sprintf("dropin-miner_9.9.9_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	stub := newReleaseStub(t, name, tarGz(t, "dropin-miner", []byte("#!/bin/sh\nexit 0\n")), false)
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")

	code, out, made := runInstallShSlice(t, stub, binDir)
	if code == 0 {
		t.Fatalf("a wrong checksum exited 0\n%s", out)
	}
	if !strings.Contains(out, "checksum FAILED") {
		t.Fatalf("the refusal did not name the checksum:\n%s", out)
	}
	if stub.archiveRequests() != 1 {
		t.Fatalf("the stub served the archive %d time(s); the slice did not run as expected\n%s", stub.archiveRequests(), out)
	}
	if lexists(filepath.Join(binDir, "dropin-miner")) {
		t.Fatalf("a binary that failed its checksum was installed:\n%s", out)
	}
	goneOrFail(t, made, "install.sh, wrong checksum")
}

// The same slice on the success path, so a "fix" that always threw could
// not pass the case above.
func TestInstallShRemovesTheDownloadOnTheSuccessPathToo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is POSIX-only; install.ps1 has its own case")
	}
	name := fmt.Sprintf("dropin-miner_9.9.9_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	stub := newReleaseStub(t, name, tarGz(t, "dropin-miner", []byte("#!/bin/sh\nexit 0\n")), true)
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")

	code, out, made := runInstallShSlice(t, stub, binDir)
	if code != 0 {
		t.Fatalf("a correct checksum exited %d\n%s", code, out)
	}
	if !lexists(filepath.Join(binDir, "dropin-miner")) {
		t.Fatalf("the verified binary was not installed:\n%s", out)
	}
	goneOrFail(t, made, "install.sh, correct checksum")
}

// ── install.ps1 ─────────────────────────────────────────────────────────

// tempRootMarker prefixes the temporary directory the slice is about to
// use, as GetTempPath itself reports it. The case requires the line in the
// output, because the wrong-checksum case expects a non-zero exit and would
// otherwise read a preamble that failed as the refusal it wanted.
//
// Judging the path is Go's job, not the preamble's. The first version had
// PowerShell compare the two strings and throw, and both Windows runners
// went red on it: GitHub's TEMP is the 8.3 short form
// (C:\Users\RUNNER~1\...), which is what Go's t.TempDir() hands back and
// what Resolve-Path leaves alone, while .NET's GetTempPath returns the long
// form (C:\Users\runneradmin\...). One directory, two spellings, and a
// string comparison that called them different places.
const tempRootMarker = "TEMPROOT="

func runInstallPs1Slice(t *testing.T, stub *releaseStub, binDir, tmpRoot string) (int, string) {
	t.Helper()
	body := sliceOf(t, "install.ps1",
		"  $sums = $release.assets | Where-Object { $_.name -eq \"checksums.txt\" }\n",
		"\n  $exe = Join-Path $BinDir \"dropin-miner.exe\"")

	base := stub.srv.URL + "/repo/releases/download/v9.9.9"
	preamble := strings.Join([]string{
		`$ErrorActionPreference = "Stop"`,
		`$env:TEMP = ` + psLiteral(tmpRoot),
		`$env:TMP = ` + psLiteral(tmpRoot),
		// Reported, not assumed: the slice's own GetTempPath call is what
		// decides where the temporary directory lands, so the case has no
		// business asserting the scratch directory is empty until it has
		// seen that the two are the same place.
		`Write-Host (` + psLiteral(tempRootMarker) + ` + [System.IO.Path]::GetTempPath())`,
		`$tag = "v9.9.9"`,
		`$name = ` + psLiteral(stub.name),
		`$BinDir = ` + psLiteral(binDir),
		`$asset = [pscustomobject]@{ browser_download_url = ` + psLiteral(base+"/"+stub.name) + ` }`,
		`$sums  = [pscustomobject]@{ browser_download_url = ` + psLiteral(base+"/checksums.txt") + ` }`,
		"",
	}, "\r\n")

	path := filepath.Join(t.TempDir(), "slice.ps1")
	if err := os.WriteFile(path, []byte(preamble+body+"\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(powershell(t), "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", path) // #nosec G204 -- a file this test just wrote
	cmd.Env = installerEnv(filepath.Dir(binDir), map[string]string{"TEMP": tmpRoot, "TMP": tmpRoot})
	return runScript(t, cmd)
}

// psLiteral is a PowerShell single-quoted string: the only escape inside
// one is a doubled quote, and nothing in it is expanded.
func psLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// sameDirOnDisk is whether two spellings name one directory. Windows
// offers several — 8.3 short names, case, a trailing separator — and
// EvalSymlinks resolves each to the final path the filesystem knows, which
// is the only comparison that holds on a GitHub runner. rendered_form.go's
// samePath deliberately does not ask the filesystem, because it compares
// paths inside a host's config file that may not exist; here both
// directories do exist, and 8.3 is exactly what has to collapse.
func sameDirOnDisk(a, b string) bool {
	resolve := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Clean(r)
		}
		return filepath.Clean(p)
	}
	return strings.EqualFold(resolve(a), resolve(b))
}

// ps1SliceRan is what must hold before "the scratch directory is empty"
// means anything: the slice reported where its temporary directory would
// go, that place is the scratch directory, and the stub served the
// archive.
func ps1SliceRan(t *testing.T, stub *releaseStub, out, tmpRoot string) {
	t.Helper()
	i := strings.Index(out, tempRootMarker)
	if i < 0 {
		t.Fatalf("the slice never reported its temporary directory:\n%s", out)
	}
	reported := strings.TrimSpace(out[i+len(tempRootMarker):])
	if nl := strings.IndexAny(reported, "\r\n"); nl >= 0 {
		reported = reported[:nl]
	}
	if !sameDirOnDisk(reported, tmpRoot) {
		t.Fatalf("GetTempPath is %q, not the scratch directory %q; the emptiness check below would prove nothing", reported, tmpRoot)
	}
	if stub.archiveRequests() != 1 {
		t.Fatalf("the stub served the archive %d time(s); the slice did not run as expected\n%s", stub.archiveRequests(), out)
	}
}

func TestInstallPs1RemovesTheDownloadWhenTheChecksumFails(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("install.ps1 runs on the Windows runner")
	}
	name := "dropin-miner_9.9.9_windows_amd64.zip"
	stub := newReleaseStub(t, name, zipOf(t, "dropin-miner.exe", []byte("MZ stub")), false)
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	tmpRoot := filepath.Join(root, "tmp")
	if err := os.MkdirAll(tmpRoot, 0o700); err != nil {
		t.Fatal(err)
	}

	code, out := runInstallPs1Slice(t, stub, binDir, tmpRoot)
	ps1SliceRan(t, stub, out, tmpRoot)
	if code == 0 {
		t.Fatalf("a wrong checksum exited 0\n%s", out)
	}
	if !strings.Contains(out, "checksum FAILED") {
		t.Fatalf("the refusal did not name the checksum:\n%s", out)
	}
	if lexists(filepath.Join(binDir, "dropin-miner.exe")) {
		t.Fatalf("a binary that failed its checksum was installed:\n%s", out)
	}
	// This is #85 exactly: the unverified archive used to stay in %TEMP%,
	// the one file the script had just called untrustworthy.
	emptyDirOrFail(t, tmpRoot, "install.ps1, wrong checksum")
}

func TestInstallPs1RemovesTheDownloadOnTheSuccessPathToo(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("install.ps1 runs on the Windows runner")
	}
	name := "dropin-miner_9.9.9_windows_amd64.zip"
	stub := newReleaseStub(t, name, zipOf(t, "dropin-miner.exe", []byte("MZ stub")), true)
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	tmpRoot := filepath.Join(root, "tmp")
	if err := os.MkdirAll(tmpRoot, 0o700); err != nil {
		t.Fatal(err)
	}

	code, out := runInstallPs1Slice(t, stub, binDir, tmpRoot)
	ps1SliceRan(t, stub, out, tmpRoot)
	if code != 0 {
		t.Fatalf("a correct checksum exited %d\n%s", code, out)
	}
	if !lexists(filepath.Join(binDir, "dropin-miner.exe")) {
		t.Fatalf("the verified binary was not installed:\n%s", out)
	}
	emptyDirOrFail(t, tmpRoot, "install.ps1, correct checksum")
}
