package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

var ErrNoSession = errors.New("Beam login is required")

// Session owns the in-memory access token and the rotating durable refresh
// token. Callers never read or write either token directly.
type Session struct {
	Client DeviceClient
	Store  Store

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
}

func (s *Session) AcceptLogin(token Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acceptLocked(token, Credentials{})
}

func (s *Session) AccessToken(ctx context.Context, forceRefresh bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !forceRefresh && s.accessToken != "" && time.Until(s.expiresAt) > 30*time.Second {
		return s.accessToken, nil
	}
	credentials, err := s.Store.Load()
	if err != nil {
		return "", err
	}
	if !credentials.Active() {
		return "", ErrNoSession
	}
	token, err := s.Client.Refresh(ctx, credentials.RefreshToken)
	if err != nil {
		var oauthErr *OAuthError
		if errors.As(err, &oauthErr) && !oauthErr.Retryable && oauthErr.Status < http.StatusInternalServerError {
			_ = s.Store.Delete()
			s.clearLocked()
		}
		return "", err
	}
	if err := s.acceptLocked(token, credentials); err != nil {
		return "", err
	}
	return s.accessToken, nil
}

func (s *Session) HasSession() (bool, error) {
	credentials, err := s.Store.Load()
	return credentials.Active(), err
}

func (s *Session) Expire() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clearLocked()
	return s.Store.Delete()
}

// Logout always attempts local deletion, even when revocation cannot reach
// Beam Auth. The two errors are separate so the command can report local
// success without pretending server-side revocation was confirmed.
func (s *Session) Logout(ctx context.Context) (revokeErr, deleteErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	credentials, loadErr := s.Store.Load()
	if loadErr == nil && credentials.Active() {
		revokeErr = s.Client.Revoke(ctx, credentials.RefreshToken)
	} else if loadErr != nil {
		revokeErr = fmt.Errorf("read credentials before revocation: %w", loadErr)
	}
	s.clearLocked()
	deleteErr = s.Store.Delete()
	return revokeErr, deleteErr
}

func (s *Session) acceptLocked(token Token, current Credentials) error {
	current.Version = CredentialsVersion
	current.RefreshToken = token.RefreshToken
	if err := s.Store.Save(current); err != nil {
		// The previous refresh generation was consumed by the server. If its
		// replacement cannot be committed, erase all local state rather than
		// ever attempting to reuse the old token.
		_ = s.Store.Delete()
		s.clearLocked()
		return fmt.Errorf("save rotated refresh token: %w", err)
	}
	s.accessToken = token.AccessToken
	s.expiresAt = token.ExpiresAt
	return nil
}

func (s *Session) clearLocked() {
	s.accessToken = ""
	s.expiresAt = time.Time{}
}
