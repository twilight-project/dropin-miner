package selfupdate

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

type fakeReleaseSource struct {
	release       ReleaseInfo
	assets        map[string][]byte
	requested     *Version
	releaseCalls  int
	downloadCalls int
}

func (s *fakeReleaseSource) Release(_ context.Context, requested *Version) (ReleaseInfo, error) {
	s.releaseCalls++
	s.requested = requested
	return s.release, nil
}

func (s *fakeReleaseSource) DownloadAssets(context.Context, ReleaseInfo, []AssetRequirement) (map[string][]byte, error) {
	s.downloadCalls++
	return s.assets, nil
}

// markerRunner reports as its version whatever text the file it runs holds,
// so a test's "binary" can be any bytes.
func markerRunner() CommandRunner {
	return runnerFunc(func(_ context.Context, path string, _ []string, _ []string) ([]byte, []byte, error) {
		body, err := os.ReadFile(path) // #nosec G304 -- test-owned path
		if err != nil {
			return nil, nil, err
		}
		return []byte("dropin-miner " + strings.TrimSpace(string(body)) + "\n"), nil, nil
	})
}

// release030 is a published v0.3.0 whose linux/amd64 executable says body.
func release030(t *testing.T, body string) *fakeReleaseSource {
	t.Helper()
	v, _ := ParseVersion("0.3.0")
	artifact, _ := ArtifactFor(v, "linux", "amd64")
	archive := makeTarGz(t, archiveEntry{name: "dropin-miner", body: body})
	sum := sha256.Sum256(archive)
	return &fakeReleaseSource{
		release: ReleaseInfo{Version: v, Assets: map[string]ReleaseAsset{
			artifact.ArchiveName: {Name: artifact.ArchiveName, Size: int64(len(archive))},
			ChecksumAssetName:    {Name: ChecksumAssetName, Size: 100},
		}},
		assets: map[string][]byte{
			artifact.ArchiveName: archive,
			ChecksumAssetName:    []byte(fmt.Sprintf("%x  %s\n", sum, artifact.ArchiveName)),
		},
	}
}

func installedBinary(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "bin", "dropin-miner")
	if err := os.MkdirAll(filepath.Dir(exe), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("0.2.8\n"), 0o700); err != nil { // #nosec G306 -- executable fixture
		t.Fatal(err)
	}
	return exe
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func TestPrepareStagesAVerifiedCandidateAndReplacesNothing(t *testing.T) {
	exe := installedBinary(t)
	src := release030(t, "0.3.0\n")
	p, err := Updater{Source: src, Runner: markerRunner(), GOOS: "linux", GOARCH: "amd64"}.Prepare(context.Background(), exe, "0.2.8", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Discard()
	if p.NoChange || p.From.String() != "0.2.8" || p.To.String() != "0.3.0" || p.Candidate == "" {
		t.Fatalf("prepared = %+v", p)
	}
	if b, _ := os.ReadFile(exe); string(b) != "0.2.8\n" { // #nosec G304 -- test path
		t.Error("Prepare must not change the installed binary")
	}
	if b, _ := os.ReadFile(p.Candidate); string(b) != "0.3.0\n" { // #nosec G304 -- test path
		t.Error("the candidate must hold the archive's executable")
	}
	names := dirNames(t, filepath.Dir(exe))
	if len(names) != 2 || names[1] != "dropin-miner" || !strings.HasPrefix(names[0], ".dropin-miner.candidate-") {
		t.Errorf("after Prepare the directory holds %v: only the installed binary and the candidate, no previous and no rename", names)
	}
	p.Discard()
	if names := dirNames(t, filepath.Dir(exe)); len(names) != 1 {
		t.Errorf("Discard must remove only the candidate: %v", names)
	}
}

func TestPrepareRefusesANonReleaseBuildBeforeTheNetwork(t *testing.T) {
	for _, build := range []string{"dev", "dev (0123456789ab+dirty)", "v0.3.0", "0.3.0-rc.1", "0.03.0", ""} {
		src := release030(t, "0.3.0\n")
		_, err := Updater{Source: src, Runner: markerRunner(), GOOS: "linux", GOARCH: "amd64"}.Prepare(context.Background(), installedBinary(t), build, nil)
		if KindOf(err) != KindNotRelease || src.releaseCalls != 0 {
			t.Errorf("build %q: want not_release before any request, got %v (%d requests)", build, err, src.releaseCalls)
		}
	}
}

func TestPrepareEqualIsANoOpAndOlderIsRefusedBeforeDownloading(t *testing.T) {
	src := release030(t, "0.3.0\n")
	p, err := Updater{Source: src, Runner: markerRunner(), GOOS: "linux", GOARCH: "amd64"}.Prepare(context.Background(), installedBinary(t), "0.3.0", nil)
	if err != nil || !p.NoChange || p.Candidate != "" || src.downloadCalls != 0 {
		t.Errorf("equal version: %+v %v downloads=%d", p, err, src.downloadCalls)
	}
	src = release030(t, "0.3.0\n")
	exact, _ := ParseVersion("0.3.0")
	_, err = Updater{Source: src, Runner: markerRunner(), GOOS: "linux", GOARCH: "amd64"}.Prepare(context.Background(), installedBinary(t), "0.3.1", &exact)
	if KindOf(err) != KindDowngrade || src.downloadCalls != 0 {
		t.Errorf("a lower explicit version: want downgrade before download, got %v downloads=%d", err, src.downloadCalls)
	}
	if src.requested == nil || *src.requested != exact {
		t.Error("-version selects exactly that release")
	}
}

func TestPrepareRejectsWhatDoesNotVerify(t *testing.T) {
	cases := map[string]func(s *fakeReleaseSource){
		"tampered archive": func(s *fakeReleaseSource) {
			for name, b := range s.assets {
				if strings.HasSuffix(name, ".tar.gz") {
					s.assets[name] = append(append([]byte(nil), b...), 0)
				}
			}
		},
		"missing checksum entry": func(s *fakeReleaseSource) {
			s.assets[ChecksumAssetName] = []byte(strings.Repeat("0", 64) + "  other\n")
		},
	}
	for name, mutate := range cases {
		exe := installedBinary(t)
		src := release030(t, "0.3.0\n")
		mutate(src)
		_, err := Updater{Source: src, Runner: markerRunner(), GOOS: "linux", GOARCH: "amd64"}.Prepare(context.Background(), exe, "0.2.8", nil)
		if KindOf(err) != KindReleaseInvalid {
			t.Errorf("%s: want release_invalid, got %v", name, err)
		}
		if names := dirNames(t, filepath.Dir(exe)); len(names) != 1 {
			t.Errorf("%s: nothing may be staged from an unverified release: %v", name, names)
		}
	}
}

func TestPrepareRemovesACandidateThatReportsTheWrongVersion(t *testing.T) {
	for name, body := range map[string]string{"wrong version": "0.2.9\n", "not a version": "garbage\n"} {
		exe := installedBinary(t)
		src := release030(t, body)
		_, err := Updater{Source: src, Runner: markerRunner(), GOOS: "linux", GOARCH: "amd64"}.Prepare(context.Background(), exe, "0.2.8", nil)
		if KindOf(err) != KindCandidateInvalid {
			t.Errorf("%s: want candidate_invalid, got %v", name, err)
		}
		if names := dirNames(t, filepath.Dir(exe)); len(names) != 1 {
			t.Errorf("%s: a rejected candidate must be removed: %v", name, names)
		}
	}
}

func TestPrepareRefusesAnUnsupportedPlatform(t *testing.T) {
	src := release030(t, "0.3.0\n")
	_, err := Updater{Source: src, Runner: markerRunner(), GOOS: "plan9", GOARCH: "amd64"}.Prepare(context.Background(), installedBinary(t), "0.2.8", nil)
	if KindOf(err) != KindUnsupported || src.downloadCalls != 0 {
		t.Errorf("unsupported platform: %v downloads=%d", err, src.downloadCalls)
	}
}

func TestPrepareStagesBesideTheResolvedBinaryNotTheLink(t *testing.T) {
	exe := installedBinary(t)
	link := filepath.Join(t.TempDir(), "dropin-miner")
	if err := os.Symlink(exe, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	p, err := Updater{Source: release030(t, "0.3.0\n"), Runner: markerRunner(), GOOS: "linux", GOARCH: "amd64"}.Prepare(context.Background(), link, "0.2.8", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Discard()
	realDir, _ := filepath.EvalSymlinks(filepath.Dir(exe))
	if filepath.Dir(p.Candidate) != realDir || p.Executable != filepath.Join(realDir, "dropin-miner") {
		t.Errorf("candidate %s / executable %s: both belong beside the resolved %s", p.Candidate, p.Executable, realDir)
	}
	if names := dirNames(t, filepath.Dir(link)); len(names) != 1 {
		t.Errorf("nothing may be staged beside the launcher link: %v", names)
	}
}
