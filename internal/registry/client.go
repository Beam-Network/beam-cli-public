package registry

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
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
	// ArtifactURL is a short-lived signed download URL that needs no
	// credentials; older Registries omit it.
	ArtifactURL      *string        `json:"artifactUrl,omitempty"`
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

// PublishRequest is the publish metadata. Trust level, validation status and
// publisher are derived by the Registry from the verified session, so a
// request does not claim them.
type PublishRequest struct {
	Manifest   map[string]any  `json:"manifest"`
	Artifact   PublishArtifact `json:"artifact"`
	DistTags   []string        `json:"distTags"`
	Provenance map[string]any  `json:"provenance,omitempty"`
}

type PublishArtifact struct {
	// ContentBase64 carries the archive only in the legacy JSON body.
	ContentBase64 string `json:"contentBase64,omitempty"`
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
	endpoint, err := c.artifactEndpoint(packageName, version)
	if err != nil {
		return nil, "", err
	}
	return c.artifactAttempt(ctx, endpoint, false)
}

// VersionArtifact downloads a version's archive, preferring the signed
// artifactURL from the version record and falling back to the versioned
// artifact endpoint when there is none or it fails. It returns the bytes and
// the Digest header; the caller verifies them against the version record.
func (c Client) VersionArtifact(ctx context.Context, packageName, version string, artifactURL *string) ([]byte, string, error) {
	if artifactURL != nil {
		if parsed, err := url.Parse(*artifactURL); err == nil && parsed.IsAbs() && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Host != "" {
			data, digest, err := c.artifactAttempt(ctx, parsed.String(), false)
			if err == nil {
				return data, digest, nil
			}
		}
	}
	return c.Artifact(ctx, packageName, version)
}

func (c Client) artifactEndpoint(packageName, version string) (string, error) {
	scope, name, err := PackageParts(packageName)
	if err != nil {
		return "", err
	}
	return resolveEndpoint(c.BaseURL, "/v1/packages/"+url.PathEscape(scope)+"/"+url.PathEscape(name)+"/versions/"+url.PathEscape(version)+"/artifact")
}

func (c Client) artifactAttempt(ctx context.Context, endpoint string, refreshed bool) ([]byte, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", err
	}
	request.Header.Set("Accept", "application/gzip, application/octet-stream")
	// The session token belongs to the Registry: a signed URL on another host
	// needs none and must never receive it.
	authenticated := sameOrigin(endpoint, c.BaseURL) && (c.Tokens != nil || c.Token != "")
	if authenticated && c.Tokens != nil {
		token, err := c.Tokens.AccessToken(ctx, refreshed)
		if err != nil {
			return nil, "", &Error{Kind: ErrAuth, Status: http.StatusUnauthorized, Cause: err}
		}
		request.Header.Set("Authorization", "Bearer "+token)
	} else if authenticated {
		request.Header.Set("Authorization", "Bearer "+c.Token)
	}
	response, err := c.registryOnlyAuthClient().Do(request)
	if err != nil {
		return nil, "", &Error{Kind: ErrUnavailable, Cause: err}
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized && authenticated && c.Tokens != nil && !refreshed {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		return c.artifactAttempt(ctx, endpoint, true)
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

// registryOnlyAuthClient returns the HTTP client with a redirect policy that
// drops the Authorization header whenever a redirect leaves the Registry
// origin. net/http alone keeps it for subdomains.
func (c Client) registryOnlyAuthClient() *http.Client {
	client := &http.Client{Timeout: 60 * time.Second}
	if c.HTTPClient != nil {
		copied := *c.HTTPClient
		client = &copied
	}
	previous := client.CheckRedirect
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if !sameOrigin(request.URL.String(), c.BaseURL) {
			request.Header.Del("Authorization")
		}
		if previous != nil {
			return previous(request, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return client
}

// sameOrigin reports whether two absolute URLs share scheme, host and port.
func sameOrigin(left, right string) bool {
	a, errA := url.Parse(left)
	b, errB := url.Parse(right)
	if errA != nil || errB != nil || a.Host == "" || b.Host == "" {
		return false
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Hostname(), b.Hostname()) && effectivePort(a) == effectivePort(b)
}

func effectivePort(value *url.URL) string {
	if port := value.Port(); port != "" {
		return port
	}
	switch strings.ToLower(value.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
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

// Publish uploads a version. The archive travels as the binary "artifact"
// part of a multipart body next to the "metadata" JSON part (REG-UPL-02). A
// Registry that cannot read multipart gets one retry with the legacy JSON
// body carrying the archive in base64.
func (c Client) Publish(ctx context.Context, packageName string, request PublishRequest, content []byte) (PublishResponse, error) {
	scope, name, err := PackageParts(packageName)
	if err != nil {
		return PublishResponse{}, err
	}
	path := "/v1/packages/" + url.PathEscape(scope) + "/" + url.PathEscape(name) + "/versions"
	var result PublishResponse
	if content == nil {
		err = c.do(ctx, http.MethodPost, path, request, &result)
		return result, err
	}
	request.Artifact.ContentBase64 = ""
	body, err := multipartPublishBody(request, content)
	if err != nil {
		return PublishResponse{}, err
	}
	err = c.doAttempt(ctx, http.MethodPost, path, body, &result, false)
	if !multipartUnsupported(err) {
		return result, err
	}
	request.Artifact.ContentBase64 = base64.StdEncoding.EncodeToString(content)
	result = PublishResponse{}
	err = c.do(ctx, http.MethodPost, path, request, &result)
	return result, err
}

func multipartPublishBody(request PublishRequest, content []byte) (*requestBody, error) {
	metadata, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	if err := writer.WriteField("metadata", string(metadata)); err != nil {
		return nil, err
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", `form-data; name="artifact"; filename="artifact.tgz"`)
	header.Set("Content-Type", "application/gzip")
	part, err := writer.CreatePart(header)
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(content); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return &requestBody{contentType: writer.FormDataContentType(), data: buffer.Bytes()}, nil
}

var multipartRejection = regexp.MustCompile(`(?i)multipart|content[- ]type|media type|FST_ERR_CTP`)

// multipartUnsupported reports whether a publish failed because the Registry
// cannot read a multipart body (Fastify answers 415 when no parser is
// registered for the type).
func multipartUnsupported(err error) bool {
	var registryErr *Error
	if !errors.As(err, &registryErr) {
		return false
	}
	return registryErr.Status == http.StatusUnsupportedMediaType ||
		registryErr.Status == http.StatusBadRequest && multipartRejection.MatchString(registryErr.Detail)
}

type requestBody struct {
	contentType string
	data        []byte
}

func (c Client) do(ctx context.Context, method, path string, body, out any) error {
	var payload *requestBody
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = &requestBody{contentType: "application/json", data: data}
	}
	return c.doAttempt(ctx, method, path, payload, out, false)
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

func (c Client) doAttempt(ctx context.Context, method, path string, body *requestBody, out any, refreshed bool) error {
	endpoint, err := resolveEndpoint(c.BaseURL, path)
	if err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body.data)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", body.contentType)
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
	resp, err := c.registryOnlyAuthClient().Do(req)
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
