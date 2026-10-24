package registry

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRegistryManifestConformance(t *testing.T) {
	fixtures := os.Getenv("BEAM_REGISTRY_FIXTURES")
	if fixtures == "" {
		t.Skip("set BEAM_REGISTRY_FIXTURES to a Registry manifest fixtures directory")
	}
	if _, err := os.Stat(filepath.Join(fixtures, "manifests", "cases.json")); err != nil {
		t.Fatalf("BEAM_REGISTRY_FIXTURES has no manifests/cases.json: %v", err)
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node.js is needed to run the shared fixture materializer")
	}
	data, err := os.ReadFile(filepath.Join(fixtures, "manifests", "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		File     string `json:"file"`
		Valid    bool   `json:"valid"`
		Error    string `json:"error"`
		Checksum string `json:"checksum"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("shared fixture index is empty")
	}
	for _, tc := range cases {
		t.Run(tc.File, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "action")
			command := exec.Command("node", filepath.Join(fixtures, "materialize-project.mjs"), tc.File, root)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("materialize: %s: %v", output, err)
			}
			inspection, manifest, _, err := Inspect(root)
			if !tc.Valid {
				if err == nil || !strings.Contains(err.Error(), tc.Error) {
					t.Fatalf("Inspect error = %v, want %q", err, tc.Error)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.Checksum != "" && inspection.ManifestChecksum != "sha256:"+tc.Checksum {
				t.Fatalf("checksum = %s, want %s", inspection.ManifestChecksum, tc.Checksum)
			}
			archive := filepath.Join(t.TempDir(), "action.tgz")
			artifact, err := Pack(root, archive)
			if err != nil {
				t.Fatal(err)
			}
			if artifact.ManifestChecksum != inspection.ManifestChecksum {
				t.Fatal("manifest checksum changed while packing")
			}
			if tc.File == "v2-single.valid.json" {
				testPublishResolveRoundTrip(t, artifact)
			}
			file, err := os.Open(archive)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			gz, err := gzip.NewReader(file)
			if err != nil {
				t.Fatal(err)
			}
			defer gz.Close()
			tarReader := tar.NewReader(gz)
			for {
				header, err := tarReader.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if header.Name != "beam-action.json" {
					continue
				}
				var archived map[string]any
				if err := json.NewDecoder(tarReader).Decode(&archived); err != nil {
					t.Fatal(err)
				}
				left, _ := CanonicalManifest(manifest)
				right, _ := CanonicalManifest(archived)
				if string(left) != string(right) {
					t.Fatal("archive changed the Registry manifest")
				}
				return
			}
			t.Fatal("archive omitted beam-action.json")
		})
	}
}

func testPublishResolveRoundTrip(t *testing.T, artifact Artifact) {
	t.Helper()
	archive, err := os.ReadFile(artifact.Path)
	if err != nil {
		t.Fatal(err)
	}
	version := Version{
		PackageName: artifact.Package, Version: artifact.Version,
		Manifest: artifact.Manifest, ManifestChecksum: strings.TrimPrefix(artifact.ManifestChecksum, "sha256:"),
		ArtifactChecksum: artifact.ArtifactChecksum, ArtifactSize: artifact.SizeBytes,
		Provenance: map[string]any{"source": "beam-cli"},
	}
	var published bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/versions"):
			request, decoded := decodeMultipartPublish(t, r)
			if !reflect.DeepEqual(request.Manifest, artifact.Manifest) || !reflect.DeepEqual(decoded, archive) || request.Artifact.Checksum != artifact.ArtifactChecksum {
				t.Error("publish changed the manifest or artifact")
				w.WriteHeader(400)
				return
			}
			published = true
			_ = json.NewEncoder(w).Encode(PublishResponse{Version: version})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/v1/resolve/"):
			_ = json.NewEncoder(w).Encode(ResolveResponse{Version: version, ResolvedVersion: version.Version})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/artifact"):
			w.Header().Set("Digest", artifact.ArtifactChecksum)
			_, _ = w.Write(archive)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := Client{BaseURL: server.URL}
	response, err := client.Publish(context.Background(), artifact.Package, PublishRequest{
		Manifest: artifact.Manifest,
		Artifact: PublishArtifact{Checksum: artifact.ArtifactChecksum, SizeBytes: artifact.SizeBytes, MediaType: "application/gzip"},
	}, archive)
	if err != nil || !published {
		t.Fatalf("publish: %v, called=%v", err, published)
	}
	if !reflect.DeepEqual(response.Version.Manifest, artifact.Manifest) || response.Version.ManifestChecksum != version.ManifestChecksum {
		t.Fatal("publish response lost manifest fields or checksum")
	}
	resolved, err := client.Resolve(context.Background(), artifact.Package, artifact.Version)
	if err != nil || !reflect.DeepEqual(resolved.Version.Manifest, artifact.Manifest) || resolved.Version.ManifestChecksum != version.ManifestChecksum || !reflect.DeepEqual(resolved.Version.Provenance, version.Provenance) {
		t.Fatalf("resolve lost fields: %#v, %v", resolved, err)
	}
	downloaded, digest, err := client.Artifact(context.Background(), artifact.Package, artifact.Version)
	if err != nil || digest != artifact.ArtifactChecksum || !reflect.DeepEqual(downloaded, archive) {
		t.Fatalf("artifact mismatch: digest=%q err=%v", digest, err)
	}
}
