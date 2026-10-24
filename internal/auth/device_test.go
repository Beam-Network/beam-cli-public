package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestDeviceClientUsesStrictOAuthFormContract(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		if got := r.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Fatalf("content type=%q", got)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		switch r.URL.Path {
		case "/oauth/device/authorize":
			assertForm(t, r.Form, url.Values{"client_id": {ClientID}, "scope": {Scope}})
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code":               "device-secret",
				"user_code":                 "BEAM-CODE",
				"verification_uri":          "https://auth.example/device",
				"verification_uri_complete": "https://auth.example/device?user_code=BEAM-CODE",
				"expires_in":                600,
				"interval":                  5,
			})
		case "/oauth/token":
			assertForm(t, r.Form, url.Values{
				"grant_type":  {deviceGrantType},
				"client_id":   {ClientID},
				"device_code": {"device-secret"},
			})
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "access-secret",
				"token_type":    "Bearer",
				"expires_in":    900,
				"refresh_token": "refresh-secret",
				"scope":         Scope,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := DeviceClient{BaseURL: server.URL}
	start, err := client.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if start.VerificationURIComplete != "https://auth.example/device?user_code=BEAM-CODE" {
		t.Fatalf("start=%#v", start)
	}
	result, err := client.Poll(context.Background(), start.DeviceCode)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != PollAuthorized || result.Token.RefreshToken != "refresh-secret" || result.Token.AccessToken != "access-secret" {
		t.Fatalf("result=%#v", result)
	}
	for _, retired := range []string{"/api/auth/device/start", "/api/auth/device/token"} {
		for _, call := range calls {
			if call == retired {
				t.Fatalf("retired endpoint called: %s", call)
			}
		}
	}
}

func TestPollMapsOAuthDeviceStates(t *testing.T) {
	for _, test := range []struct {
		name       string
		statusCode int
		code       string
		wantStatus PollStatus
		wantError  bool
	}{
		{name: "pending", statusCode: 400, code: "authorization_pending", wantStatus: PollPending},
		{name: "slow down", statusCode: 400, code: "slow_down", wantStatus: PollSlowDown},
		{name: "denied", statusCode: 400, code: "access_denied", wantError: true},
		{name: "expired", statusCode: 400, code: "expired_token", wantError: true},
		{name: "invalid grant", statusCode: 400, code: "invalid_grant", wantError: true},
		{name: "server unavailable", statusCode: 503, code: "server_error", wantStatus: PollNetworkError},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.statusCode)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": test.code})
			}))
			defer server.Close()
			result, err := (DeviceClient{BaseURL: server.URL}).Poll(context.Background(), "device")
			if (err != nil) != test.wantError {
				t.Fatalf("result=%#v err=%v", result, err)
			}
			if !test.wantError && result.Status != test.wantStatus {
				t.Fatalf("status=%v want=%v", result.Status, test.wantStatus)
			}
			if test.wantError {
				var oauthErr *OAuthError
				if !errors.As(err, &oauthErr) || oauthErr.Code != test.code {
					t.Fatalf("error=%#v", err)
				}
			}
		})
	}
}

func TestDeviceClientRejectsRetiredCamelCaseTokenResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accessToken": "legacy-access", "tokenType": "Bearer", "expiresIn": 900,
			"refreshToken": "legacy-refresh", "scope": Scope,
		})
	}))
	defer server.Close()
	_, err := (DeviceClient{BaseURL: server.URL}).Poll(context.Background(), "device")
	var oauthErr *OAuthError
	if !errors.As(err, &oauthErr) || oauthErr.Code != "invalid_token_response" {
		t.Fatalf("err=%#v", err)
	}
}

func TestAuthorizeHonorsSlowDownAndNetworkBackoff(t *testing.T) {
	responses := []struct {
		status int
		body   map[string]any
	}{
		{400, map[string]any{"error": "authorization_pending"}},
		{400, map[string]any{"error": "slow_down"}},
		{503, map[string]any{"error": "server_error"}},
		{200, map[string]any{
			"access_token": "access", "token_type": "Bearer", "expires_in": 900,
			"refresh_token": "refresh", "scope": Scope,
		}},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		response := responses[0]
		responses = responses[1:]
		w.WriteHeader(response.status)
		_ = json.NewEncoder(w).Encode(response.body)
	}))
	defer server.Close()

	now := time.Unix(1_700_000_000, 0)
	var waits []time.Duration
	client := DeviceClient{
		BaseURL: server.URL,
		now:     func() time.Time { return now },
		sleep: func(_ context.Context, duration time.Duration) error {
			waits = append(waits, duration)
			now = now.Add(duration)
			return nil
		},
	}
	token, err := client.Authorize(context.Background(), DeviceStart{
		DeviceCode: "device", Interval: time.Second, ExpiresIn: time.Minute,
	})
	if err != nil || token.RefreshToken != "refresh" {
		t.Fatalf("token=%#v err=%v", token, err)
	}
	want := []time.Duration{time.Second, time.Second, 6 * time.Second, 6 * time.Second}
	if len(waits) != len(want) {
		t.Fatalf("waits=%v", waits)
	}
	for index := range want {
		if waits[index] != want[index] {
			t.Fatalf("waits=%v want=%v", waits, want)
		}
	}
}

func TestAuthorizeCanBeInterruptedCleanly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (DeviceClient{BaseURL: "https://auth.example"}).Authorize(ctx, DeviceStart{
		DeviceCode: "secret", Interval: time.Second, ExpiresIn: time.Minute,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestAuthorizeExpiresWithoutPollingPastDeadline(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	client := DeviceClient{
		BaseURL: "https://auth.example",
		now:     func() time.Time { return now },
		sleep: func(_ context.Context, duration time.Duration) error {
			now = now.Add(duration)
			return nil
		},
	}
	_, err := client.Authorize(context.Background(), DeviceStart{
		DeviceCode: "device", Interval: 5 * time.Second, ExpiresIn: 2 * time.Second,
	})
	var oauthErr *OAuthError
	if !errors.As(err, &oauthErr) || oauthErr.Code != "expired_token" {
		t.Fatalf("err=%#v", err)
	}
}

func TestRefreshAndRevokeUseRotatingOAuthContract(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		switch r.URL.Path {
		case "/oauth/token":
			assertForm(t, r.Form, url.Values{
				"grant_type": {"refresh_token"}, "client_id": {ClientID}, "refresh_token": {"old-refresh"},
			})
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "access", "token_type": "Bearer", "expires_in": 900,
				"refresh_token": "new-refresh", "scope": Scope,
			})
		case "/oauth/revoke":
			assertForm(t, r.Form, url.Values{
				"client_id": {ClientID}, "token": {"new-refresh"}, "token_type_hint": {"refresh_token"},
			})
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := DeviceClient{BaseURL: server.URL}
	token, err := client.Refresh(context.Background(), "old-refresh")
	if err != nil || token.RefreshToken != "new-refresh" {
		t.Fatalf("token=%#v err=%v", token, err)
	}
	if err := client.Revoke(context.Background(), token.RefreshToken); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
}

func assertForm(t *testing.T, got, want url.Values) {
	t.Helper()
	if got.Encode() != want.Encode() {
		t.Fatalf("form=%q want=%q", got.Encode(), want.Encode())
	}
}
