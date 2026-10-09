package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/Beam-Network/beam-cli-public/internal/version"
	"github.com/zalando/go-keyring"
)

const (
	CredentialsVersion = 2
	keyringService     = "Beam CLI"
	keyringUser        = "oauth-refresh-token"
	storeKeyring       = "keyring"
	storeFile          = "file"
)

type User struct {
	ID                string  `json:"id,omitempty"`
	Name              string  `json:"name,omitempty"`
	Email             string  `json:"email,omitempty"`
	Image             *string `json:"image,omitempty"`
	WalletAddress     *string `json:"wallet_address,omitempty"`
	PlatformRole      string  `json:"platform_role,omitempty"`
	AccountType       string  `json:"account_type,omitempty"`
	RestrictionStatus string  `json:"restriction_status,omitempty"`
}

type Organization struct {
	ID                string `json:"id"`
	PublicID          string `json:"public_id,omitempty"`
	Name              string `json:"name,omitempty"`
	Slug              string `json:"slug,omitempty"`
	Role              string `json:"role,omitempty"`
	IsDefault         bool   `json:"is_default,omitempty"`
	RestrictionStatus string `json:"restriction_status,omitempty"`
	Tier              string `json:"tier,omitempty"`
}

// Credentials contains the durable portion of an OAuth session. Access tokens
// are deliberately absent: they remain in process memory only.
type Credentials struct {
	Version       int            `json:"version"`
	RefreshToken  string         `json:"refresh_token,omitempty"`
	RefreshStore  string         `json:"refresh_token_store,omitempty"`
	User          *User          `json:"user,omitempty"`
	Organizations []Organization `json:"organizations,omitempty"`
}

func (c Credentials) Active() bool { return c.RefreshToken != "" }

type credentialKeyring interface {
	Get(service, user string) (string, error)
	Set(service, user, password string) error
	Delete(service, user string) error
}

type systemKeyring struct{}

func (systemKeyring) Get(service, user string) (string, error) {
	return keyring.Get(service, user)
}

func (systemKeyring) Set(service, user, password string) error {
	return keyring.Set(service, user, password)
}

func (systemKeyring) Delete(service, user string) error {
	return keyring.Delete(service, user)
}

type Store struct {
	Path           string
	keyring        credentialKeyring
	keyringService string
	keyringUser    string
}

// NewStore enables the operating-system credential store. A Store literal is
// intentionally file-only, which keeps unit tests and library callers isolated
// from a developer's real keychain.
func NewStore(path string) Store {
	service := keyringService
	user := keyringUser
	if _, ok := version.PullRequestNumber(); ok {
		service += " " + version.StateNamespace()
	}
	return Store{Path: path, keyring: systemKeyring{}, keyringService: service, keyringUser: user}
}

func (s Store) keyringIdentity() (string, string) {
	service := s.keyringService
	if service == "" {
		service = keyringService
	}
	user := s.keyringUser
	if user == "" {
		user = keyringUser
	}
	return service, user
}

func (s Store) Load() (Credentials, error) {
	credentials, exists, err := s.loadFile()
	if err != nil {
		return Credentials{}, err
	}
	if exists && credentials.Version < CredentialsVersion {
		// Phase 1 access-token sessions are intentionally invalid. Removing the
		// file prevents any accidental fallback to the retired HS256 flow.
		if err := os.Remove(s.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return Credentials{}, fmt.Errorf("remove legacy credentials: %w", err)
		}
		return Credentials{}, nil
	}
	if credentials.Version > CredentialsVersion {
		return Credentials{}, fmt.Errorf(
			"credentials version %d is newer than supported version %d",
			credentials.Version,
			CredentialsVersion,
		)
	}

	if credentials.RefreshStore == storeFile {
		return credentials, nil
	}
	if s.keyring != nil && (credentials.RefreshStore == storeKeyring || !exists) {
		service, user := s.keyringIdentity()
		token, getErr := s.keyring.Get(service, user)
		switch {
		case getErr == nil:
			credentials.Version = CredentialsVersion
			credentials.RefreshStore = storeKeyring
			credentials.RefreshToken = token
			return credentials, nil
		case errors.Is(getErr, keyring.ErrNotFound):
			return credentials, nil
		case credentials.RefreshStore == storeKeyring:
			return Credentials{}, fmt.Errorf("read refresh token from credential store: %w", getErr)
		}
	}
	return credentials, nil
}

func (s Store) Save(credentials Credentials) error {
	if credentials.RefreshToken == "" {
		return fmt.Errorf("credentials contain no refresh token")
	}
	if err := s.secureDirectory(); err != nil {
		return err
	}

	current, exists, err := s.loadFile()
	if err != nil {
		return err
	}
	mode := current.RefreshStore
	if !exists || current.Version < CredentialsVersion || mode == "" {
		mode = storeKeyring
	}

	credentials.Version = CredentialsVersion
	keyringBecameUnavailable := false
	if mode == storeKeyring && s.keyring != nil {
		service, user := s.keyringIdentity()
		if err := s.keyring.Set(service, user, credentials.RefreshToken); err == nil {
			credentials.RefreshStore = storeKeyring
			credentials.RefreshToken = ""
			return s.writeFile(credentials)
		}
		// If the OS store is unavailable, atomically switch to the protected
		// file. The mode marker ensures a stale keyring generation is ignored.
		keyringBecameUnavailable = true
	}
	credentials.RefreshStore = storeFile
	if err := s.writeFile(credentials); err != nil {
		return err
	}
	if keyringBecameUnavailable {
		// The old keyring generation is no longer authoritative. Remove it
		// best-effort after the file-mode marker and new token are durable.
		service, user := s.keyringIdentity()
		_ = s.keyring.Delete(service, user)
	}
	return nil
}

func (s Store) UpdateContext(user *User, organizations []Organization) error {
	credentials, err := s.Load()
	if err != nil {
		return err
	}
	if !credentials.Active() {
		return fmt.Errorf("no active OAuth session")
	}
	credentials.User = user
	credentials.Organizations = organizations
	return s.Save(credentials)
}

func (s Store) Delete() error {
	credentials, _, loadErr := s.loadFile()
	fileErr := os.Remove(s.Path)
	if errors.Is(fileErr, os.ErrNotExist) {
		fileErr = nil
	}
	var keyringErr error
	if s.keyring != nil {
		service, user := s.keyringIdentity()
		keyringErr = s.keyring.Delete(service, user)
		if errors.Is(keyringErr, keyring.ErrNotFound) {
			keyringErr = nil
		}
		if credentials.RefreshStore == storeFile {
			// File mode can leave an unreachable stale keyring generation behind;
			// it is ignored during reads and deletion is best-effort.
			keyringErr = nil
		}
	}
	return errors.Join(loadErr, fileErr, keyringErr)
}

func (s Store) loadFile() (Credentials, bool, error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return Credentials{}, false, nil
	}
	if err != nil {
		return Credentials{}, false, fmt.Errorf("read credentials: %w", err)
	}
	var credentials Credentials
	if err := json.Unmarshal(data, &credentials); err != nil {
		return Credentials{}, false, fmt.Errorf("parse credentials: %w", err)
	}
	return credentials, true, nil
}

func (s Store) secureDirectory() error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return fmt.Errorf("create credentials directory: %w", err)
	}
	if err := os.Chmod(filepath.Dir(s.Path), 0o700); err != nil {
		return fmt.Errorf("secure credentials directory: %w", err)
	}
	return nil
}

func (s Store) writeFile(credentials Credentials) error {
	data, err := json.MarshalIndent(credentials, "", "  ")
	if err != nil {
		return fmt.Errorf("encode credentials: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(s.Path), ".credentials-*")
	if err != nil {
		return fmt.Errorf("create temporary credentials: %w", err)
	}
	tempPath := temp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("secure temporary credentials: %w", err)
	}
	if _, err := temp.Write(append(data, '\n')); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write temporary credentials: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync temporary credentials: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary credentials: %w", err)
	}
	if err := replaceFile(tempPath, s.Path); err != nil {
		return fmt.Errorf("replace credentials atomically: %w", err)
	}
	removeTemp = false
	return os.Chmod(s.Path, 0o600)
}

func Restrictive(path string) (bool, error) {
	if runtime.GOOS == "windows" {
		return true, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return info.Mode().Perm()&0o077 == 0, nil
}
