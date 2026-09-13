package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

type archiveLimits struct {
	executable int64
	expanded   uint64
}

var releaseLimits = archiveLimits{executable: MaxExecutableBytes, expanded: MaxExpandedBytes}

// ArchiveExecutable inspects every entry of a release archive and returns
// only the expected root executable's bytes. Nothing is extracted and no
// member name ever reaches a filesystem path. Any unsafe path, link, device
// or other special entry, a duplicate executable, or a size beyond the bounds
// rejects the archive; sizes are enforced on what is actually read, not only
// on what a header declares.
func ArchiveExecutable(archive []byte, target Artifact) ([]byte, error) {
	if int64(len(archive)) > MaxArchiveBytes {
		return nil, fmt.Errorf("archive exceeds the %d-byte compressed limit", MaxArchiveBytes)
	}
	switch target.ArchiveFormat {
	case "tar.gz":
		return executableFromTarGz(archive, target.ExecutableName, releaseLimits)
	case "zip":
		return executableFromZip(archive, target.ExecutableName, releaseLimits)
	default:
		return nil, fmt.Errorf("unsupported archive format %q", target.ArchiveFormat)
	}
}

func executableFromTarGz(raw []byte, expected string, limits archiveLimits) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("open gzip archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var executable []byte
	var expanded uint64
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar archive: %w", err)
		}
		if !safeArchivePath(header.Name) {
			return nil, fmt.Errorf("unsafe archive path %q", header.Name)
		}
		if header.Size < 0 || uint64(header.Size) > limits.expanded-expanded {
			return nil, fmt.Errorf("archive expands beyond the %d-byte limit", limits.expanded)
		}
		expanded += uint64(header.Size)
		switch header.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
		default:
			return nil, fmt.Errorf("archive entry %q has unsafe type %q", header.Name, header.Typeflag)
		}
		if header.Name != expected {
			continue
		}
		if executable != nil {
			return nil, fmt.Errorf("archive contains the executable %q twice", expected)
		}
		if header.Size == 0 || header.Size > limits.executable {
			return nil, fmt.Errorf("executable size %d is outside 1..%d bytes", header.Size, limits.executable)
		}
		if executable, err = readBounded(tr, limits.executable); err != nil {
			return nil, fmt.Errorf("read executable %q: %w", expected, err)
		}
		if int64(len(executable)) != header.Size {
			return nil, fmt.Errorf("executable %q is truncated", expected)
		}
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("finish gzip archive: %w", err)
	}
	if executable == nil {
		return nil, fmt.Errorf("archive has no root executable %q", expected)
	}
	return executable, nil
}

func executableFromZip(raw []byte, expected string, limits archiveLimits) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("open zip archive: %w", err)
	}
	if limits.executable <= 0 {
		return nil, errors.New("executable size limit is not positive")
	}
	var executable []byte
	var expanded uint64
	for _, file := range zr.File {
		if !safeArchivePath(file.Name) {
			return nil, fmt.Errorf("unsafe archive path %q", file.Name)
		}
		if file.UncompressedSize64 > limits.expanded-expanded {
			return nil, fmt.Errorf("archive expands beyond the %d-byte limit", limits.expanded)
		}
		expanded += file.UncompressedSize64
		mode := file.Mode()
		if mode.IsDir() {
			continue
		}
		if !mode.IsRegular() {
			return nil, fmt.Errorf("archive entry %q has unsafe type %s", file.Name, mode.Type())
		}
		if file.Name != expected {
			continue
		}
		if executable != nil {
			return nil, fmt.Errorf("archive contains the executable %q twice", expected)
		}
		if file.UncompressedSize64 == 0 || file.UncompressedSize64 > uint64(limits.executable) { // #nosec G115 -- positivity checked above
			return nil, fmt.Errorf("executable size %d is outside 1..%d bytes", file.UncompressedSize64, limits.executable)
		}
		r, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("open executable %q: %w", expected, err)
		}
		executable, err = readBounded(r, limits.executable)
		closeErr := r.Close()
		if err != nil {
			return nil, fmt.Errorf("read executable %q: %w", expected, err)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close executable %q: %w", expected, closeErr)
		}
		if uint64(len(executable)) != file.UncompressedSize64 {
			return nil, fmt.Errorf("executable %q is truncated", expected)
		}
	}
	if executable == nil {
		return nil, fmt.Errorf("archive has no root executable %q", expected)
	}
	return executable, nil
}

// readBounded reads at most max bytes, and fails when there is even one more:
// the limit is enforced on what arrives, whatever a header claimed.
func readBounded(r io.Reader, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("more than the %d-byte limit", max)
	}
	return data, nil
}

// safeArchivePath is a relative slash path with no traversal, no backslash
// and no drive letter.
func safeArchivePath(name string) bool {
	if name == "" || strings.ContainsAny(name, `\:`) || path.IsAbs(name) {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return false
		}
	}
	return path.Clean(name) != "."
}
