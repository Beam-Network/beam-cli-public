package beamapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/auth"
)

type fakeTokens struct {
	forces  []bool
	expired int
}

type temporaryTokens struct{}

func (temporaryTokens) AccessToken(context.Context, bool) (string, error) {
	return "", &auth.OAuthError{Code: "temporary_auth_error", Status: 503, Retryable: true}
}

func (temporaryTokens) Expire() error { return nil }

func (f *fakeTokens) AccessToken(_ context.Context, force bool) (string, error) {
	f.forces = append(f.forces, force)
	if force {
		return "refreshed-access", nil
	}
	return "initial-access", nil
}

func (f *fakeTokens) Expire() error {
	f.expired++
	return nil
}

func TestClientRetriesExactlyOnceAfter401(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			if r.Header.Get("Authorization") != "Bearer initial-access" {
				t.Fatalf("first authorization=%q", r.Header.Get("Authorization"))
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer refreshed-access" {
			t.Fatalf("retry authorization=%q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer server.Close()
	tokens := &fakeTokens{}
	var result map[string]bool
	if err := (Client{BaseURL: server.URL, Tokens: tokens}).Get(context.Background(), "/api/me", &result); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || len(tokens.forces) != 2 || tokens.forces[0] || !tokens.forces[1] || tokens.expired != 0 {
		t.Fatalf("requests=%d forces=%v expired=%d", requests, tokens.forces, tokens.expired)
	}
}

func TestClientExpiresSessionAfterSecond401(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	tokens := &fakeTokens{}
	err := (Client{BaseURL: server.URL, Tokens: tokens}).Get(context.Background(), "/api/me", &map[string]any{})
	if err == nil || requests != 2 || tokens.expired != 1 {
		t.Fatalf("err=%v requests=%d expired=%d", err, requests, tokens.expired)
	}
}

func TestClientPreservesSessionOn503(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	tokens := &fakeTokens{}
	err := (Client{BaseURL: server.URL, Tokens: tokens}).Get(context.Background(), "/api/me", &map[string]any{})
	if err == nil || tokens.expired != 0 {
		t.Fatalf("err=%v expired=%d", err, tokens.expired)
	}
}

func TestClientMapsTemporaryRefreshFailureAsUnavailable(t *testing.T) {
	err := (Client{BaseURL: "https://api.example", Tokens: temporaryTokens{}}).Get(
		context.Background(), "/api/me", &map[string]any{},
	)
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Kind != ErrUnavailable {
		t.Fatalf("err=%#v", err)
	}
}

func TestContextLoadsMeAndOrganizations(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/me":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"accountType": "user",
				"user": map[string]any{
					"id": "user-1", "name": "Beam User", "email": "beam@example.com", "platformRole": "USER", "restrictionStatus": "LIMITED",
				},
			})
		case "/api/organizations":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"organizations": []map[string]any{{
					"id": "org-1", "publicId": "public-1", "name": "Beam", "isDefault": true, "role": "OWNER",
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	user, organizations, err := (Client{BaseURL: server.URL, Tokens: &fakeTokens{}}).Context(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if user.Email != "beam@example.com" || user.RestrictionStatus != "LIMITED" || len(organizations) != 1 || organizations[0].ID != "org-1" {
		t.Fatalf("user=%#v organizations=%#v", user, organizations)
	}
}
