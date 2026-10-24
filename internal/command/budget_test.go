package command

import (
	"strings"
	"testing"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/beamapi"
	"github.com/Beam-Network/beam-cli-public/internal/output"
)

func TestFormatBudgetAlertPrintsDecimalCredits(t *testing.T) {
	alert := beamapi.BudgetAlert{
		Threshold:     1,
		UsageCredits:  0.04,
		BudgetCredits: 100,
		PercentUsed:   0,
		Month:         time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		CreatedAt:     time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC),
	}
	line := formatBudgetAlert(alert, output.Renderer{Mode: output.Human})
	if !strings.Contains(line, "organization crossed 1%  (0.04 of 100 credits, 0%)  2026-09") {
		t.Fatalf("line = %q", line)
	}
	if strings.Contains(line, "budget spent") {
		t.Fatalf("an unspent budget reads as spent: %q", line)
	}
}

func TestFormatBudgetAlertMarksSpentBudgetWithFractionalUsage(t *testing.T) {
	alert := beamapi.BudgetAlert{
		Threshold:     100,
		UsageCredits:  100.000001,
		BudgetCredits: 100,
		PercentUsed:   100,
		Month:         time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		CreatedAt:     time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC),
		Project: &struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}{ID: "p1", Name: "nightly"},
	}
	line := formatBudgetAlert(alert, output.Renderer{Mode: output.Human})
	if !strings.Contains(line, "nightly crossed 100%  (100.01 of 100 credits, 100% — budget spent)") {
		t.Fatalf("line = %q", line)
	}

	// Usage rounds up to hundredths, so an older six-decimal amount just
	// under the budget is shown, and judged, as the whole budget.
	alert.UsageCredits = 99.999999
	line = formatBudgetAlert(alert, output.Renderer{Mode: output.Human})
	if !strings.Contains(line, "(100 of 100 credits, 100% — budget spent)") {
		t.Fatalf("line = %q", line)
	}

	alert.UsageCredits = 99.99
	alert.PercentUsed = 99
	line = formatBudgetAlert(alert, output.Renderer{Mode: output.Human})
	if strings.Contains(line, "budget spent") {
		t.Fatalf("usage just under the budget reads as spent: %q", line)
	}
	if !strings.Contains(line, "(99.99 of 100 credits, 99%)") {
		t.Fatalf("line = %q", line)
	}
}
