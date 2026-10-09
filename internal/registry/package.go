package registry

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Inspection struct {
	Package          string   `json:"package"`
	Version          string   `json:"version"`
	Entrypoint       string   `json:"entrypoint"`
	Files            int      `json:"files"`
	ManifestChecksum string   `json:"manifest_checksum"`
	Permissions      []string `json:"permissions"`
	Placements       []string `json:"placements"`
}

type Artifact struct {
	Inspection
	Path             string         `json:"path"`
	SizeBytes        int64          `json:"size_bytes"`
	ArtifactChecksum string         `json:"artifact_checksum"`
	Manifest         map[string]any `json:"manifest"`
}

func Inspect(directory string) (Inspection, map[string]any, []string, error) {
	root, err := filepath.Abs(directory)
	if err != nil {
		return Inspection{}, nil, nil, err
	}
	data, err := os.ReadFile(filepath.Join(root, "beam-action.json"))
	if err != nil {
		return Inspection{}, nil, nil, fmt.Errorf("read beam-action.json: %w", err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Inspection{}, nil, nil, fmt.Errorf("parse beam-action.json: %w", err)
	}
	if err := ValidateManifest(manifest); err != nil {
		return Inspection{}, nil, nil, err
	}
	name := manifest["name"].(string)
	version := manifest["version"].(string)
	entrypoint, err := projectEntrypoint(root, manifest)
	if err != nil {
		return Inspection{}, nil, nil, err
	}
	cleanEntry := filepath.Clean(filepath.FromSlash(entrypoint))
	if filepath.IsAbs(cleanEntry) || cleanEntry == ".." || strings.HasPrefix(cleanEntry, ".."+string(filepath.Separator)) {
		return Inspection{}, nil, nil, fmt.Errorf("manifest entrypoint must stay inside the package directory")
	}
	if info, err := os.Stat(filepath.Join(root, cleanEntry)); err != nil || !info.Mode().IsRegular() {
		return Inspection{}, nil, nil, fmt.Errorf("manifest entrypoint does not exist: %s", entrypoint)
	}
	placements := nestedStrings(manifest, "runtime", "placements")
	files, err := packageFiles(root, filepath.ToSlash(cleanEntry))
	if err != nil {
		return Inspection{}, nil, nil, err
	}
	canonical, err := CanonicalManifest(manifest)
	if err != nil {
		return Inspection{}, nil, nil, err
	}
	sum := sha256.Sum256(canonical)
	inspection := Inspection{
		Package:          name,
		Version:          version,
		Entrypoint:       filepath.ToSlash(entrypoint),
		Files:            len(files),
		ManifestChecksum: "sha256:" + hex.EncodeToString(sum[:]),
		Permissions:      stringsValue(manifest["permissions"]),
		Placements:       placements,
	}
	return inspection, manifest, files, nil
}

func Pack(directory, output string) (Artifact, error) {
	inspection, manifest, files, err := Inspect(directory)
	if err != nil {
		return Artifact{}, err
	}
	root, err := filepath.Abs(directory)
	if err != nil {
		return Artifact{}, err
	}
	out, err := filepath.Abs(output)
	if err != nil {
		return Artifact{}, err
	}
	filtered := files[:0]
	for _, relative := range files {
		candidate := filepath.Join(root, filepath.FromSlash(relative))
		if filepath.Clean(candidate) != filepath.Clean(out) {
			filtered = append(filtered, relative)
		}
	}
	files = filtered
	inspection.Files = len(files)
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return Artifact{}, fmt.Errorf("create output directory: %w", err)
	}
	file, err := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return Artifact{}, fmt.Errorf("create package: %w", err)
	}
	writeErr := writeArchive(file, root, files)
	closeErr := file.Close()
	if writeErr != nil {
		return Artifact{}, writeErr
	}
	if closeErr != nil {
		return Artifact{}, closeErr
	}
	data, err := os.ReadFile(out)
	if err != nil {
		return Artifact{}, err
	}
	sum := sha256.Sum256(data)
	return Artifact{
		Inspection:       inspection,
		Path:             out,
		SizeBytes:        int64(len(data)),
		ArtifactChecksum: "sha256:" + hex.EncodeToString(sum[:]),
		Manifest:         manifest,
	}, nil
}

func writeArchive(destination io.Writer, root string, files []string) error {
	gz := gzip.NewWriter(destination)
	gz.Header.ModTime = time.Unix(0, 0)
	gz.Header.OS = 255
	tw := tar.NewWriter(gz)
	for _, relative := range files {
		full := filepath.Join(root, filepath.FromSlash(relative))
		info, err := os.Stat(full)
		if err != nil {
			return fmt.Errorf("stat %s: %w", relative, err)
		}
		mode := int64(0o644)
		if info.Mode().Perm()&0o111 != 0 {
			mode = 0o755
		}
		header := &tar.Header{
			Name:       relative,
			Mode:       mode,
			Size:       info.Size(),
			ModTime:    time.Unix(0, 0),
			AccessTime: time.Unix(0, 0),
			ChangeTime: time.Unix(0, 0),
			Typeflag:   tar.TypeReg,
			Format:     tar.FormatPAX,
		}
		if err := tw.WriteHeader(header); err != nil {
			return fmt.Errorf("archive %s: %w", relative, err)
		}
		source, err := os.Open(full)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, source)
		closeErr := source.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func nestedStrings(value map[string]any, objectKey, valueKey string) []string {
	object, _ := value[objectKey].(map[string]any)
	return stringsValue(object[valueKey])
}

func stringsValue(value any) []string {
	raw, _ := value.([]any)
	result := make([]string, 0, len(raw))
	for _, candidate := range raw {
		if text, ok := candidate.(string); ok {
			result = append(result, text)
		}
	}
	return result
}
