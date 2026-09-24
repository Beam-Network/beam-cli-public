package beamapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrAPIKeyInvalid reports a key the Beam API does not recognise or has
// deactivated.
var ErrAPIKeyInvalid = errors.New("beam api key is not valid")

// APIKeyIdentity is what the Beam API knows about a key. A key belongs to
// exactly one organization, which is what decides the organization a room is
// created in, so the CLI resolves it before starting a billable room.
type APIKeyIdentity struct {
	KeyID          string
	OrganizationID string
}

type verifyAPIKeyResponse struct {
	KeyID          string `json:"keyId"`
	OrganizationID string `json:"organizationId"`
	Valid          bool   `json:"valid"`
	Error          string `json:"error"`
}

// VerifyAPIKey resolves the identity behind a Beam API key. The key
// authenticates the request itself, so no session is required.
func VerifyAPIKey(ctx context.Context, apiURL, apiKey string, httpClient *http.Client) (APIKeyIdentity, error) {
	base := strings.TrimSpace(apiURL)
	key := strings.TrimSpace(apiKey)
	if base == "" {
		return APIKeyIdentity{}, errors.New("no Beam API URL is configured")
	}
	if key == "" {
		return APIKeyIdentity{}, errors.New("no Beam API key was supplied")
	}
	endpoint, err := url.Parse(base)
	if err != nil {
		return APIKeyIdentity{}, fmt.Errorf("parse Beam API URL: %w", err)
	}
	endpoint = endpoint.JoinPath("api", "keys", "verify")
	payload, err := json.Marshal(map[string]string{"apiKey": key})
	if err != nil {
		return APIKeyIdentity{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return APIKeyIdentity{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	client := httpClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return APIKeyIdentity{}, err
	}
	defer func() { _ = response.Body.Close() }()
	var decoded verifyAPIKeyResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil && response.StatusCode == http.StatusOK {
		return APIKeyIdentity{}, fmt.Errorf("decode key verification: %w", err)
	}
	if response.StatusCode != http.StatusOK || !decoded.Valid || strings.TrimSpace(decoded.OrganizationID) == "" {
		return APIKeyIdentity{}, ErrAPIKeyInvalid
	}
	return APIKeyIdentity{KeyID: decoded.KeyID, OrganizationID: decoded.OrganizationID}, nil
}
