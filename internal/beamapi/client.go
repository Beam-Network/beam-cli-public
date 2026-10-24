package beamapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/auth"
)

type ErrorKind string

const (
	ErrUnavailable ErrorKind = "unavailable"
	ErrAuth        ErrorKind = "auth"
	ErrResponse    ErrorKind = "response"
)

type Error struct {
	Kind   ErrorKind
	Status int
	Cause  error
}

func (e *Error) Error() string {
	switch e.Kind {
	case ErrUnavailable:
		return "Beam API is temporarily unavailable"
	case ErrAuth:
		return "Beam authentication is invalid or expired"
	default:
		return fmt.Sprintf("Beam API request failed (HTTP %d)", e.Status)
	}
}

func (e *Error) Unwrap() error { return e.Cause }

type TokenSource interface {
	AccessToken(context.Context, bool) (string, error)
	Expire() error
}

type Client struct {
	BaseURL    string
	Tokens     TokenSource
	HTTPClient *http.Client
}

type Me struct {
	Type        string `json:"type,omitempty"`
	ExpiresAt   int64  `json:"exp,omitempty"`
	AccountType string `json:"accountType,omitempty"`
	User        struct {
		ID                string  `json:"id"`
		Name              string  `json:"name"`
		Email             string  `json:"email"`
		Image             *string `json:"image"`
		WalletAddress     *string `json:"walletAddress"`
		PlatformRole      string  `json:"platformRole"`
		RestrictionStatus string  `json:"restrictionStatus"`
	} `json:"user"`
}

type Organizations struct {
	Organizations []struct {
		ID                string `json:"id"`
		PublicID          string `json:"publicId"`
		Name              string `json:"name"`
		Slug              string `json:"slug"`
		Role              string `json:"role"`
		IsDefault         bool   `json:"isDefault"`
		RestrictionStatus string `json:"restrictionStatus"`
		Tier              string `json:"tier"`
	} `json:"organizations"`
}

func (c Client) Context(ctx context.Context) (*auth.User, []auth.Organization, error) {
	var me Me
	if err := c.Get(ctx, "/api/me", &me); err != nil {
		return nil, nil, err
	}
	var response Organizations
	if err := c.Get(ctx, "/api/organizations", &response); err != nil {
		return nil, nil, err
	}
	user := &auth.User{
		ID:                me.User.ID,
		Name:              me.User.Name,
		Email:             me.User.Email,
		Image:             me.User.Image,
		WalletAddress:     me.User.WalletAddress,
		PlatformRole:      me.User.PlatformRole,
		AccountType:       me.AccountType,
		RestrictionStatus: me.User.RestrictionStatus,
	}
	organizations := make([]auth.Organization, 0, len(response.Organizations))
	for _, organization := range response.Organizations {
		organizations = append(organizations, auth.Organization{
			ID:                organization.ID,
			PublicID:          organization.PublicID,
			Name:              organization.Name,
			Slug:              organization.Slug,
			Role:              organization.Role,
			IsDefault:         organization.IsDefault,
			RestrictionStatus: organization.RestrictionStatus,
			Tier:              organization.Tier,
		})
	}
	return user, organizations, nil
}

func (c Client) Get(ctx context.Context, path string, out any) error {
	return c.get(ctx, path, out, false, "")
}

// GetForOrganization is Get with an organization named, for routes that serve
// one organization at a time rather than everything the caller can see.
func (c Client) GetForOrganization(ctx context.Context, path, organizationID string, out any) error {
	return c.get(ctx, path, out, false, organizationID)
}

func (c Client) get(ctx context.Context, path string, out any, refreshed bool, organizationID string) error {
	if c.Tokens == nil {
		return &Error{Kind: ErrAuth, Status: http.StatusUnauthorized, Cause: auth.ErrNoSession}
	}
	token, err := c.Tokens.AccessToken(ctx, refreshed)
	if err != nil {
		var oauthErr *auth.OAuthError
		if errors.As(err, &oauthErr) && oauthErr.Retryable {
			return &Error{Kind: ErrUnavailable, Status: http.StatusServiceUnavailable, Cause: err}
		}
		return &Error{Kind: ErrAuth, Status: http.StatusUnauthorized, Cause: err}
	}
	base, err := url.Parse(c.BaseURL)
	if err != nil {
		return fmt.Errorf("invalid Beam API URL: %w", err)
	}
	endpoint, err := base.Parse(path)
	if err != nil {
		return fmt.Errorf("build Beam API URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if organizationID != "" {
		req.Header.Set("X-Organization-Id", organizationID)
	}
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return &Error{Kind: ErrUnavailable, Status: http.StatusServiceUnavailable, Cause: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized && !refreshed {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return c.get(ctx, path, out, true, organizationID)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		_ = c.Tokens.Expire()
		return &Error{Kind: ErrAuth, Status: resp.StatusCode}
	}
	if resp.StatusCode >= 500 {
		return &Error{Kind: ErrUnavailable, Status: resp.StatusCode}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &Error{Kind: ErrResponse, Status: resp.StatusCode}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return &Error{Kind: ErrResponse, Status: resp.StatusCode, Cause: err}
	}
	return nil
}
