package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// decodeMultipartPublish reads the two-part publish body defined by the
// Registry contract: a "metadata" JSON field and an "artifact" binary part.
func decodeMultipartPublish(t *testing.T, r *http.Request) (PublishRequest, []byte) {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		t.Errorf("content type = %q", r.Header.Get("Content-Type"))
		return PublishRequest{}, nil
	}
	reader := multipart.NewReader(r.Body, params["boundary"])
	var request PublishRequest
	var content []byte
	var names []string
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Error(err)
			return PublishRequest{}, nil
		}
		names = append(names, part.FormName())
		data, err := io.ReadAll(part)
		if err != nil {
			t.Error(err)
			return PublishRequest{}, nil
		}
		switch part.FormName() {
		case "metadata":
			var raw map[string]any
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Errorf("metadata is not JSON: %v", err)
			}
			for _, field := range []string{"trustLevel", "validationStatus", "publishedBy"} {
				if _, ok := raw[field]; ok {
					t.Errorf("metadata still claims %s", field)
				}
			}
			if artifact, _ := raw["artifact"].(map[string]any); artifact["contentBase64"] != nil {
				t.Error("metadata still inlines the archive")
			}
			if err := json.Unmarshal(data, &request); err != nil {
				t.Error(err)
			}
		case "artifact":
			content = data
		}
	}
	if strings.Join(names, ",") != "metadata,artifact" {
		t.Errorf("parts = %v, want metadata then artifact", names)
	}
	return request, content
}

func TestPublishSendsMetadataAndBinaryArtifactAsMultipart(t *testing.T) {
	archive := []byte{0x1f, 0x8b, 0x00, 0xff, 'b', 'e', 'a', 'm'}
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		request, content := decodeMultipartPublish(t, r)
		if string(content) != string(archive) || request.Artifact.Checksum != "sha256:abc" || request.Artifact.SizeBytes != int64(len(archive)) {
			t.Errorf("publish = %#v content=%q", request, content)
		}
		if request.Provenance["source"] != "beam-cli" || request.DistTags[0] != "next" {
			t.Errorf("metadata lost fields: %#v", request)
		}
		_ = json.NewEncoder(w).Encode(PublishResponse{Version: Version{ArtifactChecksum: request.Artifact.Checksum}})
	}))
	defer server.Close()
	client := Client{BaseURL: server.URL, Token: "token"}
	result, err := client.Publish(context.Background(), "@beam/example", PublishRequest{
		Manifest:   map[string]any{"name": "@beam/example"},
		Artifact:   PublishArtifact{Checksum: "sha256:abc", SizeBytes: int64(len(archive)), MediaType: "application/gzip"},
		DistTags:   []string{"next"},
		Provenance: map[string]any{"source": "beam-cli"},
	}, archive)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || result.Version.ArtifactChecksum != "sha256:abc" {
		t.Fatalf("requests=%d result=%#v", requests, result)
	}
}

func TestPublishFallsBackToJSONOnceWhenMultipartIsUnsupported(t *testing.T) {
	archive := []byte("archive-bytes")
	var contentTypes []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentTypes = append(contentTypes, r.Header.Get("Content-Type"))
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			// What Fastify answers when no multipart parser is registered.
			w.WriteHeader(http.StatusUnsupportedMediaType)
			_, _ = io.WriteString(w, `{"error":"FST_ERR_CTP_INVALID_MEDIA_TYPE","message":"Unsupported Media Type: multipart/form-data"}`)
			return
		}
		var raw map[string]any
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Error(err)
			return
		}
		for _, field := range []string{"trustLevel", "validationStatus", "publishedBy"} {
			if _, ok := raw[field]; ok {
				t.Errorf("legacy body still claims %s", field)
			}
		}
		artifact, _ := raw["artifact"].(map[string]any)
		encoded, _ := artifact["contentBase64"].(string)
		content, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || string(content) != string(archive) {
			t.Errorf("legacy artifact = %q err=%v", content, err)
		}
		_ = json.NewEncoder(w).Encode(PublishResponse{})
	}))
	defer server.Close()
	_, err := (Client{BaseURL: server.URL}).Publish(context.Background(), "@beam/example", PublishRequest{
		Artifact: PublishArtifact{Checksum: "sha256:abc", SizeBytes: int64(len(archive))},
	}, archive)
	if err != nil {
		t.Fatal(err)
	}
	if len(contentTypes) != 2 || !strings.HasPrefix(contentTypes[0], "multipart/form-data") || contentTypes[1] != "application/json" {
		t.Fatalf("content types = %q", contentTypes)
	}
}

func TestPublishDoesNotFallBackOnOtherRejections(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_manifest","message":"Manifest version is invalid."}`)
	}))
	defer server.Close()
	_, err := (Client{BaseURL: server.URL}).Publish(context.Background(), "@beam/example", PublishRequest{}, []byte("archive"))
	if err == nil || err.Error() != "Manifest version is invalid." || requests != 1 {
		t.Fatalf("requests=%d err=%v", requests, err)
	}
}

func TestMultipartPublishIsResentWholeAfterTokenRefresh(t *testing.T) {
	var authorizations []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		if len(authorizations) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if _, content := decodeMultipartPublish(t, r); string(content) != "archive" {
			t.Errorf("retried artifact = %q", content)
		}
		_ = json.NewEncoder(w).Encode(PublishResponse{})
	}))
	defer server.Close()
	_, err := (Client{BaseURL: server.URL, Tokens: &testTokenSource{}}).Publish(context.Background(), "@beam/example", PublishRequest{}, []byte("archive"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(authorizations, ",") != "Bearer initial-token,Bearer refreshed-token" {
		t.Fatalf("authorizations = %q", authorizations)
	}
}

func TestVersionArtifactPrefersSignedURLAndNeverSendsTheTokenElsewhere(t *testing.T) {
	var storageAuthorization, registryHits string
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		storageAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Digest", "sha256:signed")
		_, _ = io.WriteString(w, "signed-bytes")
	}))
	defer storage.Close()
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registryHits += r.URL.Path + "|" + r.Header.Get("Authorization") + ";"
		_, _ = io.WriteString(w, "registry-bytes")
	}))
	defer registry.Close()
	client := Client{BaseURL: registry.URL + "/registry", Token: "secret-token"}

	signed := storage.URL + "/registry/v1/artifacts/sha256/abc?exp=1&sig=x"
	data, digest, err := client.VersionArtifact(context.Background(), "@beam/example", "1.0.0", &signed)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "signed-bytes" || digest != "sha256:signed" || storageAuthorization != "" || registryHits != "" {
		t.Fatalf("data=%q digest=%q storage auth=%q registry=%q", data, digest, storageAuthorization, registryHits)
	}

	sameOrigin := registry.URL + "/registry/v1/artifacts/sha256/abc?exp=1&sig=x"
	if _, _, err := client.VersionArtifact(context.Background(), "@beam/example", "1.0.0", &sameOrigin); err != nil {
		t.Fatal(err)
	}
	if registryHits != "/registry/v1/artifacts/sha256/abc|Bearer secret-token;" {
		t.Fatalf("registry hits = %q", registryHits)
	}
}

func TestVersionArtifactFallsBackToTheVersionedEndpoint(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.Contains(r.URL.Path, "/artifacts/sha256/") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = io.WriteString(w, "versioned-bytes")
	}))
	defer server.Close()
	client := Client{BaseURL: server.URL}
	expired := server.URL + "/v1/artifacts/sha256/abc?exp=1&sig=x"
	for _, artifactURL := range []*string{nil, &expired} {
		paths = nil
		data, _, err := client.VersionArtifact(context.Background(), "@beam/example", "1.0.0", artifactURL)
		if err != nil || string(data) != "versioned-bytes" {
			t.Fatalf("data=%q err=%v", data, err)
		}
		if paths[len(paths)-1] != "/v1/packages/@beam/example/versions/1.0.0/artifact" {
			t.Fatalf("paths = %q", paths)
		}
	}
}

func TestRegistryRedirectOffOriginDropsTheToken(t *testing.T) {
	var elsewhere string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "bytes")
	}))
	defer other.Close()
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/blob", http.StatusFound)
	}))
	defer registry.Close()
	data, _, err := (Client{BaseURL: registry.URL, Token: "secret-token"}).Artifact(context.Background(), "@beam/example", "1.0.0")
	if err != nil || string(data) != "bytes" {
		t.Fatalf("data=%q err=%v", data, err)
	}
	if elsewhere != "" {
		t.Fatalf("token forwarded off-origin: %q", elsewhere)
	}
}
