package actionrun

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Beam-Network/beam-cli-public/internal/registry"
)

//go:embed runner.mjs
var runnerScript string

type Options struct {
	Directory   string
	ConfigPath  string
	InputsPath  string
	SecretsPath string
	LogLevel    string
	Runtime     string
	Logs        io.Writer
}

type ReportedError struct {
	Err      error
	Reported bool
}

func (err *ReportedError) Error() string {
	return err.Err.Error()
}

func (err *ReportedError) Unwrap() error {
	return err.Err
}

func WasReported(err error) bool {
	var reported *ReportedError
	return errors.As(err, &reported) && reported.Reported
}

type manifest struct {
	Name       string `json:"name"`
	Entrypoint string `json:"entrypoint"`
	Runtime    struct {
		Type       string `json:"type"`
		Entrypoint string `json:"entrypoint"`
	} `json:"runtime"`
	Scripts struct {
		Build string `json:"build"`
	} `json:"scripts"`
}

type packageJSON struct {
	Scripts struct {
		Build string `json:"build"`
	} `json:"scripts"`
}

type runnerInput struct {
	PackageName string         `json:"packageName"`
	Config      map[string]any `json:"config"`
	Inputs      map[string]any `json:"inputs"`
	Secrets     map[string]any `json:"secrets"`
}

func Run(ctx context.Context, options Options) (json.RawMessage, error) {
	root, err := filepath.Abs(defaultString(options.Directory, "."))
	if err != nil {
		return nil, fmt.Errorf("resolve action directory: %w", err)
	}
	manifestPath := filepath.Join(root, "beam-action.json")
	var actionManifest manifest
	if err := readJSON(manifestPath, &actionManifest); err != nil {
		return nil, err
	}
	if actionManifest.Runtime.Type != "" && actionManifest.Runtime.Type != "node" {
		return nil, fmt.Errorf("action runtime %q is not supported by the local runner", actionManifest.Runtime.Type)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		return nil, fmt.Errorf("Node.js is required to run Node action packages: %w", err)
	}
	buildScript := strings.TrimSpace(actionManifest.Scripts.Build)
	if buildScript == "" {
		var actionPackage packageJSON
		packagePath := filepath.Join(root, "package.json")
		if err := readJSONIfExists(packagePath, &actionPackage); err != nil {
			return nil, err
		}
		buildScript = strings.TrimSpace(actionPackage.Scripts.Build)
	}
	if buildScript != "" {
		if err := runBuild(ctx, root, buildScript, options.Logs); err != nil {
			return nil, &ReportedError{Err: err, Reported: options.Logs != nil}
		}
	}
	inspection, _, _, err := registry.Inspect(root)
	if err != nil {
		return nil, err
	}
	entrypoint := actionManifest.Entrypoint
	if entrypoint == "" {
		entrypoint = actionManifest.Runtime.Entrypoint
	}
	if entrypoint == "" {
		entrypoint = inspection.Entrypoint
	}

	config, err := readInputFile(root, options.ConfigPath, "fixtures/config.local.json")
	if err != nil {
		return nil, fmt.Errorf("read action config: %w", err)
	}
	inputs, err := readInputFile(root, options.InputsPath, "fixtures/input.local.json")
	if err != nil {
		return nil, fmt.Errorf("read action inputs: %w", err)
	}
	secrets, err := readInputFile(root, options.SecretsPath, "fixtures/secrets.local.json")
	if err != nil {
		return nil, fmt.Errorf("read action secrets: %w", err)
	}
	payload, err := json.Marshal(runnerInput{
		PackageName: inspection.Package,
		Config:      config,
		Inputs:      inputs,
		Secrets:     secrets,
	})
	if err != nil {
		return nil, fmt.Errorf("encode local action input: %w", err)
	}

	command := exec.CommandContext(ctx, node, "--input-type=module", "--eval", runnerScript)
	command.Dir = root
	command.Env = append(os.Environ(),
		"BEAM_ACTION_DIRECTORY="+root,
		"BEAM_ACTION_ENTRYPOINT="+filepath.Join(root, filepath.FromSlash(entrypoint)),
		"BEAM_ACTION_LOG_LEVEL="+defaultString(options.LogLevel, "info"),
		"BEAM_ACTION_RUNTIME="+defaultString(options.Runtime, "mock"),
	)
	command.Stdin = bytes.NewReader(payload)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	if options.Logs == nil {
		command.Stderr = &stderr
	} else {
		command.Stderr = io.MultiWriter(options.Logs, &stderr)
	}
	if err := command.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return nil, &ReportedError{
			Err:      fmt.Errorf("action execution failed: %s", detail),
			Reported: options.Logs != nil,
		}
	}
	var result json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return nil, fmt.Errorf("action returned invalid JSON: %w", err)
	}
	return result, nil
}

func readInputFile(root, suppliedPath, defaultRelativePath string) (map[string]any, error) {
	path := suppliedPath
	explicit := path != ""
	if path == "" {
		path = filepath.Join(root, filepath.FromSlash(defaultRelativePath))
	} else if !filepath.IsAbs(path) {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		path = absolute
	}
	var value map[string]any
	err := readJSON(path, &value)
	if errors.Is(err, os.ErrNotExist) && !explicit {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, fmt.Errorf("%s must contain a JSON object", path)
	}
	return value, nil
}

func readJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

func readJSONIfExists(path string, target any) error {
	err := readJSON(path, target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func runBuild(ctx context.Context, directory, script string, logs io.Writer) error {
	var command *exec.Cmd
	if runtime.GOOS == "windows" {
		command = exec.CommandContext(ctx, "cmd", "/C", script)
	} else {
		command = exec.CommandContext(ctx, "/bin/sh", "-c", script)
	}
	command.Dir = directory
	var output bytes.Buffer
	if logs == nil {
		command.Stdout = &output
		command.Stderr = &output
	} else {
		writer := io.MultiWriter(logs, &output)
		command.Stdout = writer
		command.Stderr = writer
	}
	if err := command.Run(); err != nil {
		detail := strings.TrimSpace(output.String())
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("action build failed: %s", detail)
	}
	return nil
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
