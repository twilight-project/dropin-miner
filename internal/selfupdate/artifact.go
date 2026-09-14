package selfupdate

import "fmt"

const (
	// ProjectName is the GoReleaser project and the executable's base name.
	ProjectName = "dropin-miner"
	// ChecksumAssetName is GoReleaser's checksum.name_template.
	ChecksumAssetName = "checksums.txt"
)

// Artifact is the one release archive, and the one executable in it, for a
// platform.
type Artifact struct {
	GOOS           string
	GOARCH         string
	ArchiveName    string
	ExecutableName string
	ArchiveFormat  string // "tar.gz" or "zip"
}

// ArtifactFor is the runtime half of the release naming contract. It is not a
// template evaluator: tools/releasecheck's
// TestSelfUpdaterAssetNamesMatchGoReleaser ties every name it returns to
// .goreleaser.yaml, so a naming change there fails CI instead of stranding
// installed updaters on names that no longer exist.
func ArtifactFor(v Version, goos, goarch string) (Artifact, error) {
	if goarch != "amd64" && goarch != "arm64" {
		return Artifact{}, fmt.Errorf("no release artifact for architecture %s", goarch)
	}
	var ext, executable, format string
	switch goos {
	case "linux", "darwin":
		ext, executable, format = ".tar.gz", ProjectName, "tar.gz"
	case "windows":
		ext, executable, format = ".zip", ProjectName+".exe", "zip"
	default:
		return Artifact{}, fmt.Errorf("no release artifact for operating system %s", goos)
	}
	return Artifact{
		GOOS:           goos,
		GOARCH:         goarch,
		ArchiveName:    fmt.Sprintf("%s_%s_%s_%s%s", ProjectName, v, goos, goarch, ext),
		ExecutableName: executable,
		ArchiveFormat:  format,
	}, nil
}
