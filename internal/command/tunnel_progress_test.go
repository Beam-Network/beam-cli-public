package command

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/output"
)

func TestTunnelCreationProgressExplainsNonInteractiveWait(t *testing.T) {
	var stderr bytes.Buffer
	renderer := output.Renderer{Mode: output.Human, Err: &stderr}
	result, err := withTunnelCreationProgress(renderer, tunnelProgressOptions{
		Public: true, ObjectStorage: true,
	}, func() (string, error) {
		return "ready", nil
	})
	if err != nil || result != "ready" {
		t.Fatalf("result=%q err=%v", result, err)
	}
	for _, expected := range []string{
		"Preparing object-storage tunnel...",
		"✓ Preparing object-storage tunnel (",
		"✓ Object-storage tunnel ready in <1s",
	} {
		if !strings.Contains(stderr.String(), expected) {
			t.Fatalf("stderr missing %q: %q", expected, stderr.String())
		}
	}
}

func TestTunnelProgressKeepsCompletedStageLinesWithDurations(t *testing.T) {
	var stderr bytes.Buffer
	renderer := output.Renderer{Mode: output.Human, Err: &stderr}
	started := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	display := newTunnelProgressDisplay(renderer, "Preparing public tunnel", started)
	display.advance("Connecting to Beam relays", started.Add(1500*time.Millisecond))
	display.advance("Publishing stable public route", started.Add(4*time.Second))
	display.finish(false, started.Add(7*time.Second))

	for _, expected := range []string{
		"✓ Preparing public tunnel (1.5s)",
		"✓ Connecting to Beam relays (2.5s)",
		"✗ Publishing stable public route (3s)",
	} {
		if !strings.Contains(stderr.String(), expected) {
			t.Fatalf("stderr missing %q: %q", expected, stderr.String())
		}
	}
}

func TestTunnelCreationProgressIsSilentForJSON(t *testing.T) {
	var stderr bytes.Buffer
	renderer := output.Renderer{Mode: output.JSON, Err: &stderr, Interactive: true}
	_, err := withTunnelCreationProgress(renderer, tunnelProgressOptions{Public: true}, func() (bool, error) {
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestTunnelCreationProgressReportsFailure(t *testing.T) {
	var stderr bytes.Buffer
	renderer := output.Renderer{Mode: output.Human, Err: &stderr}
	want := errors.New("creation failed")
	_, err := withTunnelCreationProgress(renderer, tunnelProgressOptions{Public: true}, func() (bool, error) {
		return false, want
	})
	if !errors.Is(err, want) {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(stderr.String(), "Tunnel creation failed after <1s.") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestTunnelProgressStagesDescribePublicLifecycle(t *testing.T) {
	stages := tunnelProgressStages(tunnelProgressOptions{Public: true, ObjectStorage: true})
	want := []tunnelProgressStage{
		{After: 1500 * time.Millisecond, Message: "Connecting to Beam relays"},
		{After: 4 * time.Second, Message: "Publishing stable public route"},
		{After: 7 * time.Second, Message: "Finalizing DNS and TLS"},
		{After: 10 * time.Second, Message: "Securing S3-compatible access"},
		{After: 15 * time.Second, Message: "Still waiting for public endpoint"},
	}
	if len(stages) != len(want) {
		t.Fatalf("stages=%#v", stages)
	}
	for index := range want {
		if stages[index] != want[index] {
			t.Fatalf("stage %d = %#v, want %#v", index, stages[index], want[index])
		}
	}
}
