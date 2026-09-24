package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

type testTokenSource struct {
	forces  []bool
	expired int
}

func (s *testTokenSource) AccessToken(_ context.Context, force bool) (string, error) {
	s.forces = append(s.forces, force)
	if force {
		return "refreshed-token", nil
	}
	return "initial-token", nil
}

func (s *testTokenSource) Expire() error {
	s.expired++
	return nil
}

func TestVersionsUsesRegistryContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/packages/@beam/example/versions" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatal("missing bearer token")
		}
		_ = json.NewEncoder(w).Encode(VersionsResponse{
			Versions: []Version{{PackageName: "@beam/example", Version: "1.2.3"}},
		})
	}))
	defer server.Close()
	client := Client{BaseURL: server.URL, Token: "test-token"}
	result, err := client.Versions(context.Background(), "@beam/example")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Versions) != 1 || result.Versions[0].Version != "1.2.3" {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestPublishSendsArtifactAndMapsAuthenticationError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Authentication expired."}`))
	}))
	defer server.Close()
	client := Client{BaseURL: server.URL}
	_, err := client.Publish(context.Background(), "@beam/example", PublishRequest{})
	registryErr, ok := err.(*Error)
	if !ok || registryErr.Kind != ErrAuth {
		t.Fatalf("error = %#v", err)
	}
	if registryErr.Error() != "Authentication expired." {
		t.Fatalf("message = %q", registryErr.Error())
	}
}

// The HTTP client must not impose its own manifest contract. Validation belongs
// to the Registry; these extra fields represent the versioned manifest being
// transported, not a second schema maintained by the CLI.
func TestPublishAndResolvePreserveVersionedManifestAndArtifactChecksum(t *testing.T) {
	for _, apiVersion := range []string{"workflow-actions/v1", "workflow-actions/v2"} {
		t.Run(apiVersion, func(t *testing.T) {
			manifest := map[string]any{
				"name": "@example/transport", "version": "2.0.0", "apiVersion": apiVersion,
				"description": "Research & development <safe>",
				"contracts":   map[string]any{"computation": map[string]any{"semanticId": "example.normalize/v1"}},
				"execution":   map[string]any{"requiredCapabilities": []any{"action-artifact-ports/v1"}},
			}
			checksum := "sha256:0123456789abcdef"
			var published PublishRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/v1/packages/@example/transport/versions":
					if err := json.NewDecoder(r.Body).Decode(&published); err != nil {
						t.Errorf("decode publish: %v", err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					_ = json.NewEncoder(w).Encode(PublishResponse{Version: Version{
						Manifest: published.Manifest, ArtifactChecksum: published.Artifact.Checksum,
						ArtifactSize: published.Artifact.SizeBytes,
					}})
				case r.Method == http.MethodGet && r.URL.Path == "/v1/resolve/@example/transport" && r.URL.Query().Get("range") == "2.0.0":
					_ = json.NewEncoder(w).Encode(ResolveResponse{Version: Version{
						Manifest: published.Manifest, ArtifactChecksum: published.Artifact.Checksum,
						ArtifactSize: published.Artifact.SizeBytes,
					}, ResolvedVersion: "2.0.0"})
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			client := Client{BaseURL: server.URL}
			artifact := PublishArtifact{ContentBase64: "YXJjaGl2ZQ==", Checksum: checksum, SizeBytes: 7, MediaType: "application/gzip"}
			result, err := client.Publish(context.Background(), "@example/transport", PublishRequest{Manifest: manifest, Artifact: artifact})
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := client.Resolve(context.Background(), "@example/transport", "2.0.0")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(published.Manifest, manifest) || published.Artifact != artifact {
				t.Fatalf("publish changed fields: manifest=%#v artifact=%#v", published.Manifest, published.Artifact)
			}
			for _, version := range []Version{result.Version, resolved.Version} {
				if !reflect.DeepEqual(version.Manifest, manifest) || version.ArtifactChecksum != checksum || version.ArtifactSize != 7 {
					t.Fatalf("response changed fields: %#v", version)
				}
			}
		})
	}
}

func TestUnavailableRegistryHasStableKind(t *testing.T) {
	client := Client{BaseURL: "http://127.0.0.1:1"}
	_, err := client.Resolve(context.Background(), "@beam/example", "latest")
	registryErr, ok := err.(*Error)
	if !ok || registryErr.Kind != ErrUnavailable {
		t.Fatalf("error = %#v", err)
	}
}

func TestAuthenticatedRegistryRequestRefreshesAndRetriesOnceOn401(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			if r.Header.Get("Authorization") != "Bearer initial-token" {
				t.Fatalf("first authorization=%q", r.Header.Get("Authorization"))
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer refreshed-token" {
			t.Fatalf("retry authorization=%q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(VersionsResponse{})
	}))
	defer server.Close()
	tokens := &testTokenSource{}
	_, err := (Client{BaseURL: server.URL, Tokens: tokens}).Versions(context.Background(), "@beam/example")
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || len(tokens.forces) != 2 || tokens.forces[0] || !tokens.forces[1] || tokens.expired != 0 {
		t.Fatalf("requests=%d forces=%v expired=%d", requests, tokens.forces, tokens.expired)
	}
}

func TestBasePathIsPreservedOnEveryRequest(t *testing.T) {
	var escaped, query string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		escaped, query = r.URL.EscapedPath(), r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(PackagesResponse{})
	}))
	defer server.Close()

	client := Client{BaseURL: server.URL + "/registry", Token: "test-token"}
	if _, err := client.Search(context.Background(), "archive"); err != nil {
		t.Fatal(err)
	}
	if escaped != "/registry/v1/packages" {
		t.Fatalf("path = %q, want the /registry base path kept", escaped)
	}
	if query != "search=archive" {
		t.Fatalf("query = %q", query)
	}

	// url.PathEscape leaves "@" alone, so the scope reaches the server literally.
	if _, err := client.Package(context.Background(), "@beam/example"); err != nil {
		t.Fatal(err)
	}
	if escaped != "/registry/v1/packages/@beam/example" {
		t.Fatalf("path = %q, want the base path kept and the scope unchanged", escaped)
	}
}

func TestArtifactDownloadKeepsBasePath(t *testing.T) {
	var escaped string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		escaped = r.URL.EscapedPath()
		_, _ = w.Write([]byte("artifact-bytes"))
	}))
	defer server.Close()

	client := Client{BaseURL: server.URL + "/registry", Token: "test-token"}
	data, _, err := client.Artifact(context.Background(), "@beam/example", "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "artifact-bytes" {
		t.Fatalf("data = %q", data)
	}
	if escaped != "/registry/v1/packages/@beam/example/versions/1.2.3/artifact" {
		t.Fatalf("path = %q", escaped)
	}
}

func TestBaseURLShapesResolveToOnePath(t *testing.T) {
	for _, testCase := range []struct{ name, suffix, want string }{
		{"no base path", "", "/v1/packages"},
		{"base path", "/registry", "/registry/v1/packages"},
		{"trailing slash", "/registry/", "/registry/v1/packages"},
		{"nested base path", "/api/registry", "/api/registry/v1/packages"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var got string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.URL.EscapedPath()
				_ = json.NewEncoder(w).Encode(PackagesResponse{})
			}))
			defer server.Close()

			client := Client{BaseURL: server.URL + testCase.suffix, Token: "test-token"}
			if _, err := client.Search(context.Background(), ""); err != nil {
				t.Fatal(err)
			}
			if got != testCase.want {
				t.Fatalf("path = %q, want %q", got, testCase.want)
			}
		})
	}
}
