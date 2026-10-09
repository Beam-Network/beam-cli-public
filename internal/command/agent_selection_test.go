package command

import (
	"errors"
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/auth"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
)

func TestRegisteredAgentAcceptsItsOrganizationSlug(t *testing.T) {
	status := tunnel.AgentConnectionStatus{Registered: true, CoordinatorURL: "https://coordinator.example.com", OrganizationID: "org-a"}
	organizations := []auth.Organization{
		{ID: "org-a", PublicID: "pub-a", Slug: "acme"},
		{ID: "org-b", PublicID: "pub-b", Slug: "other"},
	}
	for _, requested := range []string{"org-a", " org-a ", "acme", "pub-a"} {
		if err := validateRegisteredAgentSelection(status, status.CoordinatorURL, false, requested, true, organizations); err != nil {
			t.Errorf("--organization %q refused for the registered organization: %v", requested, err)
		}
	}
	for _, test := range []struct {
		requested     string
		organizations []auth.Organization
	}{
		{requested: "org-b", organizations: organizations},
		{requested: "other", organizations: organizations},
		{requested: "pub-b", organizations: organizations},
		// Without a cached organization list a slug cannot be resolved.
		{requested: "acme"},
	} {
		err := validateRegisteredAgentSelection(status, status.CoordinatorURL, false, test.requested, true, test.organizations)
		var cliErr *Error
		if !errors.As(err, &cliErr) || cliErr.Kind != errorKindAgentConflict || cliErr.Message != "This machine is registered with another organization." {
			t.Errorf("--organization %q with %d cached organizations: err=%v", test.requested, len(test.organizations), err)
		}
	}
}
