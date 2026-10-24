package update

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Beam-Network/beam-cli-public/internal/version"
)

const maxBinary = 256 << 20

func extractBundle(archivePath, destination, goos string) error {
	wanted := []string{
		version.ExecutableName(version.CLIName(), goos),
		version.ExecutableName(version.AgentName(), goos),
	}
	found := make(map[string]bool, len(wanted))
	if goos == "windows" {
		if err := extractZip(archivePath, destination, wanted, found); err != nil {
			return err
		}
	} else if err := extractTarGzip(archivePath, destination, wanted, found); err != nil {
		return err
	}
	for _, name := range wanted {
		if !found[name] {
			return fmt.Errorf("verified bundle does not contain %s", name)
		}
	}
	return nil
}

func extractTarGzip(path, destination string, wanted []string, found map[string]bool) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	compressed, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer compressed.Close()
	reader := tar.NewReader(compressed)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name, ok := exactBundleName(header.Name, wanted)
		if !ok {
			continue
		}
		if found[name] {
			return fmt.Errorf("verified bundle contains duplicate %s", name)
		}
		if header.Typeflag != tar.TypeReg || header.Size < 0 || header.Size > maxBinary {
			return fmt.Errorf("verified bundle contains invalid %s", name)
		}
		if err := writeExtracted(filepath.Join(destination, name), reader, header.Size); err != nil {
			return err
		}
		found[name] = true
	}
}

func extractZip(path, destination string, wanted []string, found map[string]bool) error {
	reader, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer reader.Close()
	for _, entry := range reader.File {
		name, ok := exactBundleName(entry.Name, wanted)
		if !ok {
			continue
		}
		if found[name] {
			return fmt.Errorf("verified bundle contains duplicate %s", name)
		}
		if entry.FileInfo().IsDir() || entry.UncompressedSize64 > maxBinary {
			return fmt.Errorf("verified bundle contains invalid %s", name)
		}
		source, err := entry.Open()
		if err != nil {
			return err
		}
		err = writeExtracted(filepath.Join(destination, name), source, int64(entry.UncompressedSize64))
		closeErr := source.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		found[name] = true
	}
	return nil
}

func exactBundleName(raw string, wanted []string) (string, bool) {
	clean := strings.TrimPrefix(filepath.ToSlash(raw), "./")
	if strings.Contains(clean, "/") {
		return "", false
	}
	for _, name := range wanted {
		if clean == name {
			return name, true
		}
	}
	return "", false
}

func writeExtracted(path string, source io.Reader, size int64) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(file, io.LimitReader(source, maxBinary+1))
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if written != size || written > maxBinary {
		return errors.New("verified bundle contains a truncated or oversized binary")
	}
	return os.Chmod(path, 0o700)
}
