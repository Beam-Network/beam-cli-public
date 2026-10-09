package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/version"
)

func TestCheckOnlyUsesManifestWithoutDownloadingBundle(t *testing.T) {
	var archiveRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/latest.json" {
			_, _ = w.Write([]byte(`{"schemaVersion":1,"version":"v1.2.3"}`))
			return
		}
		archiveRequests.Add(1)
		http.NotFound(w, request)
	}))
	defer server.Close()

	result, err := Run(context.Background(), Options{
		BaseURL:        server.URL,
		CurrentVersion: "v1.0.0",
		CheckOnly:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.UpdateAvailable || result.Version != "v1.2.3" || result.Updated {
		t.Fatalf("result=%#v", result)
	}
	if archiveRequests.Load() != 0 {
		t.Fatalf("check downloaded %d release resources", archiveRequests.Load())
	}
}

func TestAlreadyCurrentDoesNotDownloadOrStopDaemon(t *testing.T) {
	var resourceRequests atomic.Int32
	var beforeActivate atomic.Bool
	var afterActivate atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/latest.json" {
			_, _ = w.Write([]byte(`{"schemaVersion":1,"version":"v1.2.3"}`))
			return
		}
		resourceRequests.Add(1)
		http.NotFound(w, request)
	}))
	defer server.Close()

	result, err := Run(context.Background(), Options{
		BaseURL:        server.URL,
		CurrentVersion: "1.2.3",
		BeforeActivate: func(context.Context) error {
			beforeActivate.Store(true)
			return nil
		},
		AfterActivate: func(context.Context) error {
			afterActivate.Store(true)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.UpdateAvailable || result.Updated || result.Deferred {
		t.Fatalf("result=%#v", result)
	}
	if resourceRequests.Load() != 0 || beforeActivate.Load() || afterActivate.Load() {
		t.Fatalf("resources=%d beforeActivate=%t afterActivate=%t", resourceRequests.Load(), beforeActivate.Load(), afterActivate.Load())
	}
}

func TestCheckOnlyUsesConfiguredChannelManifest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/pr-27.json" {
			t.Errorf("manifest path = %q", request.URL.Path)
			http.NotFound(w, request)
			return
		}
		_, _ = w.Write([]byte(`{"schemaVersion":1,"version":"v0.0.0-pr.27.cli.agent"}`))
	}))
	defer server.Close()

	result, err := Run(context.Background(), Options{
		BaseURL:         server.URL,
		CurrentVersion:  "v0.0.0-pr.27.old.old",
		ChannelManifest: "pr-27.json",
		CheckOnly:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.UpdateAvailable || result.Version != "v0.0.0-pr.27.cli.agent" {
		t.Fatalf("result=%#v", result)
	}
}

func TestRejectsUnsafeChannelManifest(t *testing.T) {
	_, err := Run(context.Background(), Options{
		BaseURL:         "https://cdn.example.test",
		ChannelManifest: "../latest.json",
		CheckOnly:       true,
	})
	if err == nil || !strings.Contains(err.Error(), "channel manifest") {
		t.Fatalf("err=%v", err)
	}
}

func TestChecksumFailureDoesNotStopDaemonOrReplaceFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows replacement is intentionally deferred")
	}
	archive := testTarGzip(t, map[string]string{version.CLIName(): "new beam", version.AgentName(): "new agent"})
	server := releaseServer(t, "v2.0.0", archive, strings.Repeat("0", 64))
	defer server.Close()
	installDir := t.TempDir()
	beamPath := filepath.Join(installDir, version.CLIName())
	agentPath := filepath.Join(installDir, version.AgentName())
	writeExecutable(t, beamPath, "old beam")
	writeExecutable(t, agentPath, "old agent")
	var beforeActivate atomic.Bool
	var afterActivate atomic.Bool

	_, err := Run(context.Background(), Options{
		BaseURL:        server.URL,
		CurrentVersion: "v1.0.0",
		ExecutablePath: beamPath,
		BeforeActivate: func(context.Context) error {
			beforeActivate.Store(true)
			return nil
		},
		AfterActivate: func(context.Context) error {
			afterActivate.Store(true)
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "checksum verification failed") {
		t.Fatalf("err=%v", err)
	}
	if beforeActivate.Load() || afterActivate.Load() {
		t.Fatal("daemon lifecycle callback ran before bundle verification")
	}
	assertFile(t, beamPath, "old beam")
	assertFile(t, agentPath, "old agent")
}

func TestRejectsInsecureCDNOverride(t *testing.T) {
	_, err := Run(context.Background(), Options{BaseURL: "http://cdn.example.test", CheckOnly: true})
	if err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("err=%v", err)
	}
}

func TestRejectsUnsafePinnedVersion(t *testing.T) {
	_, err := Run(context.Background(), Options{
		BaseURL:       "https://cdn.example.test",
		TargetVersion: "../../private",
		CheckOnly:     true,
	})
	if err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("err=%v", err)
	}
}

func TestExtractWindowsBundleRequiresBothExecutables(t *testing.T) {
	destination := t.TempDir()
	archivePath := filepath.Join(destination, "bundle.zip")
	var contents bytes.Buffer
	archive := zip.NewWriter(&contents)
	for name, data := range map[string]string{
		version.ExecutableName(version.CLIName(), "windows"):   "windows beam",
		version.ExecutableName(version.AgentName(), "windows"): "windows agent",
		"README.md": "ignored",
	} {
		file, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archivePath, contents.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := extractBundle(archivePath, destination, "windows"); err != nil {
		t.Fatal(err)
	}
	assertFile(t, filepath.Join(destination, version.ExecutableName(version.CLIName(), "windows")), "windows beam")
	assertFile(t, filepath.Join(destination, version.ExecutableName(version.AgentName(), "windows")), "windows agent")
}

func releaseServer(t *testing.T, version string, archive []byte, expectedChecksum string) *httptest.Server {
	t.Helper()
	archiveName, err := releaseArchive(version, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/latest.json":
			_, _ = fmt.Fprintf(w, `{"schemaVersion":1,"version":%q}`, version)
		case "/releases/" + version + "/" + archiveName:
			_, _ = w.Write(archive)
		case "/releases/" + version + "/checksums.txt":
			_, _ = fmt.Fprintf(w, "%s  %s\n", expectedChecksum, archiveName)
		default:
			http.NotFound(w, request)
		}
	}))
}

func testTarGzip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var result bytes.Buffer
	compressed := gzip.NewWriter(&result)
	archive := tar.NewWriter(compressed)
	for name, contents := range files {
		if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(contents))}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write([]byte(contents)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return result.Bytes()
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
}

func assertFile(t *testing.T, path, expected string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != expected {
		t.Fatalf("%s=%q want %q", path, data, expected)
	}
}

func archiveChecksum(data []byte) string {
	hash := sha256.Sum256(data)
	return fmt.Sprintf("%x", hash[:])
}
