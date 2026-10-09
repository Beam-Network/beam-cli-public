//go:build !windows

package command

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/testutil"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
	"github.com/Beam-Network/beam-cli-public/internal/version"
)

func endpointsDaemon(t *testing.T) string {
	t.Helper()
	return testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case tunnel.StatusPath:
			_ = json.NewEncoder(w).Encode(tunnel.Status{Ready: true, AgentVersion: "test", ProtocolVersion: tunnel.ProtocolVersion})
		case tunnel.EndpointsPath:
			_ = json.NewEncoder(w).Encode(tunnel.EndpointsResponse{Endpoints: []tunnel.Endpoint{
				// Deliberately different ID lengths: this is what tab stops could
				// not keep aligned.
				{ID: "ep_a", Direction: "source", Kind: "file", Status: "active", PublicURL: "https://a.tunnel.example.test"},
				{ID: "ep_bbbbbbbbbbbbbbbb", Direction: "destination", Kind: "http", Status: "closed", PublicURL: "-"},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
}

func runEndpoints(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", endpointsDaemon(t))
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(context.Background(), args, &stdout, &stderr)
	return stdout.String(), stderr.String(), code
}

func TestListingRendersAlignedColumnsUnderAHeader(t *testing.T) {
	enableTunnels(t)
	stdout, stderr, code := runEndpoints(t, "tunnel", "endpoints")
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	want := strings.Join([]string{
		"ENDPOINT ID          DIRECTION    KIND  STATUS  URL",
		"ep_a                 source       file  active  https://a.tunnel.example.test",
		"ep_bbbbbbbbbbbbbbbb  destination  http  closed  -",
		"",
	}, "\n")
	if stdout != want {
		t.Errorf("stdout =\n%q\nwant\n%q", stdout, want)
	}
}

// The test binary's stdout is not a terminal, which is exactly the condition
// that must suppress styling. Every existing script reading these listings
// depends on it.
func TestListingIsUnstyledWhenStdoutIsNotATerminal(t *testing.T) {
	enableTunnels(t)
	stdout, _, code := runEndpoints(t, "tunnel", "endpoints")
	if code != ExitOK {
		t.Fatalf("code=%d", code)
	}
	if strings.Contains(stdout, "\x1b") {
		t.Errorf("non-terminal listing contains escape sequences: %q", stdout)
	}
}

func TestListingJSONIsUnaffectedByTableRendering(t *testing.T) {
	enableTunnels(t)
	stdout, stderr, code := runEndpoints(t, "tunnel", "endpoints", "--json")
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
	var got tunnel.EndpointsResponse
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout)
	}
	if len(got.Endpoints) != 2 || got.Endpoints[0].ID != "ep_a" || got.Endpoints[1].ID != "ep_bbbbbbbbbbbbbbbb" {
		t.Fatalf("result = %#v", got)
	}
	if strings.Contains(stdout, "ENDPOINT ID") {
		t.Error("JSON output carries a table header")
	}
}

// An empty listing prints nothing at all. A lone header would be new output
// that every caller of a previously silent command would have to handle.
func TestEmptyListingPrintsNoHeader(t *testing.T) {
	enableTunnels(t)
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case tunnel.StatusPath:
			_ = json.NewEncoder(w).Encode(tunnel.Status{Ready: true, AgentVersion: "test", ProtocolVersion: tunnel.ProtocolVersion})
		case tunnel.EndpointsPath:
			_ = json.NewEncoder(w).Encode(tunnel.EndpointsResponse{})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("BEAM_AGENT_SOCKET", socket)
	var stdout, stderr bytes.Buffer
	if code := New(version.BuildInfo{Version: "test"}).Run(context.Background(), []string{"tunnel", "endpoints"}, &stdout, &stderr); code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("empty listing printed %q", stdout.String())
	}
}
