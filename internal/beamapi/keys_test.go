package beamapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLookupAPIKeyOrganization(t *testing.T) {
	for _, test := range []struct {
		name        string
		status      int
		body        map[string]any
		wantOrg     string
		wantMessage string
	}{
		{name: "active key", status: http.StatusOK, body: map[string]any{"valid": true, "keyId": "key_1", "organizationId": "org_1"}, wantOrg: "org_1"},
		{name: "out of credit still identifies", status: http.StatusPaymentRequired, body: map[string]any{"valid": false, "keyId": "key_1", "organizationId": "org_1", "error": "No credits"}, wantOrg: "org_1"},
		{name: "no billable permission", status: http.StatusForbidden, body: map[string]any{"valid": false, "organizationId": "org_1", "error": "API key is not allowed to run billable actions"}, wantMessage: "API key is not allowed to run billable actions"},
		{name: "disabled", status: http.StatusForbidden, body: map[string]any{"valid": false, "error": "API key is disabled"}, wantMessage: "API key is disabled"},
		{name: "unknown", status: http.StatusUnauthorized, body: map[string]any{"valid": false, "error": "Invalid API key"}, wantMessage: "Invalid API key"},
		{name: "personal key without organization", status: http.StatusOK, body: map[string]any{"valid": true, "keyId": "key_1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request map[string]string
				if r.URL.Path != "/api/keys/verify" || json.NewDecoder(r.Body).Decode(&request) != nil || request["apiKey"] != "b1m_key" {
					http.Error(w, "unexpected request", http.StatusBadRequest)
					return
				}
				w.WriteHeader(test.status)
				_ = json.NewEncoder(w).Encode(test.body)
			}))
			defer server.Close()
			identity, err := LookupAPIKeyOrganization(context.Background(), server.URL, "b1m_key", server.Client())
			if test.wantOrg != "" {
				if err != nil || identity.OrganizationID != test.wantOrg {
					t.Fatalf("identity=%+v err=%v", identity, err)
				}
				return
			}
			var rejected *APIKeyRejectedError
			if !errors.As(err, &rejected) || !errors.Is(err, ErrAPIKeyInvalid) {
				t.Fatalf("err=%v, want APIKeyRejectedError matching ErrAPIKeyInvalid", err)
			}
			if rejected.Message != test.wantMessage || rejected.StatusCode != test.status {
				t.Fatalf("rejected=%+v", rejected)
			}
		})
	}
}

func TestVerifyAPIKeyStillRequiresAPayingKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_ = json.NewEncoder(w).Encode(map[string]any{"valid": false, "keyId": "key_1", "organizationId": "org_1"})
	}))
	defer server.Close()
	if _, err := VerifyAPIKey(context.Background(), server.URL, "b1m_key", server.Client()); !errors.Is(err, ErrAPIKeyInvalid) {
		t.Fatalf("err=%v, want ErrAPIKeyInvalid", err)
	}
}
