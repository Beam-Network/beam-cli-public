package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type ErrorKind string

const (
	ErrUnavailable ErrorKind = "unavailable"
	ErrAuth        ErrorKind = "auth"
	ErrNotFound    ErrorKind = "not_found"
	ErrConflict    ErrorKind = "conflict"
	ErrResponse    ErrorKind = "response"
)

type Error struct {
	Kind   ErrorKind
	Status int
	Detail string
	Cause  error
}

func (e *Error) Error() string {
	if e.Detail != "" {
		return e.Detail
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return "registry request failed"
}

func (e *Error) Unwrap() error { return e.Cause }

type Client struct {
	BaseURL    string
	Token      string
	Tokens     TokenSource
	HTTPClient *http.Client
}

type TokenSource interface {
	AccessToken(context.Context, bool) (string, error)
	Expire() error
}

type Version struct {
	ID               string         `json:"id"`
	PackageID        string         `json:"packageId"`
	PackageName      string         `json:"packageName"`
	Version          string         `json:"version"`
	Manifest         map[string]any `json:"manifest"`
	ManifestChecksum string         `json:"manifestChecksum"`
	ArtifactChecksum string         `json:"artifactChecksum"`
	ArtifactSize     int64          `json:"artifactSizeBytes"`
	HippiusBucket    *string        `json:"hippiusBucket"`
	HippiusKey       *string        `json:"hippiusKey"`
	HippiusEndpoint  *string        `json:"hippiusEndpoint"`
	MediaType        string         `json:"mediaType"`
	Signature        *string        `json:"signature"`
	Provenance       map[string]any `json:"provenance"`
	Status           string         `json:"status"`
	ValidationStatus string         `json:"validationStatus"`
	PublishedBy      string         `json:"publishedBy"`
	PublishedAt      string         `json:"publishedAt"`
	CreatedAt        string         `json:"createdAt"`
	UpdatedAt        string         `json:"updatedAt"`
}

type VersionsResponse struct {
	Versions []Version `json:"versions"`
}

type PackagesResponse struct {
	Packages []map[string]any `json:"packages"`
}

type ResolveResponse struct {
	Package         map[string]any `json:"package"`
	Version         Version        `json:"version"`
	RequestedRange  string         `json:"requestedRange"`
	ResolvedVersion string         `json:"resolvedVersion"`
	DistTag         map[string]any `json:"distTag,omitempty"`
}

type PublishRequest struct {
	Manifest         map[string]any  `json:"manifest"`
	Artifact         PublishArtifact `json:"artifact"`
	DistTags         []string        `json:"distTags"`
	TrustLevel       string          `json:"trustLevel,omitempty"`
	ValidationStatus string          `json:"validationStatus,omitempty"`
	PublishedBy      string          `json:"publishedBy,omitempty"`
	Provenance       map[string]any  `json:"provenance,omitempty"`
}

type PublishArtifact struct {
	ContentBase64 string `json:"contentBase64"`
	Checksum      string `json:"checksum"`
	SizeBytes     int64  `json:"sizeBytes"`
	MediaType     string `json:"mediaType"`
}

type PublishResponse struct {
	Package map[string]any `json:"package"`
	Version Version        `json:"version"`
}

func (c Client) Versions(ctx context.Context, packageName string) (VersionsResponse, error) {
	scope, name, err := PackageParts(packageName)
	if err != nil {
		return VersionsResponse{}, err
	}
	var result VersionsResponse
	err = c.do(ctx, http.MethodGet, "/v1/packages/"+url.PathEscape(scope)+"/"+url.PathEscape(name)+"/versions", nil, &result)
	return result, err
}

func (c Client) Search(ctx context.Context, query string) (PackagesResponse, error) {
	path := "/v1/packages"
	if strings.TrimSpace(query) != "" {
		path += "?search=" + url.QueryEscape(query)
	}
	var result PackagesResponse
	err := c.do(ctx, http.MethodGet, path, nil, &result)
	return result, err
}

func (c Client) Package(ctx context.Context, packageName string) (map[string]any, error) {
	scope, name, err := PackageParts(packageName)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	err = c.do(ctx, http.MethodGet, "/v1/packages/"+url.PathEscape(scope)+"/"+url.PathEscape(name), nil, &result)
	return result, err
}

func (c Client) Version(ctx context.Context, packageName, version string) (Version, error) {
	scope, name, err := PackageParts(packageName)
	if err != nil {
		return Version{}, err
	}
	var result Version
	err = c.do(ctx, http.MethodGet, "/v1/packages/"+url.PathEscape(scope)+"/"+url.PathEscape(name)+"/versions/"+url.PathEscape(version), nil, &result)
	return result, err
}

func (c Client) Artifact(ctx context.Context, packageName, version string) ([]byte, string, error) {
	scope, name, err := PackageParts(packageName)
	if err != nil {
		return nil, "", err
	}
	path := "/v1/packages/" + url.PathEscape(scope) + "/" + url.PathEscape(name) + "/versions/" + url.PathEscape(version) + "/artifact"
	return c.artifactAttempt(ctx, path, false)
}

func (c Client) artifactAttempt(ctx context.Context, path string, refreshed bool) ([]byte, string, error) {
	endpoint, err := resolveEndpoint(c.BaseURL, path)
	if err != nil {
		return nil, "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", err
	}
	request.Header.Set("Accept", "application/gzip, application/octet-stream")
	if c.Tokens != nil {
		token, err := c.Tokens.AccessToken(ctx, refreshed)
		if err != nil {
			return nil, "", &Error{Kind: ErrAuth, Status: http.StatusUnauthorized, Cause: err}
		}
		request.Header.Set("Authorization", "Bearer "+token)
	} else if c.Token != "" {
		request.Header.Set("Authorization", "Bearer "+c.Token)
	}
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, "", &Error{Kind: ErrUnavailable, Cause: err}
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized && c.Tokens != nil && !refreshed {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		return c.artifactAttempt(ctx, path, true)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		kind := ErrResponse
		if response.StatusCode == http.StatusNotFound {
			kind = ErrNotFound
		}
		return nil, "", &Error{Kind: kind, Status: response.StatusCode, Detail: responseMessage(data, response.StatusCode)}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<30))
	return data, response.Header.Get("Digest"), err
}

func (c Client) Resolve(ctx context.Context, packageName, versionRange string) (ResolveResponse, error) {
	scope, name, err := PackageParts(packageName)
	if err != nil {
		return ResolveResponse{}, err
	}
	path := "/v1/resolve/" + url.PathEscape(scope) + "/" + url.PathEscape(name) + "?range=" + url.QueryEscape(versionRange)
	var result ResolveResponse
	err = c.do(ctx, http.MethodGet, path, nil, &result)
	return result, err
}

func (c Client) Publish(ctx context.Context, packageName string, request PublishRequest) (PublishResponse, error) {
	scope, name, err := PackageParts(packageName)
	if err != nil {
		return PublishResponse{}, err
	}
	var result PublishResponse
	err = c.do(ctx, http.MethodPost, "/v1/packages/"+url.PathEscape(scope)+"/"+url.PathEscape(name)+"/versions", request, &result)
	return result, err
}

func (c Client) do(ctx context.Context, method, path string, body, out any) error {
	return c.doAttempt(ctx, method, path, body, out, false)
}

// resolveEndpoint appends an API path to the configured base URL, keeping any
// base path such as the "/registry" prefix. url.URL.Parse cannot be used here:
// every path below is an absolute reference, which RFC 3986 resolves by
// replacing the base path rather than extending it. The paths are already
// escaped by their callers, so they are joined as-is.
func resolveEndpoint(baseURL, path string) (string, error) {
	if _, err := url.Parse(baseURL); err != nil {
		return "", fmt.Errorf("invalid Registry URL: %w", err)
	}
	endpoint := strings.TrimRight(baseURL, "/") + path
	if _, err := url.Parse(endpoint); err != nil {
		return "", fmt.Errorf("build Registry URL: %w", err)
	}
	return endpoint, nil
}

func (c Client) doAttempt(ctx context.Context, method, path string, body, out any, refreshed bool) error {
	endpoint, err := resolveEndpoint(c.BaseURL, path)
	if err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		data, marshalErr := json.Marshal(body)
		if marshalErr != nil {
			return marshalErr
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	token := c.Token
	if c.Tokens != nil {
		var tokenErr error
		token, tokenErr = c.Tokens.AccessToken(ctx, refreshed)
		if tokenErr != nil {
			var retryable interface{ Temporary() bool }
			if errors.As(tokenErr, &retryable) && retryable.Temporary() {
				return &Error{Kind: ErrUnavailable, Status: http.StatusServiceUnavailable, Detail: "Beam authentication is temporarily unavailable.", Cause: tokenErr}
			}
			return &Error{Kind: ErrAuth, Status: http.StatusUnauthorized, Detail: "Beam authentication is missing or expired.", Cause: tokenErr}
		}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) {
			return &Error{Kind: ErrUnavailable, Detail: "Registry is unreachable.", Cause: err}
		}
		return &Error{Kind: ErrUnavailable, Detail: "Registry is unreachable.", Cause: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return &Error{Kind: ErrResponse, Status: resp.StatusCode, Detail: "Could not read Registry response.", Cause: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusUnauthorized && c.Tokens != nil && !refreshed {
			return c.doAttempt(ctx, method, path, body, out, true)
		}
		if resp.StatusCode == http.StatusUnauthorized && c.Tokens != nil {
			_ = c.Tokens.Expire()
		}
		kind := ErrResponse
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			kind = ErrAuth
		case http.StatusNotFound:
			kind = ErrNotFound
		case http.StatusConflict:
			kind = ErrConflict
		}
		return &Error{Kind: kind, Status: resp.StatusCode, Detail: responseMessage(data, resp.StatusCode)}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return &Error{Kind: ErrResponse, Status: resp.StatusCode, Detail: "Registry returned invalid JSON.", Cause: err}
		}
	}
	return nil
}

func PackageParts(packageName string) (string, string, error) {
	parts := strings.Split(packageName, "/")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "@") || len(parts[0]) < 2 || parts[1] == "" {
		return "", "", fmt.Errorf("package must use the form @scope/name")
	}
	return parts[0], parts[1], nil
}

func responseMessage(data []byte, status int) string {
	var payload struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(data, &payload) == nil {
		if payload.Message != "" {
			return payload.Message
		}
		if payload.Error != "" {
			return payload.Error
		}
	}
	return fmt.Sprintf("Registry request failed (HTTP %d).", status)
}
