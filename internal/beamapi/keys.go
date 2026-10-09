package beamapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	status, decoded, err := postKeyVerification(ctx, apiURL, apiKey, httpClient)
	if err != nil {
		return APIKeyIdentity{}, err
	}
	if status != http.StatusOK || !decoded.Valid || strings.TrimSpace(decoded.OrganizationID) == "" {
		return APIKeyIdentity{}, ErrAPIKeyInvalid
	}
	return APIKeyIdentity{KeyID: decoded.KeyID, OrganizationID: decoded.OrganizationID}, nil
}

// APIKeyRejectedError is a key the Beam API refused to identify. Message is
// the Beam API's own explanation (for example "API key is disabled"); it never
// contains the key. It matches ErrAPIKeyInvalid under errors.Is.
type APIKeyRejectedError struct {
	StatusCode int
	Message    string
}

func (e *APIKeyRejectedError) Error() string {
	if e.Message == "" {
		return ErrAPIKeyInvalid.Error()
	}
	return ErrAPIKeyInvalid.Error() + ": " + e.Message
}

func (e *APIKeyRejectedError) Is(target error) bool { return target == ErrAPIKeyInvalid }

// LookupAPIKeyOrganization resolves which organization an API key acts for,
// without requiring the key to be able to pay right now. It mirrors the
// coordinator's own check for key-authenticated organization work such as
// agent enrollment: an active key that is merely out of credit (HTTP 402)
// still identifies its organization, while a missing, disabled, expired,
// revoked, or permission-less key does not.
func LookupAPIKeyOrganization(ctx context.Context, apiURL, apiKey string, httpClient *http.Client) (APIKeyIdentity, error) {
	status, decoded, err := postKeyVerification(ctx, apiURL, apiKey, httpClient)
	if err != nil {
		return APIKeyIdentity{}, err
	}
	identified := (status == http.StatusOK && decoded.Valid) || status == http.StatusPaymentRequired
	if !identified || strings.TrimSpace(decoded.OrganizationID) == "" {
		return APIKeyIdentity{}, &APIKeyRejectedError{StatusCode: status, Message: strings.TrimSpace(decoded.Error)}
	}
	return APIKeyIdentity{KeyID: strings.TrimSpace(decoded.KeyID), OrganizationID: strings.TrimSpace(decoded.OrganizationID)}, nil
}

func postKeyVerification(ctx context.Context, apiURL, apiKey string, httpClient *http.Client) (int, verifyAPIKeyResponse, error) {
	base := strings.TrimSpace(apiURL)
	key := strings.TrimSpace(apiKey)
	if base == "" {
		return 0, verifyAPIKeyResponse{}, errors.New("no Beam API URL is configured")
	}
	if key == "" {
		return 0, verifyAPIKeyResponse{}, errors.New("no Beam API key was supplied")
	}
	endpoint, err := url.Parse(base)
	if err != nil {
		return 0, verifyAPIKeyResponse{}, fmt.Errorf("parse Beam API URL: %w", err)
	}
	endpoint = endpoint.JoinPath("api", "keys", "verify")
	payload, err := json.Marshal(map[string]string{"apiKey": key})
	if err != nil {
		return 0, verifyAPIKeyResponse{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return 0, verifyAPIKeyResponse{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	client := httpClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, verifyAPIKeyResponse{}, err
	}
	defer func() { _ = response.Body.Close() }()
	var decoded verifyAPIKeyResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&decoded); err != nil && response.StatusCode == http.StatusOK {
		return 0, verifyAPIKeyResponse{}, fmt.Errorf("decode key verification: %w", err)
	}
	return response.StatusCode, decoded, nil
}
