package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// AssetRequirement is one asset a verifier needs, and the bound under which
// it may be fetched. A verifier states every remote input before anything is
// downloaded, so a later signature or provenance verifier can ask for more
// assets without changing discovery or download.
type AssetRequirement struct {
	Name     string
	MaxBytes int64
}

// ReleaseVerifier decides whether a release's assets may be trusted.
type ReleaseVerifier interface {
	RequiredAssets(release ReleaseInfo, target Artifact) ([]AssetRequirement, error)
	Verify(ctx context.Context, assets map[string][]byte, target Artifact) error
}

// SHA256Verifier checks the archive against the release's checksums.txt.
// It proves the bytes are the ones the release published, not who published
// them; signing is not part of this release.
type SHA256Verifier struct{}

func (SHA256Verifier) RequiredAssets(_ ReleaseInfo, target Artifact) ([]AssetRequirement, error) {
	return []AssetRequirement{
		{Name: target.ArchiveName, MaxBytes: MaxArchiveBytes},
		{Name: ChecksumAssetName, MaxBytes: MaxChecksumBytes},
	}, nil
}

func (SHA256Verifier) Verify(ctx context.Context, assets map[string][]byte, target Artifact) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	archive, ok := assets[target.ArchiveName]
	if !ok {
		return fmt.Errorf("verifier did not receive %q", target.ArchiveName)
	}
	sums, ok := assets[ChecksumAssetName]
	if !ok {
		return fmt.Errorf("verifier did not receive %q", ChecksumAssetName)
	}
	want, err := checksumFor(sums, target.ArchiveName)
	if err != nil {
		return err
	}
	if sha256.Sum256(archive) != want {
		return fmt.Errorf("SHA-256 of %q does not match checksums.txt", target.ArchiveName)
	}
	return nil
}

// checksumFor parses checksums.txt conservatively: every non-empty line is a
// 64-digit hex SHA-256, whitespace, and one safe basename. Any other line, a
// name listed twice, or a missing target rejects the whole file. Names are
// only compared, never joined to a local path.
func checksumFor(raw []byte, target string) ([sha256.Size]byte, error) {
	var zero [sha256.Size]byte
	entries := make(map[string][sha256.Size]byte)
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != sha256.Size*2 || !safeAssetName(fields[1]) {
			return zero, fmt.Errorf("checksums.txt line %d is malformed", i+1)
		}
		decoded, err := hex.DecodeString(fields[0])
		if err != nil {
			return zero, fmt.Errorf("checksums.txt line %d is not a SHA-256", i+1)
		}
		if _, dup := entries[fields[1]]; dup {
			return zero, fmt.Errorf("checksums.txt lists %q more than once", fields[1])
		}
		var sum [sha256.Size]byte
		copy(sum[:], decoded)
		entries[fields[1]] = sum
	}
	want, ok := entries[target]
	if !ok {
		return zero, errors.New("checksums.txt has no entry for " + target)
	}
	return want, nil
}

// safeAssetName is a basename with no separator, no traversal and no drive.
func safeAssetName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\:`)
}
