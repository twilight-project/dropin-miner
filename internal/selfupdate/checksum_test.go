package selfupdate

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

func sumsFor(t *testing.T, body []byte, name string) string {
	t.Helper()
	return fmt.Sprintf("%x  %s\n", sha256.Sum256(body), name)
}

func verify(target Artifact, archive []byte, sums string) error {
	return SHA256Verifier{}.Verify(context.Background(), map[string][]byte{
		target.ArchiveName: archive,
		ChecksumAssetName:  []byte(sums),
	}, target)
}

func TestChecksumVerifierAcceptsTheExactAsset(t *testing.T) {
	archive := []byte("archive")
	target := Artifact{ArchiveName: "dropin-miner_0.3.0_linux_amd64.tar.gz"}
	other := fmt.Sprintf("%x  dropin-miner_0.3.0_darwin_arm64.tar.gz\n", sha256.Sum256([]byte("other")))
	upper := strings.ToUpper(fmt.Sprintf("%x", sha256.Sum256(archive))) + "\t" + target.ArchiveName + "\r\n"
	for name, sums := range map[string]string{
		"goreleaser form":       other + sumsFor(t, archive, target.ArchiveName),
		"uppercase, tab, CRLF":  "\n" + upper + "\n",
		"no trailing newline":   strings.TrimSuffix(sumsFor(t, archive, target.ArchiveName), "\n"),
		"blank lines around it": "\n\n" + sumsFor(t, archive, target.ArchiveName) + "\n\n",
	} {
		if err := verify(target, archive, sums); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestChecksumVerifierRejectsMalformedDuplicateMissingAndMismatch(t *testing.T) {
	target := Artifact{ArchiveName: "asset.tar.gz"}
	archive := []byte("archive")
	good := sumsFor(t, archive, target.ArchiveName)
	zero := strings.Repeat("0", 64)
	for name, sums := range map[string]string{
		"duplicate target entry":    good + good,
		"duplicate other entry":     good + zero + "  x\n" + zero + "  x\n",
		"not hex":                   strings.Repeat("z", 64) + "  asset.tar.gz\n",
		"short digest":              zero[:63] + "  asset.tar.gz\n",
		"three fields":              zero + "  asset.tar.gz extra\n",
		"traversal name":            zero + "  ../asset.tar.gz\n" + good,
		"slash name":                zero + "  dir/asset.tar.gz\n" + good,
		"backslash name":            zero + `  dir\asset.tar.gz` + "\n" + good,
		"drive name":                zero + "  C:asset.tar.gz\n" + good,
		"a malformed line anywhere": good + "garbage\n",
		"missing target":            zero + "  other.tar.gz\n",
		"a longer name only":        zero + "  asset.tar.gz.sig\n",
		"mismatch":                  zero + "  asset.tar.gz\n",
		"empty":                     "",
	} {
		if err := verify(target, archive, sums); err == nil {
			t.Errorf("%s: accepted %q", name, sums)
		}
	}
}

func TestChecksumVerifierDeclaresBoundedAssets(t *testing.T) {
	target := Artifact{ArchiveName: "dropin-miner_0.3.0_linux_amd64.tar.gz"}
	got, err := SHA256Verifier{}.RequiredAssets(ReleaseInfo{}, target)
	if err != nil {
		t.Fatal(err)
	}
	want := []AssetRequirement{{target.ArchiveName, MaxArchiveBytes}, {ChecksumAssetName, MaxChecksumBytes}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("RequiredAssets = %v, want %v", got, want)
	}
	if err := (SHA256Verifier{}).Verify(context.Background(), map[string][]byte{target.ArchiveName: nil}, target); err == nil {
		t.Error("a verifier that did not receive checksums.txt must fail")
	}
}

func TestFrozenBounds(t *testing.T) {
	for name, pair := range map[string][2]int64{
		"release json":   {MaxReleaseJSONBytes, 1 << 20},
		"checksums":      {MaxChecksumBytes, 64 << 10},
		"archive":        {MaxArchiveBytes, 64 << 20},
		"executable":     {MaxExecutableBytes, 64 << 20},
		"expanded":       {int64(MaxExpandedBytes), 128 << 20},
		"request":        {int64(RequestTimeout), int64(120_000_000_000)},
		"operation":      {int64(OperationTimeout), int64(180_000_000_000)},
		"candidate exec": {int64(CandidateTimeout), int64(5_000_000_000)},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s bound is %d, frozen at %d: raising it is an architecture review, not an edit", name, pair[0], pair[1])
		}
	}
}
