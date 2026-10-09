package command

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/auth"
	"github.com/Beam-Network/beam-cli-public/internal/config"
	"github.com/Beam-Network/beam-cli-public/internal/registry"
)

// TestOptionalRegistrySessionReturnsATrueNilInterface is the regression this
// package needs most. A nil *auth.Session assigned to registry.Client.Tokens
// leaves that interface field non-nil, so the client's "if c.Tokens != nil"
// guard passes and AccessToken runs on a nil receiver. Comparing the returned
// value against nil is exactly the check that used to succeed by accident.
func TestOptionalRegistrySessionReturnsATrueNilInterface(t *testing.T) {
	store := auth.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	tokens, err := optionalRegistrySession(config.Config{}, store)
	if err != nil {
		t.Fatal(err)
	}
	if tokens != nil {
		t.Fatalf("token source = %#v, want a nil interface", tokens)
	}
	// The interface must also be nil once it is stored on the client, which is
	// the field the panicking guard actually reads.
	if client := (registry.Client{Tokens: tokens}); client.Tokens != nil {
		t.Fatalf("client.Tokens = %#v, want nil", client.Tokens)
	}
}

// TestAnonymousRegistryReadSendsNoAuthorization drives the whole path that
// panicked: no credentials, a registry that answers, and a read that must
// complete and carry no bearer token.
func TestAnonymousRegistryReadSendsNoAuthorization(t *testing.T) {
	var sawAuthorization bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			sawAuthorization = true
		}
		_, _ = w.Write([]byte(`{"packages":[],"versions":[]}`))
	}))
	defer server.Close()

	store := auth.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	cfg := config.Config{RegistryURL: server.URL}
	client, err := registryReadClient(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Search(context.Background(), "transfer"); err != nil {
		t.Fatalf("anonymous search failed: %v", err)
	}
	if sawAuthorization {
		t.Error("anonymous read sent an Authorization header")
	}
}

// Every command that builds a registry client from optionalRegistrySession has
// to survive the no-credentials path. registry search, show and download went
// through registryReadClient; action version assigned Tokens directly.
func TestRegistryReadsSurviveMissingCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"packages":[],"versions":[],"package":{},"version":{}}`))
	}))
	defer server.Close()

	store := auth.Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	cfg := config.Config{RegistryURL: server.URL}
	tokens, err := optionalRegistrySession(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	client := registry.Client{BaseURL: cfg.RegistryURL, Tokens: tokens}
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"search":   func() error { _, err := client.Search(ctx, "x"); return err },
		"versions": func() error { _, err := client.Versions(ctx, "@beam/probe"); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); err != nil {
				t.Fatalf("%s with no credentials: %v", name, err)
			}
		})
	}
}
