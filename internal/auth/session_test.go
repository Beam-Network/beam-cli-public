package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestSessionRefreshesBeforeExpiryAndCommitsRotation(t *testing.T) {
	var refreshCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			http.NotFound(w, r)
			return
		}
		refreshCalls++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "new-access", "token_type": "Bearer", "expires_in": 900,
			"refresh_token": "new-refresh", "scope": Scope,
		})
	}))
	defer server.Close()
	store := Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	session := &Session{Client: DeviceClient{BaseURL: server.URL}, Store: store}
	if err := session.AcceptLogin(Token{
		AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(20 * time.Second), Scope: Scope,
	}); err != nil {
		t.Fatal(err)
	}
	access, err := session.AccessToken(context.Background(), false)
	if err != nil || access != "new-access" {
		t.Fatalf("access=%q err=%v", access, err)
	}
	credentials, err := store.Load()
	if err != nil || credentials.RefreshToken != "new-refresh" {
		t.Fatalf("credentials=%#v err=%v", credentials, err)
	}
	if refreshCalls != 1 {
		t.Fatalf("refresh calls=%d", refreshCalls)
	}
}

func TestRejectedRefreshExpiresSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
	}))
	defer server.Close()
	store := Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	if err := store.Save(Credentials{RefreshToken: "revoked-refresh"}); err != nil {
		t.Fatal(err)
	}
	session := &Session{Client: DeviceClient{BaseURL: server.URL}, Store: store}
	if _, err := session.AccessToken(context.Background(), false); err == nil {
		t.Fatal("expected refresh rejection")
	}
	credentials, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Active() {
		t.Fatal("rejected refresh token remains")
	}
}

func TestTemporaryRefreshFailurePreservesSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "server_error"})
	}))
	defer server.Close()
	store := Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	if err := store.Save(Credentials{RefreshToken: "keep-refresh"}); err != nil {
		t.Fatal(err)
	}
	session := &Session{Client: DeviceClient{BaseURL: server.URL}, Store: store}
	if _, err := session.AccessToken(context.Background(), false); err == nil {
		t.Fatal("expected refresh failure")
	}
	credentials, err := store.Load()
	if err != nil || credentials.RefreshToken != "keep-refresh" {
		t.Fatalf("temporary failure deleted session: %#v err=%v", credentials, err)
	}
}

func TestLogoutDeletesLocallyWhenRevocationFails(t *testing.T) {
	store := Store{Path: filepath.Join(t.TempDir(), "credentials.json")}
	if err := store.Save(Credentials{RefreshToken: "refresh"}); err != nil {
		t.Fatal(err)
	}
	session := &Session{
		Client: DeviceClient{BaseURL: "http://127.0.0.1:1"},
		Store:  store,
	}
	revokeErr, deleteErr := session.Logout(context.Background())
	if revokeErr == nil || deleteErr != nil {
		t.Fatalf("revokeErr=%v deleteErr=%v", revokeErr, deleteErr)
	}
	credentials, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Active() {
		t.Fatal("local credentials remain")
	}
}

func TestNoOAuthErrorContainsSubmittedTokens(t *testing.T) {
	secret := "must-never-appear"
	client := DeviceClient{BaseURL: "http://127.0.0.1:1"}
	_, err := client.Refresh(context.Background(), secret)
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, context.Canceled) || contains(err.Error(), secret) {
		t.Fatalf("unsafe error=%q", err)
	}
}

func contains(value, part string) bool {
	for index := 0; index+len(part) <= len(value); index++ {
		if value[index:index+len(part)] == part {
			return true
		}
	}
	return false
}
