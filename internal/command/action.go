package command

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/actionrun"
	"github.com/Beam-Network/beam-cli-public/internal/auth"
	"github.com/Beam-Network/beam-cli-public/internal/config"
	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/registry"
	"github.com/Beam-Network/beam-cli-public/internal/version"
)

func (a *App) action(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		return renderNamedHelp("action", renderer)
	}
	switch args[0] {
	case "init":
		return actionInit(args[1:], renderer)
	case "inspect", "validate":
		return registryInspect(args[1:], renderer)
	case "run":
		if has(args[1:], "--watch") {
			return actionRunWatch(ctx, withoutArgs(args[1:], "--watch"), renderer)
		}
		return actionRun(ctx, args[1:], renderer)
	case "pack":
		return actionPack(args[1:], renderer)
	case "publish":
		publishArgs := args[1:]
		if !hasFlag(publishArgs, "--tag") {
			publishArgs = append(publishArgs, "--tag", "latest")
		}
		return a.registryPublishReady(ctx, publishArgs, cfg, paths, renderer)
	case "version":
		return actionVersion(ctx, args[1:], cfg, a.authStore(paths.Credentials), renderer)
	default:
		return usage(fmt.Sprintf("unknown action command %q", args[0]))
	}
}

func actionInit(args []string, renderer output.Renderer) error {
	if err := validateOptions(args, map[string]bool{"--name": true}, nil); err != nil {
		return err
	}
	positionals, err := positional(args)
	if err != nil || len(positionals) > 1 {
		return usage("action init accepts at most one directory")
	}
	directory := "."
	if len(positionals) == 1 {
		directory = positionals[0]
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return usage("action directory is invalid")
	}
	name, _, err := flag(args, "--name")
	if err != nil {
		return err
	}
	if name == "" {
		slug := strings.ToLower(filepath.Base(absolute))
		slug = strings.Map(func(character rune) rune {
			if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' {
				return character
			}
			return '-'
		}, slug)
		name = "@beam/" + strings.Trim(slug, "-")
	}
	if _, _, err := registry.PackageParts(name); err != nil {
		return usage(err.Error())
	}
	files := map[string]string{
		"beam-action.json": fmt.Sprintf("{\n  \"apiVersion\": \"workflow-actions/v1\",\n  \"name\": %q,\n  \"version\": \"0.1.0\",\n  \"entrypoint\": \"src/index.mjs\",\n  \"runtime\": {\"placements\": [\"node\"]},\n  \"permissions\": []\n}\n", name),
		"src/index.mjs": `export default async function run(context) {
  return { outputs: { message: "Hello from Beam" }, state: context.state ?? {}, artifacts: [] };
}
`,
		"fixtures/config.local.json":  "{}\n",
		"fixtures/input.local.json":   "{}\n",
		"fixtures/secrets.local.json": "{}\n",
	}
	for relative := range files {
		if _, err := os.Stat(filepath.Join(absolute, relative)); err == nil {
			return cliError(ExitConflict, "Action initialization would overwrite existing files.", "Choose an empty directory.", nil)
		}
	}
	for relative, content := range files {
		path := filepath.Join(absolute, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if strings.Contains(relative, "secrets") {
			mode = 0o600
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			return err
		}
	}
	return renderer.Result(map[string]any{"directory": absolute, "package": name, "files": len(files)}, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "Initialized %s in %s.\nNext: %s action run %s\n", name, absolute, version.CLIName(), absolute)
		return err
	})
}

func actionPack(args []string, renderer output.Renderer) error {
	if err := validateOptions(args, map[string]bool{"--out": true}, nil); err != nil {
		return err
	}
	positionals, err := positional(args)
	if err != nil || len(positionals) > 1 {
		return usage("action pack accepts at most one directory")
	}
	directory := "."
	if len(positionals) == 1 {
		directory = positionals[0]
	}
	out, found, err := flag(args, "--out")
	if err != nil {
		return err
	}
	if !found {
		inspection, _, _, inspectErr := registry.Inspect(directory)
		if inspectErr != nil {
			return cliError(ExitUsage, "Action validation failed.", "Fix beam-action.json and the entrypoint.", inspectErr)
		}
		filename := strings.ReplaceAll(strings.TrimPrefix(inspection.Package, "@"), "/", "-") + "-" + inspection.Version + ".tgz"
		out = filepath.Join(directory, "dist", filename)
	}
	return registryPack([]string{directory, "--out", out}, renderer)
}

func actionVersion(ctx context.Context, args []string, cfg config.Config, store auth.Store, renderer output.Renderer) error {
	if len(args) > 1 {
		return usage("action version accepts at most one directory")
	}
	directory := "."
	if len(args) == 1 {
		directory = args[0]
	}
	inspection, _, _, err := registry.Inspect(directory)
	if err != nil {
		return cliError(ExitUsage, "Action validation failed.", "Fix beam-action.json and the entrypoint.", err)
	}
	tokens, _ := optionalRegistrySession(cfg, store)
	client := registry.Client{BaseURL: cfg.RegistryURL, Tokens: tokens}
	versions, remoteErr := client.Versions(ctx, inspection.Package)
	result := map[string]any{"package": inspection.Package, "local_version": inspection.Version, "published": versions.Versions}
	if remoteErr != nil {
		result["registry_error"] = remoteErr.Error()
	}
	return renderer.Result(result, func(w io.Writer) error {
		_, _ = fmt.Fprintf(w, "Local: %s@%s\n", inspection.Package, inspection.Version)
		for _, version := range versions.Versions {
			_, _ = fmt.Fprintf(w, "Published: %s\t%s\n", version.Version, version.Status)
		}
		return nil
	})
}

func actionRunWatch(ctx context.Context, args []string, renderer output.Renderer) error {
	positionals, err := positional(args)
	if err != nil || len(positionals) > 1 {
		return usage("action run --watch accepts at most one directory")
	}
	directory := "."
	if len(positionals) == 1 {
		directory = positionals[0]
	}
	last, _ := actionTreeModTime(directory)
	for {
		if err := actionRun(ctx, args, renderer); err != nil {
			renderer.Progress(err.Error())
		}
		renderer.Progress("Watching action files for changes...")
		ticker := time.NewTicker(500 * time.Millisecond)
		changed := false
		for !changed {
			select {
			case <-ctx.Done():
				ticker.Stop()
				return ctx.Err()
			case <-ticker.C:
				current, _ := actionTreeModTime(directory)
				if current.After(last) {
					last, changed = current, true
				}
			}
		}
		ticker.Stop()
	}
}

func actionTreeModTime(directory string) (time.Time, error) {
	var latest time.Time
	err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && (entry.Name() == ".git" || entry.Name() == "node_modules" || entry.Name() == "dist") && path != directory {
			return filepath.SkipDir
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err == nil && info.ModTime().After(latest) {
			latest = info.ModTime()
		}
		return err
	})
	return latest, err
}

func actionRun(ctx context.Context, args []string, renderer output.Renderer) error {
	valueFlags := map[string]bool{
		"--config":    true,
		"--inputs":    true,
		"--secrets":   true,
		"--log-level": true,
		"--runtime":   true,
	}
	if err := validateOptions(args, valueFlags, nil); err != nil {
		return err
	}
	positionals, err := positional(args)
	if err != nil {
		return err
	}
	if len(positionals) > 1 {
		return usage("action run accepts at most one directory")
	}
	directory := "."
	if len(positionals) == 1 {
		directory = positionals[0]
	}
	logLevel, _, err := flag(args, "--log-level")
	if err != nil {
		return err
	}
	if logLevel != "" && logLevel != "debug" && logLevel != "info" && logLevel != "warn" && logLevel != "error" {
		return usage("--log-level must be debug, info, warn, or error")
	}
	runtimeMode, _, err := flag(args, "--runtime")
	if err != nil {
		return err
	}
	if runtimeMode != "" && runtimeMode != "mock" && runtimeMode != "live" {
		return usage("--runtime must be mock or live")
	}
	configPath, _, err := flag(args, "--config")
	if err != nil {
		return err
	}
	inputsPath, _, err := flag(args, "--inputs")
	if err != nil {
		return err
	}
	secretsPath, _, err := flag(args, "--secrets")
	if err != nil {
		return err
	}
	var logs io.Writer
	if renderer.Mode == output.Human {
		logs = renderer.Err
	}
	renderer.Progress("Running local action...")
	result, err := actionrun.Run(ctx, actionrun.Options{
		Directory:   directory,
		ConfigPath:  configPath,
		InputsPath:  inputsPath,
		SecretsPath: secretsPath,
		LogLevel:    logLevel,
		Runtime:     runtimeMode,
		Logs:        logs,
	})
	if err != nil {
		message := "Could not run the action."
		if renderer.Mode == output.Human && !actionrun.WasReported(err) {
			message = fmt.Sprintf("Could not run the action: %v", err)
		}
		return cliError(
			ExitOperationFailed,
			message,
			"Check the action manifest, Node.js runtime, build, and fixture files.",
			err,
		)
	}
	return renderer.Result(result, func(w io.Writer) error {
		var formatted bytes.Buffer
		if err := json.Indent(&formatted, result, "", "  "); err != nil {
			return err
		}
		if err := formatted.WriteByte('\n'); err != nil {
			return err
		}
		_, err := w.Write(formatted.Bytes())
		return err
	})
}
