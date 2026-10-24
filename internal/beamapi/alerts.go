package beamapi

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// BudgetAlert is one crossing of a monthly budget threshold.
//
// Beam records a row the first time each threshold is reached in a month, so a
// list of these is a history of crossings rather than a stream that repeats on
// every transfer.
type BudgetAlert struct {
	ID            string     `json:"id"`
	TargetType    string     `json:"targetType"`
	Threshold     int        `json:"threshold"`
	UsageCredits  Credits    `json:"usageCredits"`
	BudgetCredits Credits    `json:"budgetCredits"`
	PercentUsed   int        `json:"percentUsed"`
	Month         time.Time  `json:"month"`
	CreatedAt     time.Time  `json:"createdAt"`
	Acknowledged  *time.Time `json:"acknowledgedAt"`
	APIKey        *struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Prefix string `json:"prefix"`
	} `json:"apiKey"`
	Project *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"project"`
}

type budgetAlertsResponse struct {
	Alerts []BudgetAlert `json:"alerts"`
}

// BudgetAlertOptions narrows a listing. The zero value asks for everything the
// server will give.
type BudgetAlertOptions struct {
	// Only alerts raised after this instant. Poll with the newest CreatedAt you
	// have already seen.
	Since time.Time
	// Drop alerts an operator has dismissed in the console.
	UnacknowledgedOnly bool
	// 1-200. Zero takes the server default.
	Limit int
}

// BudgetAlerts lists budget threshold alerts for an organization.
//
// It reads /api/alerts rather than /v1/alerts because the CLI signs in as a
// person: /v1 takes a service account credential, which nobody should have to
// mint just to be warned about their own spend.
func (c Client) BudgetAlerts(
	ctx context.Context,
	organizationID string,
	options BudgetAlertOptions,
) ([]BudgetAlert, error) {
	if organizationID == "" {
		return nil, fmt.Errorf("no organization is selected")
	}

	query := url.Values{}
	if !options.Since.IsZero() {
		query.Set("since", options.Since.UTC().Format(time.RFC3339))
	}
	if options.UnacknowledgedOnly {
		query.Set("unacknowledged", "true")
	}
	if options.Limit > 0 {
		query.Set("limit", strconv.Itoa(options.Limit))
	}

	path := "/api/alerts"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}

	var response budgetAlertsResponse
	if err := c.GetForOrganization(ctx, path, organizationID, &response); err != nil {
		return nil, err
	}
	return response.Alerts, nil
}
