package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/twilight-project/dropin-miner/internal/selfupdate"
)

// goreleaserFixture is the repository's current naming contract, written
// out so a test can vary one thing at a time. It is checked against the
// real file by TestExpectedAssetsMatchesTheRepositorysOwnConfig below,
// so a drift between the two shows up as a failure here rather than as a
// fixture quietly describing a config that no longer exists.
const goreleaserFixture = `
version: 2
project_name: dropin-miner
builds:
  - main: ./cmd/dropin-miner
    binary: dropin-miner
    env: [CGO_ENABLED=0]
    goos: [linux, darwin, windows]
    goarch: [amd64, arm64]
archives:
  - formats: [tar.gz]
    name_template: "{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}"
    format_overrides:
      - goos: windows
        formats: [zip]
checksum:
  name_template: checksums.txt
release:
  draft: false
`

var wantAssets028 = []string{
	"checksums.txt",
	"dropin-miner_0.2.8_darwin_amd64.tar.gz",
	"dropin-miner_0.2.8_darwin_arm64.tar.gz",
	"dropin-miner_0.2.8_linux_amd64.tar.gz",
	"dropin-miner_0.2.8_linux_arm64.tar.gz",
	"dropin-miner_0.2.8_windows_amd64.zip",
	"dropin-miner_0.2.8_windows_arm64.zip",
}

// TestSelfUpdaterAssetNamesMatchGoReleaser is the coupling between the
// release pipeline and every installed updater. The updater computes asset
// names with a small checked-in function, not GoReleaser's template engine;
// this test derives the whole matrix from the real .goreleaser.yaml and
// requires the two to agree, so a naming, matrix, format or checksum-name
// change fails here before it can strand deployed updaters on names that no
// longer exist.
func TestSelfUpdaterAssetNamesMatchGoReleaser(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	contract, err := ParseNamingContract(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"v0.2.8", "v0.3.0", "v1.10.0"} {
		v := mustParseTag(t, tag)
		want, err := contract.ExpectedAssets(v)
		if err != nil {
			t.Fatal(err)
		}
		runtimeVersion, err := selfupdate.ParseVersion(v.String())
		if err != nil {
			t.Fatal(err)
		}
		got := []string{selfupdate.ChecksumAssetName}
		for _, goos := range contract.GOOS {
			for _, goarch := range contract.GOARCH {
				artifact, err := selfupdate.ArtifactFor(runtimeVersion, goos, goarch)
				if err != nil {
					t.Fatalf("%s: the updater has no artifact for GoReleaser's %s/%s: %v", tag, goos, goarch, err)
				}
				got = append(got, artifact.ArchiveName)
			}
		}
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s: the self-updater and .goreleaser.yaml disagree\n updater: %v\n release: %v", tag, got, want)
		}
	}
}

func expectedAssetsFor(t *testing.T, yaml, tag string) ([]string, error) {
	t.Helper()
	contract, err := ParseNamingContract([]byte(yaml))
	if err != nil {
		return nil, err
	}
	return contract.ExpectedAssets(mustParseTag(t, tag))
}

func TestExpectedAssetsExpandsTheMatrixAndTheWindowsOverride(t *testing.T) {
	got, err := expectedAssetsFor(t, goreleaserFixture, "v0.2.8")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "\n") != strings.Join(wantAssets028, "\n") {
		t.Errorf("expected assets\n got %v\nwant %v", got, wantAssets028)
	}
}

// The fixture above is only worth anything while it still describes the
// real file. This is the tie: the repository's own .goreleaser.yaml must
// produce the same names for the same version.
func TestExpectedAssetsMatchesTheRepositorysOwnConfig(t *testing.T) {
	real, err := os.ReadFile(filepath.Join("..", "..", ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	fromReal, err := expectedAssetsFor(t, string(real), "v0.2.8")
	if err != nil {
		t.Fatalf("the repository's own .goreleaser.yaml was refused: %v", err)
	}
	if strings.Join(fromReal, "\n") != strings.Join(wantAssets028, "\n") {
		t.Errorf(".goreleaser.yaml no longer names the assets this test fixture describes.\n"+
			" real config gives %v\n fixture expects    %v\n"+
			"If the naming contract changed on purpose, update goreleaserFixture, wantAssets028 "+
			"and npm/install.js together.", fromReal, wantAssets028)
	}
}

// Every case here is a way the artifact names could move somewhere this
// tool is not looking. A release check that shrugged at any of them
// would keep validating an expectation the config had already left
// behind — which is the failure this whole file exists to prevent.
func TestParseNamingContractFailsClosedOnShapesItDoesNotModel(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, wantIn string
	}{
		{
			name:   "a second build, meaning a second artifact matrix",
			yaml:   strings.Replace(goreleaserFixture, "archives:\n", "  - main: ./cmd/other\n    binary: other\n    goos: [linux]\n    goarch: [amd64]\narchives:\n", 1),
			wantIn: "builds entries",
		},
		{
			name:   "targets, which replaces the goos x goarch expansion",
			yaml:   strings.Replace(goreleaserFixture, "    goarch: [amd64, arm64]", "    goarch: [amd64, arm64]\n    targets: [linux_amd64]", 1),
			wantIn: "targets",
		},
		{
			name:   "ignore, which removes pairs from the matrix",
			yaml:   strings.Replace(goreleaserFixture, "    goarch: [amd64, arm64]", "    goarch: [amd64, arm64]\n    ignore:\n      - goos: windows\n        goarch: arm64", 1),
			wantIn: "ignore",
		},
		{
			name:   "no explicit goos, leaving goreleaser's defaults to guess at",
			yaml:   strings.Replace(goreleaserFixture, "    goos: [linux, darwin, windows]\n", "", 1),
			wantIn: "goos and goarch",
		},
		{
			name:   "no name_template, likewise",
			yaml:   strings.Replace(goreleaserFixture, `    name_template: "{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}"`, "", 1),
			wantIn: "name_template",
		},
		{
			name:   "two formats, so which extension a file gets is ambiguous",
			yaml:   strings.Replace(goreleaserFixture, "formats: [tar.gz]", "formats: [tar.gz, zip]", 1),
			wantIn: "formats",
		},
		{
			name:   "the deprecated singular format key, which this does not read",
			yaml:   strings.Replace(goreleaserFixture, "formats: [tar.gz]", "format: tar.gz", 1),
			wantIn: "formats",
		},
		{
			name:   "checksums disabled, which npm/install.js requires",
			yaml:   strings.Replace(goreleaserFixture, "  name_template: checksums.txt", "  name_template: checksums.txt\n  disable: true", 1),
			wantIn: "checksum",
		},
		{
			name:   "no project_name",
			yaml:   strings.Replace(goreleaserFixture, "project_name: dropin-miner", "", 1),
			wantIn: "project_name",
		},
	} {
		_, err := ParseNamingContract([]byte(tc.yaml))
		if err == nil {
			t.Errorf("%s: accepted, when it should have stopped the release", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantIn) {
			t.Errorf("%s: error does not mention %q: %v", tc.name, tc.wantIn, err)
		}
	}
}

// text/template errors on a field the data does not have, and that is
// load-bearing: .Arm and .Amd64 resolve to values only goreleaser knows,
// so a template reaching for one must fail here rather than produce a
// plausible wrong name that no release will ever carry.
func TestExpectedAssetsRefusesATemplateItCannotResolve(t *testing.T) {
	for _, tc := range []struct{ name, tmpl string }{
		{"a goreleaser field this tool cannot predict", `{{ .ProjectName }}_{{ .Version }}_{{ .Os }}{{ .Arm }}`},
		{"a template function", `{{ .ProjectName }}_{{ title .Os }}`},
		{"a syntactically broken template", `{{ .ProjectName`},
	} {
		yaml := strings.Replace(goreleaserFixture,
			`name_template: "{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}"`,
			`name_template: "`+tc.tmpl+`"`, 1)
		if got, err := expectedAssetsFor(t, yaml, "v0.2.8"); err == nil {
			t.Errorf("%s: accepted, producing %v", tc.name, got)
		}
	}
}

func TestExpectedAssetsRefusesAFormatWithNoStatedExtension(t *testing.T) {
	yaml := strings.Replace(goreleaserFixture, "formats: [tar.gz]", "formats: [tar.xz]", 1)
	if got, err := expectedAssetsFor(t, yaml, "v0.2.8"); err == nil {
		t.Errorf("an unmodelled archive format was accepted, producing %v", got)
	}
}

func TestDiffAssetsNamesBothDirections(t *testing.T) {
	want := []string{"a", "b", "c"}
	d := DiffAssets(want, []string{"a", "c", "z"})
	if d.Complete() {
		t.Fatal("a release missing an asset and carrying an unexpected one was reported complete")
	}
	if strings.Join(d.Missing, ",") != "b" {
		t.Errorf("Missing = %v, want [b]", d.Missing)
	}
	if strings.Join(d.Unexpected, ",") != "z" {
		t.Errorf("Unexpected = %v, want [z]", d.Unexpected)
	}
	err := d.Error()
	if err == nil {
		t.Fatal("Error() was nil for an incomplete diff")
	}
	for _, want := range []string{"missing", "b", "unexpected", "z"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the diff error does not mention %q: %v", want, err)
		}
	}
	if !DiffAssets(want, []string{"c", "b", "a"}).Complete() {
		t.Error("a complete release in a different order was reported incomplete")
	}
}

// An extra asset is a failure, not a shrug. It means the release config
// grew an artifact kind this check cannot name, and a verifier that
// ignores files it does not understand is not verifying the release.
func TestDiffAssetsTreatsAnUnexpectedAssetAsIncomplete(t *testing.T) {
	d := DiffAssets(wantAssets028, append(append([]string(nil), wantAssets028...), "dropin-miner_0.2.8_linux_amd64.deb"))
	if d.Complete() {
		t.Error("an unexpected asset was accepted; the set comparison is one-directional")
	}
}
