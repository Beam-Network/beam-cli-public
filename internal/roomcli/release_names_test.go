package roomcli

import (
	"bytes"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/version"
)

// productionCommand finds a command named for the production binary, which a
// development or pull request build must never print.
var productionCommand = regexp.MustCompile("(^|[^\\w-])beam (room|webhook|agent)\\b")

// A development or pull request build prints its own binary name in every
// notice and usage line, so a copied command runs the same channel instead of
// the production CLI.
func TestRoomNoticesNameTheRunningBinary(t *testing.T) {
	original := version.Version
	t.Cleanup(func() { version.Version = original })
	for _, test := range []struct{ version, cli string }{
		{"v0.0.0-dev.c85930cb2c64.7267ac677c0d", "beam-dev"},
		{"v0.0.0-pr.42.abcdef123456.abcdef123456", "beam-pr-42"},
	} {
		version.Version = test.version
		var listings bytes.Buffer
		runner := Runner{Out: io.Discard, Err: &listings}
		runner.emptyListNotice(0, "")
		runner.emptyListNotice(1, "")
		runner.emptyListNotice(1, "activ")
		var usage bytes.Buffer
		_ = Runner{Out: io.Discard, Err: &usage}.usageError("")
		failedDelivery := liveOnlyPublishNotice(map[string]any{"online_deliveries": float64(2), "delivered_deliveries": float64(1), "failed_deliveries": float64(1)})
		for name, text := range map[string]string{
			"empty listings": listings.String(),
			"usage":          usage.String(),
			"listen":         liveOnlyListenNotice(),
			"publish":        liveOnlyPublishNotice(map[string]any{"publication_id": "p"}),
			"failed publish": failedDelivery,
		} {
			if productionCommand.MatchString(text) {
				t.Fatalf("%s %s names the production binary: %q", test.cli, name, text)
			}
			if !strings.Contains(text, test.cli+" ") {
				t.Fatalf("%s %s does not name %s: %q", test.cli, name, test.cli, text)
			}
		}
		for _, want := range []string{
			test.cli + " room: no rooms.\n",
			`"` + test.cli + ` room create --api-key $BEAM_API_KEY"`,
			`"` + test.cli + ` room join ROOM_ID --invitation-file FILE"`,
			`"` + test.cli + ` room list --state all"`,
		} {
			if !strings.Contains(listings.String(), want) {
				t.Fatalf("empty listings lack %q: %q", want, listings.String())
			}
		}
		if !strings.HasPrefix(usage.String(), "usage: "+test.cli+" room ") || !strings.Contains(usage.String(), "\n       "+test.cli+" room channel object ") {
			t.Fatalf("usage = %q", usage.String())
		}
		if !strings.Contains(failedDelivery, "`"+test.cli+" agent logs`") {
			t.Fatalf("failed delivery notice = %q", failedDelivery)
		}
	}
}
