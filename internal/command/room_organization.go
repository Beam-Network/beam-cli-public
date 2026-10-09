package command

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Beam-Network/beam-cli-public/internal/auth"
	"github.com/Beam-Network/beam-cli-public/internal/beamapi"
	"github.com/Beam-Network/beam-cli-public/internal/config"
)

// takeOrganizationFlag removes --organization from room arguments and returns
// its value. The flag is handled here rather than in the room runner because
// resolving it needs the Beam API URL and the cached organization list, and the
// runner only holds the local daemon client.
func takeOrganizationFlag(args []string) (string, []string, error) {
	var requested string
	remaining := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		argument := args[index]
		switch {
		case argument == "--organization":
			if index+1 >= len(args) {
				return "", nil, usage("--organization requires an organization ID, public ID, or slug")
			}
			requested = args[index+1]
			index++
		case strings.HasPrefix(argument, "--organization="):
			requested = strings.TrimPrefix(argument, "--organization=")
		default:
			remaining = append(remaining, argument)
		}
	}
	if requested != "" && strings.TrimSpace(requested) == "" {
		return "", nil, usage("--organization requires an organization ID, public ID, or slug")
	}
	return strings.TrimSpace(requested), remaining, nil
}

// roomAPIKey resolves the key a room is charged to, matching the room runner's
// own precedence so both agree on which key is in play.
func roomAPIKey(args []string) string {
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--api-key" && index+1 < len(args) {
			return strings.TrimSpace(args[index+1])
		}
		if strings.HasPrefix(argument, "--api-key=") {
			return strings.TrimSpace(strings.TrimPrefix(argument, "--api-key="))
		}
	}
	return strings.TrimSpace(os.Getenv("BEAM_API_KEY"))
}

// verifyRoomOrganization refuses a create whose API key does not act for the
// requested organization. A room's organization is decided by the key, so
// without this check a user who belongs to several organizations can only
// discover the room landed in the wrong one after it has been created, and
// creating a room is billable.
func (a *App) verifyRoomOrganization(ctx context.Context, requested string, args []string, cfg config.Config, paths config.Paths) error {
	if requested == "" {
		return nil
	}
	apiKey := roomAPIKey(args)
	if apiKey == "" {
		return usage("--organization requires --api-key or BEAM_API_KEY, because the key decides the room's organization")
	}
	identity, err := beamapi.VerifyAPIKey(ctx, cfg.APIURL, apiKey, nil)
	if errors.Is(err, beamapi.ErrAPIKeyInvalid) {
		return cliError(ExitAuth, "The Beam API key is not valid.", "Check the key, or create one in the Beam Console.", err)
	}
	if err != nil {
		return cliError(ExitOperationFailed, "Could not verify which organization the Beam API key belongs to.", "Check network access to the Beam API, then try again.", err)
	}
	if organizationMatches(requested, identity.OrganizationID, paths) {
		return nil
	}
	return typedCLIError(
		ExitUsage,
		errorKindOrganizationRequired,
		fmt.Sprintf("The Beam API key belongs to organization %s, not %s.", identity.OrganizationID, requested),
		"Pass an API key issued for that organization, or name the key's organization.",
		nil,
	)
}

// organizationMatches accepts an ID, public ID, or slug, resolving the latter
// two from the locally cached organization list so the check needs no session.
func organizationMatches(requested, organizationID string, paths config.Paths) bool {
	if strings.EqualFold(requested, organizationID) {
		return true
	}
	credentials, err := auth.NewStore(paths.Credentials).Load()
	if err != nil {
		return false
	}
	organization, found := findOrganization(credentials.Organizations, requested)
	return found && organization.ID == organizationID
}
