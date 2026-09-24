package auth

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/version"
	"github.com/zalando/go-keyring"
)

type memoryKeyring struct {
	token     string
	setErr    error
	getErr    error
	deleteErr error
	service   string
	user      string
}

func (m *memoryKeyring) Get(service, user string) (string, error) {
	m.service, m.user = service, user
	if m.getErr != nil {
		return "", m.getErr
	}
	if m.token == "" {
		return "", keyring.ErrNotFound
	}
	return m.token, nil
}

func (m *memoryKeyring) Set(service, user, token string) error {
	m.service, m.user = service, user
	if m.setErr != nil {
		return m.setErr
	}
	m.token = token
	return nil
}

func (m *memoryKeyring) Delete(service, user string) error {
	m.service, m.user = service, user
	if m.deleteErr != nil {
		return m.deleteErr
	}
	if m.token == "" {
		return keyring.ErrNotFound
	}
	m.token = ""
	return nil
}

func TestPullRequestStoreUsesIsolatedKeyringService(t *testing.T) {
	original := version.Version
	version.Version = "v0.0.0-pr.27.cli.agent"
	t.Cleanup(func() { version.Version = original })

	store := NewStore(filepath.Join(t.TempDir(), "credentials.json"))
	if store.keyringService != "Beam CLI beam-pr-27" || store.keyringUser != keyringUser {
		t.Fatalf("keyring identity = %q/%q", store.keyringService, store.keyringUser)
	}
}

func TestStorePrefersKeyringAndNeverPersistsAccessToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beam", "credentials.json")
	secure := &memoryKeyring{}
	store := Store{Path: path, keyring: secure}
	want := Credentials{
		RefreshToken: "refresh-secret",
		User:         &User{Email: "beam@example.com"},
	}
	if err := store.Save(want); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "refresh-secret") || strings.Contains(string(data), "access-secret") {
		t.Fatalf("token persisted in metadata: %s", data)
	}
	if secure.token != "refresh-secret" {
		t.Fatalf("keyring token=%q", secure.token)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.RefreshToken != "refresh-secret" || got.User.Email != "beam@example.com" {
		t.Fatalf("credentials=%#v", got)
	}
}

func TestStoreFallsBackToAtomicRestrictedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beam", "credentials.json")
	store := Store{Path: path, keyring: &memoryKeyring{setErr: errors.New("unavailable")}}
	if err := store.Save(Credentials{RefreshToken: "generation-one"}); err != nil {
		t.Fatal(err)
	}
	credentials, err := store.Load()
	if err != nil || credentials.RefreshToken != "generation-one" {
		t.Fatalf("credentials=%#v err=%v", credentials, err)
	}
	if err := store.Save(Credentials{RefreshToken: "generation-two"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "generation-one") || !strings.Contains(string(data), "generation-two") {
		t.Fatalf("non-atomic rotation result: %s", data)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("permissions=%o", info.Mode().Perm())
		}
	}
}

func TestStoreIgnoresAndRemovesPhaseOneCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, []byte(`{
  "version": 1,
  "access_token": "legacy-secret",
  "sessions": {"registry": {"access_token": "legacy-session"}},
  "user": {"email": "beam@example.com"}
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	credentials, err := (Store{Path: path}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Active() {
		t.Fatalf("legacy session accepted: %#v", credentials)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy credential file still exists: %v", err)
	}
}

func TestStoreRotatesKeyringBeforeReturningSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	secure := &memoryKeyring{}
	store := Store{Path: path, keyring: secure}
	if err := store.Save(Credentials{RefreshToken: "old-refresh"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(Credentials{RefreshToken: "new-refresh"}); err != nil {
		t.Fatal(err)
	}
	if secure.token != "new-refresh" {
		t.Fatalf("keyring token=%q", secure.token)
	}
	loaded, err := store.Load()
	if err != nil || loaded.RefreshToken != "new-refresh" {
		t.Fatalf("loaded=%#v err=%v", loaded, err)
	}
}

func TestStoreDeleteClearsFileAndKeyring(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	secure := &memoryKeyring{}
	store := Store{Path: path, keyring: secure}
	if err := store.Save(Credentials{RefreshToken: "refresh"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(); err != nil {
		t.Fatal(err)
	}
	if secure.token != "" {
		t.Fatal("keyring token remains")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credential file remains: %v", err)
	}
}

func TestStoreRejectsNewerCredentialVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, []byte(`{"version":99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Store{Path: path}).Load(); err == nil {
		t.Fatal("expected unsupported version error")
	}
}
