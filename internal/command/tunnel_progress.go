package command

import (
	"fmt"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/output"
)

type tunnelProgressOptions struct {
	Public        bool
	ObjectStorage bool
}

type tunnelProgressStage struct {
	After   time.Duration
	Message string
}

type tunnelProgressDisplay struct {
	renderer output.Renderer
	spinner  *output.Spinner
	message  string
	started  time.Time
}

func withTunnelCreationProgress[T any](renderer output.Renderer, options tunnelProgressOptions, create func() (T, error)) (T, error) {
	started := time.Now()
	display := newTunnelProgressDisplay(renderer, tunnelProgressInitialMessage(options), started)
	stopStages := make(chan struct{})
	stagesDone := make(chan struct{})
	go advanceTunnelProgress(display, tunnelProgressStages(options), stopStages, stagesDone)

	result, err := create()
	close(stopStages)
	<-stagesDone
	display.finish(err == nil, time.Now())

	elapsed := readableTunnelDuration(time.Since(started))
	if err != nil {
		renderer.Progress(fmt.Sprintf("Tunnel creation failed after %s.", elapsed))
		return result, err
	}
	renderer.Progress(fmt.Sprintf("✓ %s ready in %s", tunnelProgressResultName(options), elapsed))
	return result, nil
}

func newTunnelProgressDisplay(renderer output.Renderer, message string, now time.Time) *tunnelProgressDisplay {
	return &tunnelProgressDisplay{
		renderer: renderer,
		spinner:  renderer.StartSpinner(message),
		message:  message,
		started:  now,
	}
}

func (display *tunnelProgressDisplay) advance(message string, now time.Time) {
	display.completeCurrent("✓", now)
	display.message = message
	display.started = now
	display.spinner = display.renderer.StartSpinner(message)
}

func (display *tunnelProgressDisplay) finish(success bool, now time.Time) {
	marker := "✗"
	if success {
		marker = "✓"
	}
	display.completeCurrent(marker, now)
}

func (display *tunnelProgressDisplay) completeCurrent(marker string, now time.Time) {
	if display == nil || display.message == "" {
		return
	}
	display.spinner.Stop()
	duration := readableTunnelStepDuration(now.Sub(display.started))
	display.renderer.Progress(fmt.Sprintf("%s %s (%s)", marker, display.message, duration))
}

func advanceTunnelProgress(display *tunnelProgressDisplay, stages []tunnelProgressStage, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	previous := time.Duration(0)
	for _, stage := range stages {
		delay := stage.After - previous
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-stop:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		case <-timer.C:
			display.advance(stage.Message, time.Now())
		}
		previous = stage.After
	}
	<-stop
}

func tunnelProgressInitialMessage(options tunnelProgressOptions) string {
	if options.ObjectStorage {
		return "Preparing object-storage tunnel"
	}
	if options.Public {
		return "Preparing public tunnel"
	}
	return "Preparing tunnel"
}

func tunnelProgressStages(options tunnelProgressOptions) []tunnelProgressStage {
	if !options.Public {
		return []tunnelProgressStage{{After: time.Second, Message: "Connecting to Beam relay"}}
	}
	stages := []tunnelProgressStage{
		{After: 1500 * time.Millisecond, Message: "Connecting to Beam relays"},
		{After: 4 * time.Second, Message: "Publishing stable public route"},
		{After: 7 * time.Second, Message: "Finalizing DNS and TLS"},
	}
	if options.ObjectStorage {
		stages = append(stages, tunnelProgressStage{After: 10 * time.Second, Message: "Securing S3-compatible access"})
	}
	waitingAfter := 12 * time.Second
	if options.ObjectStorage {
		waitingAfter = 15 * time.Second
	}
	stages = append(stages, tunnelProgressStage{After: waitingAfter, Message: "Still waiting for public endpoint"})
	return stages
}

func tunnelProgressResultName(options tunnelProgressOptions) string {
	if options.ObjectStorage {
		return "Object-storage tunnel"
	}
	if options.Public {
		return "Public tunnel"
	}
	return "Tunnel"
}

func readableTunnelDuration(duration time.Duration) string {
	if duration < time.Second {
		return "<1s"
	}
	if duration < 10*time.Second {
		return duration.Round(100 * time.Millisecond).String()
	}
	return duration.Round(time.Second).String()
}

func readableTunnelStepDuration(duration time.Duration) string {
	if duration < 10*time.Millisecond {
		return "<10ms"
	}
	if duration < time.Second {
		return duration.Round(10 * time.Millisecond).String()
	}
	return readableTunnelDuration(duration)
}
