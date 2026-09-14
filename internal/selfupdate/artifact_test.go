package selfupdate

import "testing"

func TestArtifactForReleaseMatrix(t *testing.T) {
	v, _ := ParseVersion("v0.3.0")
	for _, tc := range []struct {
		goos, arch, want string
	}{
		{"linux", "amd64", "dropin-miner_0.3.0_linux_amd64.tar.gz"},
		{"linux", "arm64", "dropin-miner_0.3.0_linux_arm64.tar.gz"},
		{"darwin", "amd64", "dropin-miner_0.3.0_darwin_amd64.tar.gz"},
		{"darwin", "arm64", "dropin-miner_0.3.0_darwin_arm64.tar.gz"},
		{"windows", "amd64", "dropin-miner_0.3.0_windows_amd64.zip"},
		{"windows", "arm64", "dropin-miner_0.3.0_windows_arm64.zip"},
	} {
		a, err := ArtifactFor(v, tc.goos, tc.arch)
		if err != nil {
			t.Fatalf("%s/%s: %v", tc.goos, tc.arch, err)
		}
		if a.ArchiveName != tc.want {
			t.Errorf("%s/%s: got %q, want %q", tc.goos, tc.arch, a.ArchiveName, tc.want)
		}
	}
}

func TestArtifactForRejectsUnsupportedTargets(t *testing.T) {
	v, _ := ParseVersion("0.3.0")
	for _, tc := range [][2]string{{"freebsd", "amd64"}, {"linux", "386"}} {
		if _, err := ArtifactFor(v, tc[0], tc[1]); err == nil {
			t.Errorf("ArtifactFor(%s/%s) succeeded", tc[0], tc[1])
		}
	}
}
