package command

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Beam-Network/beam-cli-public/internal/auth"
	"github.com/Beam-Network/beam-cli-public/internal/config"
	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/registry"
)

func registrySearch(ctx context.Context, args []string, cfg config.Config, store auth.Store, renderer output.Renderer) error {
	if len(args) > 1 {
		return usage("registry search accepts at most one query")
	}
	query := ""
	if len(args) == 1 {
		query = args[0]
	}
	client, err := registryReadClient(cfg, store)
	if err != nil {
		return err
	}
	result, err := client.Search(ctx, query)
	if err != nil {
		return mapRegistryError(err)
	}
	return renderer.Result(result, func(w io.Writer) error {
		rows := make([][]string, 0, len(result.Packages))
		for _, item := range result.Packages {
			rows = append(rows, []string{
				fmt.Sprint(firstMapValue(item, "packageName", "package_name", "name")),
				fmt.Sprint(firstMapValue(item, "displayName", "display_name")),
				fmt.Sprint(item["description"]),
			})
		}
		return renderer.Table(w, output.Table{
			Headers: []string{"PACKAGE", "NAME", "DESCRIPTION"},
			Rows:    rows,
			Style:   output.RowStyle(-1),
		})
	})
}

func registryShow(ctx context.Context, args []string, cfg config.Config, store auth.Store, renderer output.Renderer) error {
	if len(args) != 1 {
		return usage("registry show requires one package")
	}
	client, err := registryReadClient(cfg, store)
	if err != nil {
		return err
	}
	result, err := client.Package(ctx, args[0])
	if err != nil {
		return mapRegistryError(err)
	}
	return renderer.Result(result, func(w io.Writer) error { return writeIndentedJSON(w, result) })
}

func registryDownload(ctx context.Context, args []string, cfg config.Config, store auth.Store, renderer output.Renderer) error {
	if err := validateOptions(args, map[string]bool{"--out": true}, nil); err != nil {
		return err
	}
	positionals, err := positional(args)
	if err != nil || len(positionals) != 1 {
		return usage("registry download requires PACKAGE@VERSION")
	}
	packageName, version, err := packageVersionParts(positionals[0])
	if err != nil {
		return usage(err.Error())
	}
	out, found, err := flag(args, "--out")
	if err != nil {
		return err
	}
	if !found {
		out = strings.ReplaceAll(strings.TrimPrefix(packageName, "@"), "/", "-") + "-" + version + ".tgz"
	}
	absolute, err := filepath.Abs(out)
	if err != nil {
		return usage("registry download output path is invalid")
	}
	client, err := registryReadClient(cfg, store)
	if err != nil {
		return err
	}
	data, digest, err := client.Artifact(ctx, packageName, version)
	if err != nil {
		return mapRegistryError(err)
	}
	sum := sha256.Sum256(data)
	checksum := "sha256:" + hex.EncodeToString(sum[:])
	if digest != "" && !strings.EqualFold(strings.TrimSpace(digest), checksum) {
		return cliError(ExitOperationFailed, "Registry artifact checksum does not match its Digest header.", "The artifact was not written.", nil)
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(absolute, data, 0o644); err != nil {
		return err
	}
	result := map[string]any{"package": packageName, "version": version, "path": absolute, "bytes": len(data), "checksum": checksum}
	return renderer.Result(result, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "Downloaded %s@%s to %s\nChecksum: %s\n", packageName, version, absolute, checksum)
		return err
	})
}

func registryVerify(args []string, renderer output.Renderer) error {
	if len(args) != 1 {
		return usage("registry verify requires one archive")
	}
	absolute, err := existingAbsoluteFile(args[0])
	if err != nil {
		return usage("registry archive must be an existing regular file")
	}
	data, err := os.ReadFile(absolute)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	checksum := "sha256:" + hex.EncodeToString(sum[:])
	file, err := os.Open(absolute)
	if err != nil {
		return err
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return cliError(ExitUsage, "Registry archive is not valid gzip.", "Download or pack the artifact again.", err)
	}
	defer gzipReader.Close()
	tape := tar.NewReader(gzipReader)
	files := 0
	var manifest map[string]any
	var project []byte
	archived := make(map[string]bool)
	for {
		header, err := tape.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return cliError(ExitUsage, "Registry archive is not valid tar.", "Download or pack the artifact again.", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		files++
		name := filepath.ToSlash(header.Name)
		if archived[name] {
			return cliError(ExitUsage, "Registry archive contains duplicate files.", "Pack the Action again.", nil)
		}
		archived[name] = true
		if name == "beam-action.json" {
			if err := json.NewDecoder(io.LimitReader(tape, 1<<20)).Decode(&manifest); err != nil {
				return cliError(ExitUsage, "Archive manifest is invalid JSON.", "Pack the Action again.", err)
			}
		} else if name == "beam-project.json" {
			project, err = io.ReadAll(io.LimitReader(tape, 1<<20))
			if err != nil {
				return cliError(ExitUsage, "Archive project settings are invalid.", "Pack the Action again.", err)
			}
		}
	}
	if manifest == nil {
		return cliError(ExitUsage, "Archive does not contain beam-action.json.", "Pack the Action with `beam action pack`.", nil)
	}
	if err := registry.ValidateManifest(manifest); err != nil {
		return cliError(ExitUsage, "Archive manifest is invalid.", "Fix beam-action.json and pack the Action again.", err)
	}
	entrypoint, err := registry.ArchivedEntrypoint(manifest, project)
	if err != nil {
		return cliError(ExitUsage, "Archive project settings are invalid.", "Fix beam-project.json and pack the Action again.", err)
	}
	if !archived[filepath.ToSlash(entrypoint)] {
		return cliError(ExitUsage, "Archive entrypoint is missing.", "Pack the Action again.", nil)
	}
	canonical, err := registry.CanonicalManifest(manifest)
	if err != nil {
		return err
	}
	manifestSum := sha256.Sum256(canonical)
	result := map[string]any{"valid": true, "path": absolute, "files": files, "checksum": checksum, "manifest_checksum": "sha256:" + hex.EncodeToString(manifestSum[:]), "manifest": manifest}
	return renderer.Result(result, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "Valid Beam Action archive\nFiles: %d\nChecksum: %s\n", files, checksum)
		return err
	})
}

func registryReadClient(cfg config.Config, store auth.Store) (registry.Client, error) {
	tokens, err := optionalRegistrySession(cfg, store)
	if err != nil {
		return registry.Client{}, cliError(ExitConfig, "Could not read Beam credentials.", "Run `beam doctor`.", err)
	}
	return registry.Client{BaseURL: cfg.RegistryURL, Tokens: tokens}, nil
}

func packageVersionParts(value string) (string, string, error) {
	position := strings.LastIndex(value, "@")
	if position <= 0 || position == len(value)-1 {
		return "", "", fmt.Errorf("package version must use @scope/name@version")
	}
	packageName, version := value[:position], value[position+1:]
	if _, _, err := registry.PackageParts(packageName); err != nil {
		return "", "", err
	}
	return packageName, version, nil
}

func firstMapValue(value map[string]any, keys ...string) any {
	for _, key := range keys {
		if item, ok := value[key]; ok {
			return item
		}
	}
	return ""
}
