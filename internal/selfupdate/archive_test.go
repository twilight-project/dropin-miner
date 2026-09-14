package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"hash/crc32"
	"io/fs"
	"strings"
	"testing"
)

type archiveEntry struct {
	name     string
	body     string
	typeflag byte
	mode     fs.FileMode
}

func makeTarGz(t *testing.T, entries ...archiveEntry) []byte {
	t.Helper()
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typeflag := e.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Mode: 0o755, Size: int64(len(e.body)), Typeflag: typeflag}
		switch typeflag {
		case tar.TypeSymlink, tar.TypeLink:
			h.Size, h.Linkname = 0, "target"
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo, tar.TypeDir:
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func makeZip(t *testing.T, entries ...archiveEntry) []byte {
	t.Helper()
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.mode != 0 {
			h.SetMode(e.mode)
		} else {
			h.SetMode(0o755)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

var (
	tarTarget = Artifact{ArchiveFormat: "tar.gz", ExecutableName: "dropin-miner"}
	zipTarget = Artifact{ArchiveFormat: "zip", ExecutableName: "dropin-miner.exe"}
)

func TestArchiveExecutableReadsOnlyTheRootExecutable(t *testing.T) {
	for name, tc := range map[string]struct {
		raw []byte
		art Artifact
	}{
		"tar": {makeTarGz(t,
			archiveEntry{name: "docs", typeflag: tar.TypeDir},
			archiveEntry{name: "README.md", body: "readme"},
			archiveEntry{name: "docs/dropin-miner", body: "not the root one"},
			archiveEntry{name: "dropin-miner", body: "binary"}), tarTarget},
		"zip": {makeZip(t,
			archiveEntry{name: "README.md", body: "readme"},
			archiveEntry{name: "dropin-miner.exe", body: "binary"}), zipTarget},
	} {
		got, err := ArchiveExecutable(tc.raw, tc.art)
		if err != nil || string(got) != "binary" {
			t.Errorf("%s: got %q, %v", name, got, err)
		}
	}
}

func TestArchiveExecutableRejectsUnsafeEntries(t *testing.T) {
	root := archiveEntry{name: "dropin-miner", body: "x"}
	rootExe := archiveEntry{name: "dropin-miner.exe", body: "x"}
	// Every unsafe archive also holds a valid root executable, so the only
	// reason left to refuse it is the entry under test.
	for name, tc := range map[string]struct {
		raw  []byte
		art  Artifact
		want string
	}{
		"tar traversal":          {makeTarGz(t, archiveEntry{name: "../x", body: "x"}, root), tarTarget, "unsafe archive path"},
		"tar absolute":           {makeTarGz(t, archiveEntry{name: "/x", body: "x"}, root), tarTarget, "unsafe archive path"},
		"tar backslash":          {makeTarGz(t, archiveEntry{name: `x\..\y`, body: "x"}, root), tarTarget, "unsafe archive path"},
		"tar drive":              {makeTarGz(t, archiveEntry{name: "C:y", body: "x"}, root), tarTarget, "unsafe archive path"},
		"tar symlink":            {makeTarGz(t, archiveEntry{name: "link", typeflag: tar.TypeSymlink}, root), tarTarget, "unsafe type"},
		"tar hardlink":           {makeTarGz(t, archiveEntry{name: "link", typeflag: tar.TypeLink}, root), tarTarget, "unsafe type"},
		"tar executable link":    {makeTarGz(t, archiveEntry{name: "dropin-miner", typeflag: tar.TypeSymlink}), tarTarget, "unsafe type"},
		"tar character device":   {makeTarGz(t, archiveEntry{name: "tty", typeflag: tar.TypeChar}, root), tarTarget, "unsafe type"},
		"tar fifo":               {makeTarGz(t, archiveEntry{name: "pipe", typeflag: tar.TypeFifo}, root), tarTarget, "unsafe type"},
		"tar no root executable": {makeTarGz(t, archiveEntry{name: "bin/dropin-miner", body: "x"}), tarTarget, "no root executable"},
		"zip traversal":          {makeZip(t, archiveEntry{name: "../x", body: "x"}, rootExe), zipTarget, "unsafe archive path"},
		"zip backslash":          {makeZip(t, archiveEntry{name: `..\x`, body: "x"}, rootExe), zipTarget, "unsafe archive path"},
		"zip drive":              {makeZip(t, archiveEntry{name: "C:x", body: "x"}, rootExe), zipTarget, "unsafe archive path"},
		"zip symlink":            {makeZip(t, archiveEntry{name: "link", body: "target", mode: fs.ModeSymlink | 0o777}, rootExe), zipTarget, "unsafe type"},
		"zip empty executable":   {makeZip(t, archiveEntry{name: "dropin-miner.exe"}), zipTarget, "outside 1.."},
		"not an archive":         {[]byte("plain bytes"), tarTarget, "gzip"},
	} {
		_, err := ArchiveExecutable(tc.raw, tc.art)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want a refusal naming %q, got %v", name, tc.want, err)
		}
	}
}

func TestArchiveExecutableRejectsADuplicateExecutable(t *testing.T) {
	if _, err := ArchiveExecutable(makeTarGz(t,
		archiveEntry{name: "dropin-miner", body: "one"},
		archiveEntry{name: "dropin-miner", body: "two"}), tarTarget); err == nil {
		t.Error("tar: duplicate executable accepted")
	}
	if _, err := ArchiveExecutable(makeZip(t,
		archiveEntry{name: "dropin-miner.exe", body: "one"},
		archiveEntry{name: "dropin-miner.exe", body: "two"}), zipTarget); err == nil {
		t.Error("zip: duplicate executable accepted")
	}
}

func TestArchiveExecutableBounds(t *testing.T) {
	small := archiveLimits{executable: 16, expanded: 32}
	if _, err := executableFromTarGz(makeTarGz(t, archiveEntry{name: "dropin-miner", body: strings.Repeat("x", 17)}), "dropin-miner", small); err == nil {
		t.Error("an executable over its limit was accepted")
	}
	if _, err := executableFromTarGz(makeTarGz(t, archiveEntry{name: "dropin-miner", body: strings.Repeat("x", 16)}), "dropin-miner", small); err != nil {
		t.Errorf("an executable at its limit must be accepted: %v", err)
	}
	bomb := makeTarGz(t,
		archiveEntry{name: "a", body: strings.Repeat("x", 16)},
		archiveEntry{name: "b", body: strings.Repeat("x", 16)},
		archiveEntry{name: "c", body: "x"},
		archiveEntry{name: "dropin-miner", body: "x"})
	if _, err := executableFromTarGz(bomb, "dropin-miner", small); err == nil {
		t.Error("tar: total declared expansion over its limit was accepted")
	}
	zipBomb := makeZip(t,
		archiveEntry{name: "a", body: strings.Repeat("x", 16)},
		archiveEntry{name: "b", body: strings.Repeat("x", 17)},
		archiveEntry{name: "dropin-miner.exe", body: "x"})
	if _, err := executableFromZip(zipBomb, "dropin-miner.exe", small); err == nil {
		t.Error("zip: total declared expansion over its limit was accepted")
	}
	if _, err := ArchiveExecutable(make([]byte, MaxArchiveBytes+1), tarTarget); err == nil {
		t.Error("a compressed archive over its limit was accepted")
	}
}

// A zip entry whose header understates its size: the bound holds on what is
// read, whatever the header says.
func TestArchiveExecutableEnforcesTheBoundOnWhatIsRead(t *testing.T) {
	body := []byte(strings.Repeat("x", 40))
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	w, err := zw.CreateRaw(&zip.FileHeader{
		Name: "dropin-miner.exe", Method: zip.Store,
		CRC32: crc32.ChecksumIEEE(body), CompressedSize64: uint64(len(body)), UncompressedSize64: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := executableFromZip(out.Bytes(), "dropin-miner.exe", archiveLimits{executable: 16, expanded: 1 << 20}); err == nil {
		t.Errorf("an entry that reads past its declared size was accepted: %d bytes", len(got))
	}
}

func TestReadBoundedUsesLimitPlusOne(t *testing.T) {
	if b, err := readBounded(strings.NewReader("12345"), 5); err != nil || string(b) != "12345" {
		t.Errorf("exactly the limit must be accepted: %q %v", b, err)
	}
	if _, err := readBounded(strings.NewReader("123456"), 5); err == nil {
		t.Error("one byte over the limit must be refused")
	}
}
