package command

import (
	"os"
	"strconv"
	"strings"
)

// Tunnels are not offered yet: only Rooms are supported. The tunnel commands
// stay in the binary so they can be re-enabled without a code change, but they
// are absent from help and completion and refuse to run unless this variable
// is set for internal testing.
const experimentalTunnelsEnv = "BEAM_EXPERIMENTAL_TUNNELS"

func tunnelsEnabled() bool {
	enabled, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(experimentalTunnelsEnv)))
	return err == nil && enabled
}

// isTunnelCommand reports whether args name a tunnel feature. The "tunnel room"
// and "tunnel studio" spellings are compatibility aliases for "beam room" and
// "beam studio", so they are not tunnel features.
func isTunnelCommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "share", "receive", "unshare", "transfer", "operation":
		return true
	case "tunnel":
		return len(args) < 2 || (args[1] != "room" && args[1] != "studio")
	}
	return false
}

func tunnelsUnavailable() *Error {
	return cliError(ExitUsage, "Tunnels are not available yet.", "Use `beam room` to create and operate Beam rooms.", nil)
}
