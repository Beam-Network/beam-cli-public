package command

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/Beam-Network/beam-cli-public/internal/auth"
	"github.com/Beam-Network/beam-cli-public/internal/config"
	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/registry"
)

func (a *App) registry(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		return renderNamedHelp("registry", renderer)
	}
	store := a.authStore(paths.Credentials)
	switch args[0] {
	case "login", "logout", "whoami":
		return usage(fmt.Sprintf(`registry authentication is centralized; use "beam auth %s"`, args[0]))
	case "inspect":
		return registryInspect(args[1:], renderer)
	case "pack":
		return registryPack(args[1:], renderer)
	case "publish":
		return a.registryPublishReady(ctx, args[1:], cfg, paths, renderer)
	case "versions":
		return registryVersions(ctx, args[1:], cfg, store, renderer)
	case "resolve":
		return registryResolve(ctx, args[1:], cfg, store, renderer)
	case "search":
		return registrySearch(ctx, args[1:], cfg, store, renderer)
	case "show":
		return registryShow(ctx, args[1:], cfg, store, renderer)
	case "download":
		return registryDownload(ctx, args[1:], cfg, store, renderer)
	case "verify":
		return registryVerify(args[1:], renderer)
	default:
		return usage(fmt.Sprintf("unknown registry command %q", args[0]))
	}
}

func (a *App) registryPublishReady(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	store := a.authStore(paths.Credentials)
	credentials, err := store.Load()
	if err != nil {
		return cliError(ExitConfig, "Could not read Beam credentials.", "Run `beam doctor`.", err)
	}
	if !credentials.Active() {
		if _, err := a.ensureAgentUserSession(ctx, cfg, paths, renderer); err != nil {
			return err
		}
	}
	return registryPublish(ctx, args, cfg, store, renderer, a.version.Version)
}

func registryInspect(args []string, renderer output.Renderer) error {
	if err := validateOptions(args, nil, nil); err != nil {
		return err
	}
	positionals, err := positional(args)
	if err != nil {
		return err
	}
	if len(positionals) > 1 {
		return usage("registry inspect accepts at most one directory")
	}
	directory := "."
	if len(positionals) == 1 {
		directory = positionals[0]
	}
	inspection, _, _, err := registry.Inspect(directory)
	if err != nil {
		return cliError(ExitUsage, "Package inspection failed.", "Fix beam-action.json and ensure the entrypoint exists.", err)
	}
	return renderer.Result(inspection, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "%s@%s\nEntrypoint: %s\nFiles: %d\nManifest: %s\n",
			inspection.Package, inspection.Version, inspection.Entrypoint, inspection.Files, inspection.ManifestChecksum)
		return err
	})
}

func registryPack(args []string, renderer output.Renderer) error {
	if err := validateOptions(args, map[string]bool{"--out": true}, nil); err != nil {
		return err
	}
	outputPath, err := requiredFlag(args, "--out")
	if err != nil {
		return err
	}
	positionals, err := positional(args)
	if err != nil {
		return err
	}
	if len(positionals) > 1 {
		return usage("registry pack accepts at most one directory")
	}
	directory := "."
	if len(positionals) == 1 {
		directory = positionals[0]
	}
	artifact, err := registry.Pack(directory, outputPath)
	if err != nil {
		return cliError(ExitOperationFailed, "Could not pack the action.", "Validate the package with beam registry inspect.", err)
	}
	return renderArtifact(artifact, renderer)
}

func registryPublish(ctx context.Context, args []string, cfg config.Config, store auth.Store, renderer output.Renderer, cliVersion string) error {
	if err := validateOptions(args, map[string]bool{"--tag": true}, nil); err != nil {
		return err
	}
	tag, err := requiredFlag(args, "--tag")
	if err != nil {
		return err
	}
	positionals, err := positional(args)
	if err != nil {
		return err
	}
	if len(positionals) > 1 {
		return usage("registry publish accepts at most one directory")
	}
	directory := "."
	if len(positionals) == 1 {
		directory = positionals[0]
	}
	credentials, session, err := requiredRegistrySession(cfg, store)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp("", "beam-package-*.tgz")
	if err != nil {
		return cliError(ExitOperationFailed, "Could not create a temporary package.", "", err)
	}
	tempPath := temp.Name()
	_ = temp.Close()
	defer os.Remove(tempPath)
	renderer.Progress("Packing action...")
	artifact, err := registry.Pack(directory, tempPath)
	if err != nil {
		return cliError(ExitOperationFailed, "Could not pack the action.", "Validate the package with beam registry inspect.", err)
	}
	data, err := os.ReadFile(artifact.Path)
	if err != nil {
		return cliError(ExitOperationFailed, "Could not read the package archive.", "", err)
	}
	trust, _ := artifact.Manifest["trustLevel"].(string)
	publishedBy := "cli"
	if credentials.User != nil {
		if credentials.User.Email != "" {
			publishedBy = credentials.User.Email
		} else if credentials.User.Name != "" {
			publishedBy = credentials.User.Name
		}
	}
	renderer.Progress("Publishing " + artifact.Package + "...")
	client := registry.Client{BaseURL: cfg.RegistryURL, Tokens: session}
	response, err := client.Publish(ctx, artifact.Package, registry.PublishRequest{
		Manifest: artifact.Manifest,
		Artifact: registry.PublishArtifact{
			ContentBase64: base64.StdEncoding.EncodeToString(data),
			Checksum:      artifact.ArtifactChecksum,
			SizeBytes:     artifact.SizeBytes,
			MediaType:     "application/gzip",
		},
		DistTags:         []string{tag},
		TrustLevel:       trust,
		ValidationStatus: "validated",
		PublishedBy:      publishedBy,
		Provenance: map[string]any{
			"source":      "beam-cli",
			"cli_version": cliVersion,
		},
	})
	if err != nil {
		return mapRegistryError(err)
	}
	if response.Version.Manifest != nil && !reflect.DeepEqual(response.Version.Manifest, artifact.Manifest) {
		return cliError(ExitOperationFailed, "Registry returned a different action manifest.", "Check the published version before using it.", nil)
	}
	if response.Version.ManifestChecksum != "" && !strings.EqualFold(strings.TrimPrefix(strings.ToLower(response.Version.ManifestChecksum), "sha256:"), strings.TrimPrefix(artifact.ManifestChecksum, "sha256:")) {
		return cliError(ExitOperationFailed, "Registry returned a different manifest checksum.", "Check the published version before using it.", nil)
	}
	if response.Version.ArtifactChecksum != "" && !strings.EqualFold(response.Version.ArtifactChecksum, artifact.ArtifactChecksum) {
		return cliError(ExitOperationFailed, "Registry returned a different artifact checksum.", "Check the published version before using it.", nil)
	}
	result := map[string]any{
		"package":  artifact.Package,
		"version":  artifact.Version,
		"tag":      tag,
		"checksum": artifact.ArtifactChecksum,
		"registry": response,
	}
	return renderer.Result(result, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "Published %s@%s with tag %s.\n", artifact.Package, artifact.Version, tag)
		return err
	})
}

func registryVersions(ctx context.Context, args []string, cfg config.Config, store auth.Store, renderer output.Renderer) error {
	if len(args) != 1 {
		return usage("registry versions requires one package")
	}
	if _, _, err := registry.PackageParts(args[0]); err != nil {
		return usage(err.Error())
	}
	tokens, loadErr := optionalRegistrySession(cfg, store)
	if loadErr != nil {
		return cliError(ExitConfig, "Could not read credentials.", "Check credentials.json.", loadErr)
	}
	client := registry.Client{BaseURL: cfg.RegistryURL, Tokens: tokens}
	result, err := client.Versions(ctx, args[0])
	if err != nil {
		return mapRegistryError(err)
	}
	return renderer.Result(result, func(w io.Writer) error {
		rows := make([][]string, 0, len(result.Versions))
		for _, item := range result.Versions {
			rows = append(rows, []string{item.Version, item.Status, item.ValidationStatus})
		}
		return renderer.Table(w, output.Table{
			Headers: []string{"VERSION", "STATUS", "VALIDATION"},
			Rows:    rows,
			Style:   output.RowStyle(1),
		})
	})
}

func registryResolve(ctx context.Context, args []string, cfg config.Config, store auth.Store, renderer output.Renderer) error {
	if err := validateOptions(args, map[string]bool{"--range": true}, nil); err != nil {
		return err
	}
	positionals, err := positional(args)
	if err != nil {
		return err
	}
	if len(positionals) != 1 {
		return usage("registry resolve requires one package")
	}
	versionRange := "latest"
	if value, ok, flagErr := flag(args, "--range"); flagErr != nil {
		return flagErr
	} else if ok {
		versionRange = value
	}
	if _, _, err := registry.PackageParts(positionals[0]); err != nil {
		return usage(err.Error())
	}
	tokens, loadErr := optionalRegistrySession(cfg, store)
	if loadErr != nil {
		return cliError(ExitConfig, "Could not read credentials.", "Check credentials.json.", loadErr)
	}
	client := registry.Client{BaseURL: cfg.RegistryURL, Tokens: tokens}
	result, err := client.Resolve(ctx, positionals[0], versionRange)
	if err != nil {
		return mapRegistryError(err)
	}
	return renderer.Result(result, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "%s@%s\n", positionals[0], result.ResolvedVersion)
		return err
	})
}

func requiredRegistrySession(cfg config.Config, store auth.Store) (auth.Credentials, *auth.Session, error) {
	credentials, err := store.Load()
	if err != nil {
		return auth.Credentials{}, nil, cliError(ExitConfig, "Could not read credentials.", "Check the OS credential store and credentials.json.", err)
	}
	if !credentials.Active() {
		return auth.Credentials{}, nil, cliError(ExitAuth, "Registry has no active Beam session.", `Run "beam auth login".`, nil)
	}
	return credentials, &auth.Session{
		Client: auth.DeviceClient{BaseURL: cfg.AuthURL},
		Store:  store,
	}, nil
}

// optionalRegistrySession returns the token source for a registry read, or a
// nil TokenSource when no credentials are active so the request is sent
// anonymously.
//
// The return type is the interface rather than *auth.Session on purpose. A nil
// *auth.Session assigned to registry.Client.Tokens leaves that interface field
// non-nil, so the client's "if c.Tokens != nil" guard passes and AccessToken is
// invoked on a nil receiver, which panics. Returning the interface keeps the
// nil a real nil at every call site.
func optionalRegistrySession(cfg config.Config, store auth.Store) (registry.TokenSource, error) {
	credentials, err := store.Load()
	if err != nil {
		return nil, err
	}
	if !credentials.Active() {
		return nil, nil
	}
	return &auth.Session{
		Client: auth.DeviceClient{BaseURL: cfg.AuthURL},
		Store:  store,
	}, nil
}

func mapRegistryError(err error) error {
	var registryErr *registry.Error
	if !errors.As(err, &registryErr) {
		return cliError(ExitOperationFailed, "Registry operation failed.", "Check the command inputs and try again.", err)
	}
	switch registryErr.Kind {
	case registry.ErrUnavailable:
		return cliError(ExitRegistryUnavailable, "Registry is unreachable.", "Check BEAM_REGISTRY_URL and your network connection.", err)
	case registry.ErrAuth:
		return cliError(ExitAuth, "Registry authentication is invalid or expired.", `Run "beam auth login" again.`, err)
	case registry.ErrNotFound:
		return cliError(ExitNotFound, "Registry resource was not found.", "Check the package name or version.", err)
	case registry.ErrConflict:
		return cliError(ExitConflict, "Registry rejected the operation because it conflicts with existing state.", "Use a new immutable version or inspect the package.", err)
	default:
		return cliError(ExitOperationFailed, "Registry operation failed.", "Try again or inspect the Registry response.", err)
	}
}

func renderArtifact(artifact registry.Artifact, renderer output.Renderer) error {
	return renderer.Result(artifact, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "Packed %s@%s\nOutput: %s\nFiles: %d\nBytes: %d\nChecksum: %s\n",
			artifact.Package, artifact.Version, filepath.Clean(artifact.Path), artifact.Files, artifact.SizeBytes, artifact.ArtifactChecksum)
		return err
	})
}
