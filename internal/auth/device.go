package auth

import (
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

const (
	ClientID        = "beam-cli"
	Scope           = "cli:access"
	deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"
	maxBackoff      = 30 * time.Second
)

type DeviceStart struct {
	VerificationURI         string
	VerificationURIComplete string
	UserCode                string
	DeviceCode              string
	Interval                time.Duration
	ExpiresIn               time.Duration
}

type Token struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	Scope        string
}

type PollStatus int

const (
	PollAuthorized PollStatus = iota
	PollPending
	PollSlowDown
	PollNetworkError
)

type PollResult struct {
	Status PollStatus
	Token  Token
}

type OAuthError struct {
	Code      string
	Status    int
	Retryable bool
	message   string
	Cause     error
}

func (e *OAuthError) Error() string {
	if e.message != "" {
		return e.message
	}
	return "OAuth request failed"
}

func (e *OAuthError) Unwrap() error { return e.Cause }

func (e *OAuthError) Temporary() bool { return e.Retryable }

type DeviceClient struct {
	BaseURL    string
	HTTPClient *http.Client
	now        func() time.Time
	sleep      func(context.Context, time.Duration) error
}

func (c DeviceClient) Start(ctx context.Context) (DeviceStart, error) {
	var raw struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	status, err := c.requestForm(ctx, "/oauth/device/authorize", url.Values{
		"client_id": {ClientID},
		"scope":     {Scope},
	}, &raw)
	if err != nil {
		return DeviceStart{}, err
	}
	if status < 200 || status >= 300 {
		return DeviceStart{}, oauthStatusError(status, "oauth_error")
	}
	if raw.DeviceCode == "" || raw.UserCode == "" || raw.VerificationURI == "" ||
		raw.VerificationURIComplete == "" || raw.ExpiresIn <= 0 || raw.Interval <= 0 {
		return DeviceStart{}, invalidResponse("Beam Auth returned an incomplete device authorization")
	}
	if _, err := url.ParseRequestURI(raw.VerificationURI); err != nil {
		return DeviceStart{}, invalidResponse("Beam Auth returned an invalid verification_uri")
	}
	if _, err := url.ParseRequestURI(raw.VerificationURIComplete); err != nil {
		return DeviceStart{}, invalidResponse("Beam Auth returned an invalid verification_uri_complete")
	}
	return DeviceStart{
		VerificationURI:         raw.VerificationURI,
		VerificationURIComplete: raw.VerificationURIComplete,
		UserCode:                raw.UserCode,
		DeviceCode:              raw.DeviceCode,
		Interval:                time.Duration(raw.Interval) * time.Second,
		ExpiresIn:               time.Duration(raw.ExpiresIn) * time.Second,
	}, nil
}

func (c DeviceClient) Poll(ctx context.Context, deviceCode string) (PollResult, error) {
	var raw struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
		RefreshToken string `json:"refresh_token"`
		Scope        string `json:"scope"`
		Error        string `json:"error"`
	}
	status, err := c.requestForm(ctx, "/oauth/token", url.Values{
		"grant_type":  {deviceGrantType},
		"client_id":   {ClientID},
		"device_code": {deviceCode},
	}, &raw)
	if err != nil {
		var oauthErr *OAuthError
		if errors.As(err, &oauthErr) && oauthErr.Retryable {
			return PollResult{Status: PollNetworkError}, nil
		}
		return PollResult{}, err
	}
	if status < 200 || status >= 300 {
		switch raw.Error {
		case "authorization_pending":
			return PollResult{Status: PollPending}, nil
		case "slow_down":
			return PollResult{Status: PollSlowDown}, nil
		}
		if status >= 500 {
			return PollResult{Status: PollNetworkError}, nil
		}
		return PollResult{}, oauthStatusError(status, raw.Error)
	}
	token, err := c.parseToken(raw.AccessToken, raw.TokenType, raw.ExpiresIn, raw.RefreshToken, raw.Scope)
	if err != nil {
		return PollResult{}, err
	}
	return PollResult{Status: PollAuthorized, Token: token}, nil
}

// Authorize polls until the user completes the device flow, the context is
// cancelled, or the device code expires. Transient failures use bounded
// exponential backoff and never invalidate an existing durable session.
func (c DeviceClient) Authorize(ctx context.Context, start DeviceStart) (Token, error) {
	now := c.clock()
	expiresAt := now().Add(start.ExpiresIn)
	pollInterval := start.Interval
	nextInterval := pollInterval
	networkFailures := 0
	deviceCode := start.DeviceCode
	for deviceCode != "" {
		remaining := expiresAt.Sub(now())
		if remaining <= 0 {
			return Token{}, oauthStatusError(http.StatusBadRequest, "expired_token")
		}
		wait := nextInterval
		if wait > remaining {
			wait = remaining
		}
		if err := c.pause(ctx, wait); err != nil {
			return Token{}, err
		}
		if !now().Before(expiresAt) {
			return Token{}, oauthStatusError(http.StatusBadRequest, "expired_token")
		}
		result, err := c.Poll(ctx, deviceCode)
		if err != nil {
			var oauthErr *OAuthError
			if errors.As(err, &oauthErr) && oauthErr.Code == "invalid_grant" {
				deviceCode = ""
			}
			return Token{}, err
		}
		switch result.Status {
		case PollAuthorized:
			deviceCode = ""
			return result.Token, nil
		case PollPending:
			networkFailures = 0
			nextInterval = pollInterval
		case PollSlowDown:
			networkFailures = 0
			pollInterval += 5 * time.Second
			nextInterval = pollInterval
		case PollNetworkError:
			networkFailures++
			nextInterval = boundedBackoff(pollInterval, networkFailures)
		}
	}
	return Token{}, oauthStatusError(http.StatusBadRequest, "invalid_grant")
}

func (c DeviceClient) Refresh(ctx context.Context, refreshToken string) (Token, error) {
	var raw struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
		RefreshToken string `json:"refresh_token"`
		Scope        string `json:"scope"`
		Error        string `json:"error"`
	}
	status, err := c.requestForm(ctx, "/oauth/token", url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {ClientID},
		"refresh_token": {refreshToken},
	}, &raw)
	if err != nil {
		return Token{}, err
	}
	if status < 200 || status >= 300 {
		return Token{}, oauthStatusError(status, raw.Error)
	}
	return c.parseToken(raw.AccessToken, raw.TokenType, raw.ExpiresIn, raw.RefreshToken, raw.Scope)
}

func (c DeviceClient) Revoke(ctx context.Context, refreshToken string) error {
	var raw struct {
		Error string `json:"error"`
	}
	status, err := c.requestForm(ctx, "/oauth/revoke", url.Values{
		"client_id":       {ClientID},
		"token":           {refreshToken},
		"token_type_hint": {"refresh_token"},
	}, &raw)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return oauthStatusError(status, raw.Error)
	}
	return nil
}

func (c DeviceClient) parseToken(accessToken, tokenType string, expiresIn int, refreshToken, scope string) (Token, error) {
	if accessToken == "" || refreshToken == "" || expiresIn <= 0 {
		return Token{}, invalidResponse("Beam Auth returned an incomplete token response")
	}
	if !strings.EqualFold(tokenType, "Bearer") {
		return Token{}, invalidResponse("Beam Auth returned an unsupported token_type")
	}
	granted := false
	for _, item := range strings.Fields(scope) {
		if item == Scope {
			granted = true
			break
		}
	}
	if !granted {
		return Token{}, &OAuthError{Code: "invalid_scope", Status: http.StatusForbidden, message: "Beam Auth did not grant cli:access"}
	}
	return Token{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    c.clock()().Add(time.Duration(expiresIn) * time.Second),
		Scope:        scope,
	}, nil
}

func (c DeviceClient) requestForm(ctx context.Context, path string, form url.Values, out any) (int, error) {
	base, err := url.Parse(c.BaseURL)
	if err != nil {
		return 0, fmt.Errorf("invalid auth URL: %w", err)
	}
	endpoint, err := base.Parse(path)
	if err != nil {
		return 0, fmt.Errorf("build auth URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, &OAuthError{
			Code:      "temporary_auth_error",
			Status:    http.StatusServiceUnavailable,
			Retryable: true,
			message:   "Beam Auth is temporarily unavailable",
			Cause:     err,
		}
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, &OAuthError{
			Code:      "temporary_auth_error",
			Status:    http.StatusServiceUnavailable,
			Retryable: true,
			message:   "Could not read the Beam Auth response",
			Cause:     err,
		}
	}
	if len(payload) > 0 && out != nil {
		if err := json.Unmarshal(payload, out); err != nil {
			if resp.StatusCode >= http.StatusInternalServerError {
				return resp.StatusCode, nil
			}
			return resp.StatusCode, invalidResponse("Beam Auth returned invalid JSON")
		}
	}
	return resp.StatusCode, nil
}

func (c DeviceClient) clock() func() time.Time {
	if c.now != nil {
		return c.now
	}
	return time.Now
}

func (c DeviceClient) pause(ctx context.Context, duration time.Duration) error {
	if c.sleep != nil {
		return c.sleep(ctx, duration)
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func boundedBackoff(base time.Duration, failures int) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	backoff := base
	for index := 1; index < failures && backoff < maxBackoff; index++ {
		backoff *= 2
	}
	if backoff > maxBackoff {
		return maxBackoff
	}
	return backoff
}

func invalidResponse(message string) *OAuthError {
	return &OAuthError{Code: "invalid_token_response", Status: http.StatusBadGateway, message: message}
}

func oauthStatusError(status int, code string) *OAuthError {
	if code == "" {
		code = "oauth_error"
	}
	messages := map[string]string{
		"access_denied":  "Beam login was denied",
		"expired_token":  "The Beam device code has expired",
		"invalid_grant":  "The Beam login or refresh token is no longer valid",
		"invalid_client": "Beam Auth rejected the public CLI client",
		"invalid_scope":  "Beam Auth rejected the cli:access scope",
	}
	message := messages[code]
	if message == "" {
		message = fmt.Sprintf("Beam Auth rejected the request (HTTP %d)", status)
	}
	return &OAuthError{
		Code:      code,
		Status:    status,
		Retryable: status >= 500,
		message:   message,
	}
}
