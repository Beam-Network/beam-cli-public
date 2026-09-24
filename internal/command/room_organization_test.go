package command

import (
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/config"
)

func TestTakeOrganizationFlagRemovesBothForms(t *testing.T) {
	for _, test := range []struct {
		name      string
		args      []string
		requested string
		remaining []string
	}{
		{
			name:      "separate value",
			args:      []string{"create", "--organization", "org_123", "--api-key", "b1m_x"},
			requested: "org_123",
			remaining: []string{"create", "--api-key", "b1m_x"},
		},
		{
			name:      "equals form",
			args:      []string{"create", "--organization=acme", "--lease-ttl", "60"},
			requested: "acme",
			remaining: []string{"create", "--lease-ttl", "60"},
		},
		{
			name:      "absent",
			args:      []string{"create", "--api-key", "b1m_x"},
			requested: "",
			remaining: []string{"create", "--api-key", "b1m_x"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			requested, remaining, err := takeOrganizationFlag(test.args)
			if err != nil {
				t.Fatalf("takeOrganizationFlag() error = %v", err)
			}
			if requested != test.requested {
				t.Errorf("requested = %q, want %q", requested, test.requested)
			}
			if len(remaining) != len(test.remaining) {
				t.Fatalf("remaining = %v, want %v", remaining, test.remaining)
			}
			for index := range remaining {
				if remaining[index] != test.remaining[index] {
					t.Errorf("remaining = %v, want %v", remaining, test.remaining)
					break
				}
			}
		})
	}
}

// The flag must never be silently dropped: a missing value would otherwise let
// a room be created in whichever organization the key happens to name.
func TestTakeOrganizationFlagRejectsMissingValue(t *testing.T) {
	if _, _, err := takeOrganizationFlag([]string{"create", "--organization"}); err == nil {
		t.Fatal("takeOrganizationFlag() accepted --organization without a value")
	}
}

func TestRoomAPIKeyPrefersFlagOverEnvironment(t *testing.T) {
	t.Setenv("BEAM_API_KEY", "b1m_from_env")

	if got := roomAPIKey([]string{"create", "--api-key", "b1m_from_flag"}); got != "b1m_from_flag" {
		t.Errorf("roomAPIKey() = %q, want the flag value", got)
	}
	if got := roomAPIKey([]string{"create", "--api-key=b1m_equals"}); got != "b1m_equals" {
		t.Errorf("roomAPIKey() = %q, want the equals-form value", got)
	}
	if got := roomAPIKey([]string{"create"}); got != "b1m_from_env" {
		t.Errorf("roomAPIKey() = %q, want the environment value", got)
	}
}

// No organization named means the previous behaviour is unchanged, so the check
// must not require a key or reach the network.
func TestVerifyRoomOrganizationSkipsWhenUnset(t *testing.T) {
	app := &App{}
	if err := app.verifyRoomOrganization(t.Context(), "", []string{"create"}, config.Config{APIURL: "https://api.example.invalid"}, config.Paths{Credentials: t.TempDir() + "/credentials.json"}); err != nil {
		t.Fatalf("verifyRoomOrganization() error = %v", err)
	}
}

func TestVerifyRoomOrganizationRequiresAKey(t *testing.T) {
	t.Setenv("BEAM_API_KEY", "")
	app := &App{}
	if err := app.verifyRoomOrganization(t.Context(), "org_123", []string{"create"}, config.Config{APIURL: "https://api.example.invalid"}, config.Paths{Credentials: t.TempDir() + "/credentials.json"}); err == nil {
		t.Fatal("verifyRoomOrganization() accepted an organization without an API key")
	}
}
