package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/Beam-Network/beam-cli-public/internal/auth"
	"github.com/Beam-Network/beam-cli-public/internal/beamapi"
	"github.com/Beam-Network/beam-cli-public/internal/config"
	"github.com/Beam-Network/beam-cli-public/internal/output"
)

// budget reports monthly budget threshold crossings.
//
// A budget that only reports itself in a browser tab is a budget nobody acts
// on: someone running a nightly transfer here finds out a budget is spent when
// a call is refused. This is how they find out before that.
func (a *App) budget(
	ctx context.Context,
	args []string,
	cfg config.Config,
	paths config.Paths,
	renderer output.Renderer,
) error {
	if len(args) > 0 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		return renderNamedHelp("budget", renderer)
	}

	limit := 0
	unacknowledgedOnly := false
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--unacknowledged":
			unacknowledgedOnly = true
		case "--limit":
			if index+1 >= len(args) {
				return usage("--limit needs a value")
			}
			parsed, err := strconv.Atoi(args[index+1])
			if err != nil || parsed < 1 || parsed > 200 {
				return usage("--limit must be between 1 and 200")
			}
			limit = parsed
			index++
		default:
			return usage(fmt.Sprintf("unknown budget option %q", args[index]))
		}
	}

	if strings.TrimSpace(cfg.Organization) == "" {
		return usage("no organization is selected — run `beam org use <slug>` first")
	}

	session := &auth.Session{
		Client: auth.DeviceClient{BaseURL: cfg.AuthURL},
		Store:  a.authStore(paths.Credentials),
	}
	api := beamapi.Client{BaseURL: cfg.APIURL, Tokens: session}

	alerts, err := api.BudgetAlerts(ctx, cfg.Organization, beamapi.BudgetAlertOptions{
		UnacknowledgedOnly: unacknowledgedOnly,
		Limit:              limit,
	})
	if err != nil {
		// Without this an expired session reports "The command failed", which
		// tells nobody to run `beam auth login`.
		var apiErr *beamapi.Error
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusForbidden {
			return cliError(
				ExitOperationFailed,
				"This account cannot read billing for the selected organization.",
				`Pick another with "beam org use <slug>", or ask an owner for billing access.`,
				err,
			)
		}
		return mapBeamAPIError(err)
	}

	// Newest first, so the crossing someone needs to see is the first line.
	sort.SliceStable(alerts, func(i, j int) bool {
		return alerts[i].CreatedAt.After(alerts[j].CreatedAt)
	})

	return renderer.Result(map[string]any{"alerts": alerts}, func(out io.Writer) error {
		if len(alerts) == 0 {
			_, err := fmt.Fprintln(out, "No budget alerts. Every budget is under its lowest threshold.")
			return err
		}
		for _, alert := range alerts {
			if _, err := fmt.Fprintln(out, formatBudgetAlert(alert, renderer)); err != nil {
				return err
			}
		}
		return nil
	})
}

func formatBudgetAlert(alert beamapi.BudgetAlert, renderer output.Renderer) string {
	target := "organization"
	switch {
	case alert.APIKey != nil:
		target = alert.APIKey.Name
		if target == "" {
			target = alert.APIKey.Prefix
		}
	case alert.Project != nil:
		target = alert.Project.Name
	}

	// The threshold is what was crossed; the percentage is where it stood when
	// it crossed. They differ, and reporting only one of them hides whether the
	// budget is merely warm or already spent.
	state := fmt.Sprintf("%d%% of %d credits", alert.PercentUsed, alert.BudgetCredits)
	if alert.UsageCredits >= alert.BudgetCredits && alert.BudgetCredits > 0 {
		state += " — budget spent"
	}

	line := fmt.Sprintf(
		"%s  %s crossed %d%%  (%s)  %s",
		alert.CreatedAt.Local().Format("Jan 2 15:04"),
		target,
		alert.Threshold,
		state,
		alert.Month.UTC().Format("2006-01"),
	)
	if alert.Acknowledged != nil {
		line += "  [acknowledged]"
	}
	return renderer.Label(line)
}
