//go:build !windows

package command

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/Beam-Network/beam-cli-public/internal/testutil"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
	"github.com/Beam-Network/beam-cli-public/internal/version"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAgentDashboardStartsDaemonAndReturnsSession(t *testing.T) {
	for _, jsonMode := range []bool{false, true} {
		t.Run(map[bool]string{false: "human", true: "json"}[jsonMode], func(t *testing.T) {
			var started atomic.Bool
			var starts atomic.Int32
			status := tunnel.Status{Ready: true, ProtocolVersion: tunnel.ProtocolVersion, ProtocolMinVersion: tunnel.ProtocolMinVersion}
			socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !started.Load() {
					http.Error(w, "stopped", 503)
					return
				}
				if r.URL.Path == tunnel.StatusPath {
					_ = json.NewEncoder(w).Encode(status)
					return
				}
				if r.URL.Path == "/v1/dashboard" && r.Method == "POST" {
					_ = json.NewEncoder(w).Encode(map[string]any{"url": "http://127.0.0.1:9999/#session=test", "expires_at": "2026-09-04T20:00:00Z"})
					return
				}
				http.NotFound(w, r)
			}))
			t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
			t.Setenv("BEAM_AGENT_SOCKET", socket)
			app := New(version.BuildInfo{Version: "test"})
			app.daemonStart = func(context.Context, tunnel.StartOptions) (tunnel.StartResult, error) {
				starts.Add(1)
				started.Store(true)
				return tunnel.StartResult{Status: status}, nil
			}
			app.openURL = func(string) error { t.Fatal("--no-open must never open browser"); return nil }
			args := []string{"agent", "dashboard", "--no-open"}
			if jsonMode {
				args = append([]string{"--json"}, args...)
			}
			var out, errout bytes.Buffer
			if code := app.Run(context.Background(), args, &out, &errout); code != ExitOK {
				t.Fatalf("code=%d stderr=%s", code, &errout)
			}
			if starts.Load() != 1 || !strings.Contains(out.String(), "http://127.0.0.1:9999/#session=test") {
				t.Fatalf("starts=%d out=%s", starts.Load(), &out)
			}
			if jsonMode && !json.Valid(out.Bytes()) {
				t.Fatalf("invalid JSON: %s", &out)
			}
		})
	}
}
func TestAgentDashboardMissingCapabilityAndInvalidOptions(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "old daemon", true: "invalid option"}[invalid], func(t *testing.T) {
			var requests atomic.Int32
			socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path == tunnel.StatusPath {
					_ = json.NewEncoder(w).Encode(tunnel.Status{Ready: true, ProtocolVersion: tunnel.ProtocolVersion, ProtocolMinVersion: tunnel.ProtocolMinVersion})
					return
				}
				http.NotFound(w, r)
			}))
			t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
			t.Setenv("BEAM_AGENT_SOCKET", socket)
			app := New(version.BuildInfo{Version: "test"})
			app.daemonStart = func(context.Context, tunnel.StartOptions) (tunnel.StartResult, error) {
				t.Fatal("unexpected start")
				return tunnel.StartResult{}, nil
			}
			args := []string{"agent", "dashboard"}
			if invalid {
				args = append(args, "--invalid")
			}
			var out, errout bytes.Buffer
			code := app.Run(context.Background(), args, &out, &errout)
			if invalid {
				if code == ExitOK || requests.Load() != 0 {
					t.Fatalf("code=%d requests=%d", code, requests.Load())
				}
			} else if code != ExitVersionIncompatible || !strings.Contains(errout.String(), "update") {
				t.Fatalf("code=%d stderr=%s", code, &errout)
			}
		})
	}
}
