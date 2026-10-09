package beamapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreditsStringRoundsUpToTwoDecimals(t *testing.T) {
	cases := []struct {
		amount Credits
		want   string
	}{
		{0, "0"},
		{0.04, "0.04"},
		{0.01, "0.01"},
		{0.07, "0.07"},
		{0.1, "0.1"},
		{100, "100"},
		{100.5, "100.5"},
		{100.50, "100.5"},
		{2.345, "2.35"},
		{0.001, "0.01"},
		{0.000001, "0.01"},
		{100.000001, "100.01"},
		{99.999999, "100"},
		{1234.5678901, "1234.57"},
		{0.0000004, "0"},
		{-2.5, "-2.5"},
		{-2.505, "-2.5"},
		{-0.01, "-0.01"},
		{-0.019999, "-0.01"},
		{-0.000001, "0"},
		{-0.0000004, "0"},
		{-1500, "-1500"},
	}
	for _, testCase := range cases {
		if got := testCase.amount.String(); got != testCase.want {
			t.Errorf("Credits(%v).String() = %q, want %q", float64(testCase.amount), got, testCase.want)
		}
	}
}

func TestCreditsHundredthsRoundsUp(t *testing.T) {
	cases := []struct {
		amount Credits
		want   int64
	}{
		{0, 0},
		{0.07, 7},
		{0.29, 29},
		{0.000001, 1},
		{99.99, 9999},
		{99.990001, 10000},
		{-0.000001, 0},
		{-2.505, -250},
	}
	for _, testCase := range cases {
		if got := testCase.amount.Hundredths(); got != testCase.want {
			t.Errorf("Credits(%v).Hundredths() = %d, want %d", float64(testCase.amount), got, testCase.want)
		}
	}
}

func TestBudgetAlertDecodesDecimalCredits(t *testing.T) {
	var alert BudgetAlert
	body := `{"id":"a1","targetType":"api_key","threshold":50,"usageCredits":50.123456,"budgetCredits":100,"percentUsed":50}`
	if err := json.Unmarshal([]byte(body), &alert); err != nil {
		t.Fatalf("decode decimal usage: %v", err)
	}
	if alert.UsageCredits != 50.123456 || alert.BudgetCredits != 100 {
		t.Fatalf("usage=%v budget=%v", alert.UsageCredits, alert.BudgetCredits)
	}
	if got := alert.UsageCredits.String(); got != "50.13" {
		t.Fatalf("six-decimal usage displays as %q, want 50.13", got)
	}

	// Budgets are whole credits server-side, but a decimal budget and a
	// negative usage must decode too rather than fail the whole listing.
	body = `{"usageCredits":-1.5,"budgetCredits":99.999999}`
	if err := json.Unmarshal([]byte(body), &alert); err != nil {
		t.Fatalf("decode negative usage and decimal budget: %v", err)
	}
	if alert.UsageCredits != -1.5 || alert.BudgetCredits != 99.999999 {
		t.Fatalf("usage=%v budget=%v", alert.UsageCredits, alert.BudgetCredits)
	}
}

func TestBudgetAlertsListsDecimalCredits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/alerts" {
			t.Errorf("path=%q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"alerts":[{"id":"a1","targetType":"project","threshold":1,"usageCredits":0.04,"budgetCredits":100,"percentUsed":0,"month":"2026-09-01T00:00:00Z","createdAt":"2026-09-29T10:00:00Z","acknowledgedAt":null,"apiKey":null,"project":{"id":"p1","name":"nightly"}}]}`))
	}))
	defer server.Close()

	alerts, err := (Client{BaseURL: server.URL, Tokens: &fakeTokens{}}).BudgetAlerts(
		context.Background(), "org-1", BudgetAlertOptions{},
	)
	if err != nil {
		t.Fatalf("BudgetAlerts: %v", err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts=%d", len(alerts))
	}
	if got := alerts[0].UsageCredits.String(); got != "0.04" {
		t.Fatalf("usage=%q, want 0.04", got)
	}

	// --json output re-encodes the alert; the amount must stay a decimal.
	encoded, err := json.Marshal(alerts[0])
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip map[string]any
	if err := json.Unmarshal(encoded, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip["usageCredits"] != 0.04 || roundTrip["budgetCredits"] != float64(100) {
		t.Fatalf("encoded usage=%v budget=%v", roundTrip["usageCredits"], roundTrip["budgetCredits"])
	}
}
