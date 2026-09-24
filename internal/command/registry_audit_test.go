package command

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/auth"
	"github.com/Beam-Network/beam-cli-public/internal/config"
	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/registry"
)

// This exercises the command handlers and their HTTP envelope. The local
// server echoes a publication; Registry validation is covered upstream.
func TestRegistryLegacyCommandRoundTripAgainstLocalServer(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifestBytes := []byte(`{"name":"@example/legacy","version":"1.0.0","apiVersion":"workflow-actions/v1","entrypoint":"dist/index.mjs","runtime":{"placements":["local-workers"]},"inputs":{"input":{"type":"object"}},"outputs":{"output":{"type":"object"}}}`)
	if err := os.WriteFile(filepath.Join(root, "beam-action.json"), manifestBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dist", "index.mjs"), []byte("export default 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var original map[string]any
	if err := json.Unmarshal(manifestBytes, &original); err != nil {
		t.Fatal(err)
	}
	var published registry.PublishRequest
	var archive []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/token":
			_, _ = io.WriteString(w, `{"access_token":"local-token","token_type":"Bearer","expires_in":3600,"refresh_token":"next-token","scope":"cli:access"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/packages/@example/legacy/versions":
			if r.Header.Get("Authorization") != "Bearer local-token" {
				t.Error("publication omitted the refreshed bearer token")
			}
			if err := json.NewDecoder(r.Body).Decode(&published); err != nil {
				t.Errorf("decode publication: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var err error
			archive, err = base64.StdEncoding.DecodeString(published.Artifact.ContentBase64)
			if err != nil {
				t.Errorf("decode archive: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(registry.PublishResponse{Version: registry.Version{
				Manifest: published.Manifest, ArtifactChecksum: published.Artifact.Checksum,
				ArtifactSize: published.Artifact.SizeBytes,
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/resolve/@example/legacy" && r.URL.Query().Get("range") == "1.0.0":
			_ = json.NewEncoder(w).Encode(registry.ResolveResponse{Version: registry.Version{
				Manifest: published.Manifest, ArtifactChecksum: published.Artifact.Checksum,
				ArtifactSize: published.Artifact.SizeBytes,
			}, ResolvedVersion: "1.0.0"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/packages/@example/legacy/versions/1.0.0/artifact":
			w.Header().Set("Digest", published.Artifact.Checksum)
			_, _ = w.Write(archive)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	store := auth.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	if err := store.Save(auth.Credentials{RefreshToken: "initial-token"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{AuthURL: server.URL, RegistryURL: server.URL}
	var result bytes.Buffer
	renderer := output.Renderer{Mode: output.JSON, Out: &result, Err: io.Discard}
	archivePath := filepath.Join(t.TempDir(), "legacy.tgz")
	steps := []struct {
		name string
		run  func() error
	}{
		{"inspect", func() error { return registryInspect([]string{root}, renderer) }},
		{"pack", func() error { return registryPack([]string{root, "--out", archivePath}, renderer) }},
		{"verify packed", func() error { return registryVerify([]string{archivePath}, renderer) }},
		{"publish", func() error {
			return registryPublish(context.Background(), []string{root, "--tag", "latest"}, cfg, store, renderer, "test")
		}},
		{"resolve", func() error {
			return registryResolve(context.Background(), []string{"@example/legacy", "--range", "1.0.0"}, cfg, store, renderer)
		}},
	}
	for _, step := range steps {
		result.Reset()
		if err := step.run(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
	}
	var resolved registry.ResolveResponse
	if err := json.Unmarshal(result.Bytes(), &resolved); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(archive)
	wantChecksum := "sha256:" + hex.EncodeToString(digest[:])
	if !reflect.DeepEqual(published.Manifest, original) || !reflect.DeepEqual(resolved.Version.Manifest, original) ||
		published.Artifact.Checksum != wantChecksum || resolved.Version.ArtifactChecksum != wantChecksum ||
		published.Artifact.SizeBytes != int64(len(archive)) || resolved.Version.ArtifactSize != int64(len(archive)) {
		t.Fatalf("round trip changed manifest or artifact: publish=%#v resolve=%#v", published, resolved)
	}
	result.Reset()
	downloadPath := filepath.Join(t.TempDir(), "download.tgz")
	if err := registryDownload(context.Background(), []string{"@example/legacy@1.0.0", "--out", downloadPath}, cfg, store, renderer); err != nil {
		t.Fatal(err)
	}
	if err := registryVerify([]string{downloadPath}, renderer); err != nil {
		t.Fatal(err)
	}
	downloaded, err := os.ReadFile(downloadPath)
	if err != nil || !bytes.Equal(downloaded, archive) {
		t.Fatalf("download differs from published archive: %v", err)
	}
}
