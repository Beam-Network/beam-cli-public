package roomcli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Beam-Network/beam-cli-public/internal/roomcli/protocol/btrchannel"
)

func (runner Runner) channelAdapter(ctx context.Context, mode outputMode, adapter, roomID string, args []string) error {
	if len(args) < 2 {
		return runner.usageError("channel " + adapter + " requires publish/listen and CHANNEL_ID")
	}
	action, channelID := args[0], args[1]
	flags := runner.flags("channel " + adapter + " " + action)
	relayID := flags.String("relay", "", "coordinator relay placement constraint")
	standby := flags.String("standby-relay", "", "coordinator standby relay placement constraint")
	replace := flags.Bool("replace-primary", false, "replace primary relay")
	ttl := flags.Int64("ttl-millis", 1000, "datagram lifetime in milliseconds")
	literal := flags.String("literal", "", "literal datagram payload")
	stdin := flags.Bool("stdin", adapter == "stream" && action == "publish", "read payload from stdin")
	transportMode := flags.String("transport", btrchannel.TransportAuto, "transport selector: auto, quic, or v1")
	if err := flags.Parse(args[2:]); err != nil || flags.NArg() != 0 {
		return errUsage
	}
	*transportMode = strings.ToLower(strings.TrimSpace(*transportMode))
	if *transportMode != btrchannel.TransportAuto && *transportMode != btrchannel.TransportQUIC && *transportMode != btrchannel.TransportV1 {
		return runner.usageError("--transport must be auto, quic, or v1")
	}
	query := url.Values{"relay_id": []string{*relayID}, "standby_relay_id": []string{*standby}, "replace_primary": []string{strconv.FormatBool(*replace)}}
	query.Set("transport", *transportMode)
	path := roomPath(roomID) + "/channels/" + url.PathEscape(channelID) + "/" + adapter
	switch action {
	case "publish":
		if adapter != "stream" || !*stdin || strings.TrimSpace(*literal) != "" {
			return runner.usageError("stream publish reads bytes from stdin")
		}
		var response map[string]any
		if err := runner.Client.DoRaw(ctx, http.MethodPost, path+"?"+query.Encode(), runner.in(), &response); err != nil {
			return err
		}
		return runner.renderGeneric(mode, response, fmt.Sprint(response["flow_id"]))
	case "listen":
		response, err := runner.Client.OpenStream(ctx, path+"?"+query.Encode())
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if profile := strings.TrimSpace(response.Header.Get("Beam-Transport-Profile")); profile != "" && !mode.Quiet && adapter == "stream" {
			_, _ = fmt.Fprintf(runner.err(), "transport_profile=%s\n", profile)
		}
		_, err = io.Copy(runner.out(), response.Body)
		return err
	case "send":
		if adapter != "datagram" || (*stdin == (strings.TrimSpace(*literal) != "")) {
			return runner.usageError("datagram send requires exactly one of --literal or --stdin")
		}
		var payload io.Reader = strings.NewReader(*literal)
		if *stdin {
			payload = io.LimitReader(runner.in(), int64(btrchannel.MaxDatagramPlaintextBytes)+1)
		}
		query.Set("ttl_millis", strconv.FormatInt(*ttl, 10))
		var response map[string]any
		if err := runner.Client.DoRaw(ctx, http.MethodPost, path+"?"+query.Encode(), payload, &response); err != nil {
			return err
		}
		return runner.renderGeneric(mode, response, fmt.Sprint(response["flow_id"]))
	default:
		return runner.usageError("unknown channel " + adapter + " command " + action)
	}
}
