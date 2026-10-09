package update

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

var DefaultBaseURL = "https://cdn.b1m.ai/cli"

const (
	maxManifest  = 1 << 20
	maxChecksums = 4 << 20
	maxArchive   = 512 << 20
)

// Options controls release resolution and installation. The unexported
// platform activation step always replaces beam and beam-agentd as one bundle.
type Options struct {
	BaseURL         string
	CurrentVersion  string
	TargetVersion   string
	ChannelManifest string
	CheckOnly       bool
	Force           bool
	GOOS            string
	GOARCH          string
	ExecutablePath  string
	HTTPClient      *http.Client
	BeforeActivate  func(context.Context) error
	AfterActivate   func(context.Context) error
}

type Result struct {
	CurrentVersion  string `json:"current_version"`
	Version         string `json:"version"`
	UpdateAvailable bool   `json:"update_available"`
	Updated         bool   `json:"updated"`
	Deferred        bool   `json:"deferred,omitempty"`
	InstallDir      string `json:"install_dir,omitempty"`
}

type manifest struct {
	SchemaVersion int    `json:"schemaVersion"`
	Version       string `json:"version"`
}

// Run resolves, downloads, verifies, and activates a matching Beam bundle.
func Run(ctx context.Context, options Options) (Result, error) {
	resolved, err := resolveOptions(options)
	if err != nil {
		return Result{}, err
	}
	target, err := resolveVersion(ctx, resolved)
	if err != nil {
		return Result{}, err
	}

	result := Result{
		CurrentVersion:  resolved.CurrentVersion,
		Version:         target,
		UpdateAvailable: !sameVersion(resolved.CurrentVersion, target),
	}
	if options.CheckOnly || (!result.UpdateAvailable && !options.Force) {
		return result, nil
	}

	executable, err := executablePath(resolved.ExecutablePath)
	if err != nil {
		return Result{}, fmt.Errorf("resolve the installed beam executable: %w", err)
	}
	result.InstallDir = filepath.Dir(executable)

	archiveName, err := releaseArchive(target, resolved.GOOS, resolved.GOARCH)
	if err != nil {
		return Result{}, err
	}
	releaseURL := resolved.BaseURL + "/releases/" + target
	temporary, err := os.MkdirTemp("", "beam-update-")
	if err != nil {
		return Result{}, fmt.Errorf("create update staging directory: %w", err)
	}
	if err := os.Chmod(temporary, 0o700); err != nil {
		_ = os.RemoveAll(temporary)
		return Result{}, fmt.Errorf("secure update staging directory: %w", err)
	}
	preserveTemporary := false
	defer func() {
		if !preserveTemporary {
			_ = os.RemoveAll(temporary)
		}
	}()

	archivePath := filepath.Join(temporary, archiveName)
	if err := downloadFile(ctx, resolved, releaseURL+"/"+archiveName, archivePath, maxArchive); err != nil {
		return Result{}, fmt.Errorf("download %s: %w", archiveName, err)
	}
	checksums, err := downloadBytes(ctx, resolved, releaseURL+"/checksums.txt", maxChecksums)
	if err != nil {
		return Result{}, fmt.Errorf("download release checksums: %w", err)
	}
	if err := verifyChecksum(archivePath, archiveName, checksums); err != nil {
		return Result{}, err
	}
	if err := extractBundle(archivePath, temporary, resolved.GOOS); err != nil {
		return Result{}, fmt.Errorf("extract verified Beam bundle: %w", err)
	}

	if resolved.BeforeActivate != nil {
		if err := resolved.BeforeActivate(ctx); err != nil {
			return Result{}, fmt.Errorf("stop beam-agentd before update: %w", err)
		}
	}
	deferred, preserve, err := activate(temporary, executable, resolved.GOOS)
	if err != nil {
		return Result{}, fmt.Errorf("install verified Beam bundle: %w", err)
	}
	preserveTemporary = preserve
	if !deferred && resolved.AfterActivate != nil {
		if err := resolved.AfterActivate(ctx); err != nil {
			return Result{}, fmt.Errorf("restart beam-agentd after update: %w", err)
		}
	}
	result.Updated = !deferred
	result.Deferred = deferred
	return result, nil
}

func resolveOptions(options Options) (Options, error) {
	options.BaseURL = strings.TrimRight(strings.TrimSpace(options.BaseURL), "/")
	if options.BaseURL == "" {
		options.BaseURL = DefaultBaseURL
	}
	parsed, err := url.Parse(options.BaseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && !isLoopbackHTTP(parsed)) {
		return Options{}, errors.New("BEAM_CDN_BASE_URL must be an HTTPS URL")
	}
	if options.GOOS == "" {
		options.GOOS = runtime.GOOS
	}
	if options.GOARCH == "" {
		options.GOARCH = runtime.GOARCH
	}
	if options.HTTPClient == nil {
		options.HTTPClient = &http.Client{Timeout: 2 * time.Minute}
	}
	return options, nil
}

func resolveVersion(ctx context.Context, options Options) (string, error) {
	if strings.TrimSpace(options.TargetVersion) != "" {
		return normalizeVersion(options.TargetVersion)
	}
	manifestName := strings.TrimSpace(options.ChannelManifest)
	if manifestName == "" {
		manifestName = "latest.json"
	}
	if strings.ContainsAny(manifestName, `/\\`) || !strings.HasSuffix(manifestName, ".json") {
		return "", errors.New("invalid release channel manifest")
	}
	data, err := downloadBytes(ctx, options, options.BaseURL+"/"+manifestName, maxManifest)
	if err != nil {
		return "", fmt.Errorf("resolve the latest Beam release: %w", err)
	}
	var latest manifest
	if err := json.Unmarshal(data, &latest); err != nil {
		return "", fmt.Errorf("decode latest.json: %w", err)
	}
	if latest.SchemaVersion != 1 {
		return "", fmt.Errorf("unsupported latest.json schema version %d", latest.SchemaVersion)
	}
	return normalizeVersion(latest.Version)
}

func normalizeVersion(raw string) (string, error) {
	plain := strings.TrimPrefix(strings.TrimSpace(raw), "v")
	if plain == "" {
		return "", errors.New("release version is empty")
	}
	for _, character := range plain {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '.' || character == '-' {
			continue
		}
		return "", fmt.Errorf("release version %q contains unsafe characters", raw)
	}
	return "v" + plain, nil
}

func sameVersion(current, target string) bool {
	if strings.TrimSpace(current) == "" || strings.EqualFold(strings.TrimSpace(current), "dev") {
		return false
	}
	normalized, err := normalizeVersion(current)
	return err == nil && normalized == target
}

func releaseArchive(version, goos, goarch string) (string, error) {
	supported := goarch == "amd64" || goarch == "arm64"
	if !supported || (goos != "darwin" && goos != "linux" && goos != "windows") || (goos == "windows" && goarch != "amd64") {
		return "", fmt.Errorf("Beam updates are not published for %s/%s", goos, goarch)
	}
	extension := ".tar.gz"
	if goos == "windows" {
		extension = ".zip"
	}
	return fmt.Sprintf("beam_%s_%s_%s%s", strings.TrimPrefix(version, "v"), goos, goarch, extension), nil
}

func executablePath(configured string) (string, error) {
	path := strings.TrimSpace(configured)
	if path == "" {
		var err error
		path, err = os.Executable()
		if err != nil {
			return "", err
		}
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = resolved
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("beam executable is not a regular file")
	}
	return absolute, nil
}

func downloadBytes(ctx context.Context, options Options, rawURL string, limit int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json, text/plain")
	request.Header.Set("User-Agent", "beam/"+options.CurrentVersion)
	response, err := options.HTTPClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d from %s", response.StatusCode, request.URL.Host)
	}
	if !secureResponseURL(response.Request.URL) {
		return nil, errors.New("release download was redirected to an insecure URL")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("release response exceeds the allowed size")
	}
	return data, nil
}

func downloadFile(ctx context.Context, options Options, rawURL, destination string, limit int64) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/octet-stream")
	request.Header.Set("User-Agent", "beam/"+options.CurrentVersion)
	response, err := options.HTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d from %s", response.StatusCode, request.URL.Host)
	}
	if !secureResponseURL(response.Request.URL) {
		return errors.New("release download was redirected to an insecure URL")
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(file, io.LimitReader(response.Body, limit+1))
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if written > limit {
		return errors.New("release archive exceeds the allowed size")
	}
	return nil
}

func secureResponseURL(parsed *url.URL) bool {
	return parsed != nil && (parsed.Scheme == "https" || isLoopbackHTTP(parsed))
}

func isLoopbackHTTP(parsed *url.URL) bool {
	if parsed == nil || parsed.Scheme != "http" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func verifyChecksum(archivePath, archiveName string, checksums []byte) error {
	var expected string
	scanner := bufio.NewScanner(strings.NewReader(string(checksums)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == archiveName {
			expected = strings.ToLower(fields[0])
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read release checksums: %w", err)
	}
	if len(expected) != sha256.Size*2 {
		return fmt.Errorf("checksum for %s is missing or invalid", archiveName)
	}
	if _, err := hex.DecodeString(expected); err != nil {
		return fmt.Errorf("checksum for %s is invalid", archiveName)
	}
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("hash release archive: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close release archive: %w", closeErr)
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if actual != expected {
		return fmt.Errorf("checksum verification failed for %s", archiveName)
	}
	return nil
}
