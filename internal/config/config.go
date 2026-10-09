package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/Beam-Network/beam-cli-public/internal/version"
)

const DefaultOutput = "human"

// Service defaults can be injected with -ldflags so development and
// production bundles target their respective Beam environments.
var (
	DefaultRegistryURL    = "https://api.b1m.ai/registry"
	DefaultAuthURL        = "https://auth.b1m.ai"
	DefaultAPIURL         = "https://api.b1m.ai"
	DefaultCoordinatorURL = ""
)

type Config struct {
	RegistryURL    string `json:"registry_url,omitempty"`
	AuthURL        string `json:"auth_url,omitempty"`
	APIURL         string `json:"api_url,omitempty"`
	CoordinatorURL string `json:"coordinator_url,omitempty"`
	Organization   string `json:"organization,omitempty"`
	AgentSocket    string `json:"agent_socket,omitempty"`
	Output         string `json:"output,omitempty"`
}

type Paths struct {
	Dir           string
	Config        string
	Credentials   string
	AgentSocket   string
	Contexts      string
	ActiveContext string
	ContextName   string
	ContextConfig string
}

func ResolvePaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("resolve home directory: %w", err)
	}
	dir := strings.TrimSpace(os.Getenv("BEAM_CONFIG_DIR"))
	if dir == "" {
		if runtime.GOOS == "windows" {
			dir, err = os.UserConfigDir()
			if err != nil {
				return Paths{}, fmt.Errorf("resolve config directory: %w", err)
			}
			dir = filepath.Join(dir, version.StateNamespace())
		} else if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
			dir = filepath.Join(xdg, version.StateNamespace())
		} else {
			dir = filepath.Join(home, ".config", version.StateNamespace())
		}
	}
	socket := strings.TrimSpace(os.Getenv("BEAM_AGENT_SOCKET"))
	if socket == "" {
		if runtime.GOOS == "windows" {
			if number, ok := version.PullRequestNumber(); ok {
				socket = fmt.Sprintf(`\\.\pipe\beam-agent-pr%d`, number)
			} else if version.Development() {
				socket = `\\.\pipe\beam-agent-dev`
			} else {
				socket = `\\.\pipe\beam-agent`
			}
		} else {
			socket = filepath.Join(home, "."+version.StateNamespace(), "agent.sock")
		}
	}
	return Paths{
		Dir:           dir,
		Config:        filepath.Join(dir, "config.json"),
		Credentials:   filepath.Join(dir, "credentials.json"),
		AgentSocket:   socket,
		Contexts:      filepath.Join(dir, "contexts"),
		ActiveContext: filepath.Join(dir, "active_context"),
	}, nil
}

func Load() (Config, Paths, error) {
	return LoadContext("")
}

// LoadContext loads the base configuration followed by an optional named
// context. BEAM_* environment variables remain the final authority.
func LoadContext(requested string) (Config, Paths, error) {
	paths, err := ResolvePaths()
	if err != nil {
		return Config{}, Paths{}, err
	}
	cfg := Config{
		RegistryURL:    DefaultRegistryURL,
		AuthURL:        DefaultAuthURL,
		APIURL:         DefaultAPIURL,
		CoordinatorURL: DefaultCoordinatorURL,
		AgentSocket:    paths.AgentSocket,
		Output:         DefaultOutput,
	}
	if err := mergeFile(&cfg, paths.Config); err != nil {
		return Config{}, paths, err
	}
	contextName, err := selectedContext(paths, requested)
	if err != nil {
		return Config{}, paths, err
	}
	if contextName != "" {
		paths.ContextName = contextName
		paths.ContextConfig = contextPath(paths, contextName)
		if err := mergeFile(&cfg, paths.ContextConfig); err != nil {
			return Config{}, paths, err
		}
	}
	applyEnvironment(&cfg)
	if err := validate(cfg); err != nil {
		return Config{}, paths, err
	}
	return cfg, paths, nil
}

func Save(paths Paths, cfg Config) error {
	if err := validate(cfg); err != nil {
		return err
	}
	if err := os.MkdirAll(paths.Dir, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	if err := os.Chmod(paths.Dir, 0o700); err != nil {
		return fmt.Errorf("secure config directory: %w", err)
	}
	target := paths.Config
	if paths.ContextConfig != "" {
		target = paths.ContextConfig
	}
	return writeConfig(target, cfg)
}

func writeConfig(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("secure config directory: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return os.Chmod(path, 0o600)
}

func merge(target *Config, stored Config) {
	if stored.RegistryURL != "" {
		target.RegistryURL = stored.RegistryURL
	}
	if stored.AuthURL != "" {
		target.AuthURL = stored.AuthURL
	}
	if stored.APIURL != "" {
		target.APIURL = stored.APIURL
	}
	if stored.CoordinatorURL != "" {
		target.CoordinatorURL = stored.CoordinatorURL
	}
	if stored.Organization != "" {
		target.Organization = stored.Organization
	}
	if stored.AgentSocket != "" {
		target.AgentSocket = stored.AgentSocket
	}
	if stored.Output != "" {
		target.Output = stored.Output
	}
}

func mergeFile(cfg *Config, path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var stored Config
	if err := json.Unmarshal(data, &stored); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	merge(cfg, stored)
	return nil
}

func selectedContext(paths Paths, requested string) (string, error) {
	// A context asked for by --context or BEAM_CONTEXT must exist: falling back
	// to the base configuration would run the command against the wrong
	// deployment without saying so. The active context file is trusted instead,
	// because every command loads configuration before it runs, and failing here
	// would leave "beam context list" and "beam context use" with no way back.
	name := strings.TrimSpace(requested)
	if name == "" {
		name = strings.TrimSpace(os.Getenv("BEAM_CONTEXT"))
	}
	explicit := name != ""
	if name == "" {
		data, err := os.ReadFile(paths.ActiveContext)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("read active context: %w", err)
		}
		name = strings.TrimSpace(string(data))
	}
	if name == "" {
		return "", nil
	}
	if !ValidContextName(name) {
		return "", fmt.Errorf("invalid Beam context %q", name)
	}
	if explicit {
		if _, err := os.Stat(contextPath(paths, name)); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "", fmt.Errorf("context %q does not exist", name)
			}
			return "", err
		}
	}
	return name, nil
}

func contextPath(paths Paths, name string) string {
	return filepath.Join(paths.Contexts, name+".json")
}

func ValidContextName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, character := range name {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}

func ListContexts(paths Paths) ([]string, error) {
	entries, err := os.ReadDir(paths.Contexts)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			result = append(result, strings.TrimSuffix(entry.Name(), ".json"))
		}
	}
	slices.Sort(result)
	return result, nil
}

func CreateContext(paths Paths, name string, cfg Config) error {
	if !ValidContextName(name) {
		return fmt.Errorf("invalid Beam context %q", name)
	}
	path := contextPath(paths, name)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("context %q already exists", name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeConfig(path, cfg)
}

func UseContext(paths Paths, name string) error {
	if !ValidContextName(name) {
		return fmt.Errorf("invalid Beam context %q", name)
	}
	if _, err := os.Stat(contextPath(paths, name)); err != nil {
		return fmt.Errorf("context %q does not exist", name)
	}
	if err := os.MkdirAll(paths.Dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(paths.ActiveContext, []byte(name+"\n"), 0o600)
}

func DeleteContext(paths Paths, name string) error {
	active, err := selectedContext(paths, "")
	if err != nil {
		return err
	}
	if active == name {
		return fmt.Errorf("context %q is active", name)
	}
	if err := os.Remove(contextPath(paths, name)); err != nil {
		return err
	}
	return nil
}

func applyEnvironment(cfg *Config) {
	if value := strings.TrimSpace(os.Getenv("BEAM_REGISTRY_URL")); value != "" {
		cfg.RegistryURL = value
	}
	if value := strings.TrimSpace(os.Getenv("BEAM_AUTH_URL")); value != "" {
		cfg.AuthURL = value
	}
	if value := strings.TrimSpace(os.Getenv("BEAM_API_URL")); value != "" {
		cfg.APIURL = value
	}
	if value := strings.TrimSpace(os.Getenv("BEAM_COORDINATOR_URL")); value != "" {
		cfg.CoordinatorURL = value
	}
	if value := strings.TrimSpace(os.Getenv("BEAM_ORGANIZATION")); value != "" {
		cfg.Organization = value
	}
	if value := strings.TrimSpace(os.Getenv("BEAM_AGENT_SOCKET")); value != "" {
		cfg.AgentSocket = value
	}
	if value := strings.TrimSpace(os.Getenv("BEAM_OUTPUT")); value != "" {
		cfg.Output = value
	}
}

func validate(cfg Config) error {
	switch cfg.Output {
	case "human", "json", "quiet":
	default:
		return fmt.Errorf("BEAM_OUTPUT must be human, json, or quiet")
	}
	if cfg.RegistryURL == "" || cfg.AuthURL == "" || cfg.APIURL == "" || cfg.AgentSocket == "" {
		return fmt.Errorf("registry URL, auth URL, API URL, and agent socket must not be empty")
	}
	return nil
}
