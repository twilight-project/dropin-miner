package selfupdate

import "testing"

func TestParseVersionNormalizesOptionalVPrefix(t *testing.T) {
	for _, raw := range []string{"0.3.0", "v0.3.0"} {
		got, err := ParseVersion(raw)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", raw, err)
		}
		if got.String() != "0.3.0" || got.Tag() != "v0.3.0" {
			t.Fatalf("ParseVersion(%q) = %q / %q", raw, got, got.Tag())
		}
	}
}

func TestParseVersionRefusesMalformedVersions(t *testing.T) {
	for _, raw := range []string{
		"", "dev", "1", "1.2", "1.2.3.4", "vv1.2.3", "1.02.3",
		"1.2.3-rc.1", "1.2.3+build", " 1.2.3", "1.2.3 ", "-1.2.3",
	} {
		if got, err := ParseVersion(raw); err == nil {
			t.Errorf("ParseVersion(%q) = %v, want error", raw, got)
		}
	}
}

func TestVersionCompare(t *testing.T) {
	a, _ := ParseVersion("0.2.8")
	b, _ := ParseVersion("0.3.0")
	if a.Compare(b) >= 0 || b.Compare(a) <= 0 || a.Compare(a) != 0 {
		t.Fatal("version comparison is not ordered")
	}
}

func TestParseReleaseTagRequiresCanonicalVForm(t *testing.T) {
	if _, err := ParseReleaseTag("v0.3.0"); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"0.3.0", "V0.3.0", "v0.3.0-rc.1"} {
		if _, err := ParseReleaseTag(raw); err == nil {
			t.Errorf("ParseReleaseTag(%q) succeeded", raw)
		}
	}
}
