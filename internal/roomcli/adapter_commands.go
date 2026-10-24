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
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
)

func (runner Runner) channelAdapter(ctx context.Context, mode outputMode, adapter, roomID string, args []string) error {
	if len(args) < 2 {
		return runner.usageError("channel " + adapter + " requires publish/listen and CHANNEL_ID")
	}
	action, channelID := args[0], args[1]
	path := roomPath(roomID) + "/channels/" + url.PathEscape(channelID) + "/" + adapter
	if adapter == "stream" {
		return runner.channelStream(ctx, mode, action, path, args[2:])
	}
	return runner.channelDatagram(ctx, mode, action, path, args[2:])
}

// channelStream publishes or listens to a stream. Beam Workers serve streams
// directly and Beam picks the Worker, so a stream takes no relay or transport
// flags.
func (runner Runner) channelStream(ctx context.Context, mode outputMode, action, path string, args []string) error {
	flags := runner.flags("channel stream " + action)
	stdin := flags.Bool("stdin", action == "publish", "read payload from stdin")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errUsage
	}
	switch action {
	case "publish":
		if !*stdin {
			return runner.usageError("stream publish reads bytes from stdin")
		}
		var response map[string]any
		if err := runner.Client.DoRaw(ctx, http.MethodPost, path, runner.in(), &response); err != nil {
			return err
		}
		return runner.renderGeneric(mode, response, fmt.Sprint(response["flow_id"]))
	case "listen":
		response, err := runner.Client.OpenStream(ctx, path)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if profile := strings.TrimSpace(response.Header.Get("Beam-Transport-Profile")); profile != "" && !mode.Quiet {
			_, _ = fmt.Fprintf(runner.err(), "transport_profile=%s\n", profile)
		}
		body := &readErrorReader{reader: response.Body}
		_, err = io.Copy(runner.out(), body)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil && body.err == nil {
			// Writing the output failed; the stream itself did not.
			return err
		}
		return streamEnd(response.Trailer)
	default:
		return runner.usageError("unknown channel stream command " + action)
	}
}

// Trailers the agent sets on a stream listen response once the stream ends.
// Output has started by then, so a failure can no longer change the status.
const (
	streamEndTrailer          = "Beam-Stream-End"
	streamErrorCodeTrailer    = "Beam-Stream-Error-Code"
	streamErrorMessageTrailer = "Beam-Stream-Error-Message"
	streamErrorReasonsTrailer = "Beam-Stream-Error-Reasons"
)

// streamEnd reads how a listened stream ended from its trailers: complete
// with its authenticated end of stream, or failed. Without trailers the stream
// was cut short, for example because the agent stopped.
func streamEnd(trailer http.Header) error {
	if strings.EqualFold(strings.TrimSpace(trailer.Get(streamEndTrailer)), "eof") {
		return nil
	}
	code := strings.TrimSpace(trailer.Get(streamErrorCodeTrailer))
	if code == "" {
		code = "stream_truncated"
	}
	message := singleLine(trailer.Get(streamErrorMessageTrailer))
	if message == "" {
		message = "the stream ended before its end-of-stream marker"
	}
	var reasons []any
	for _, reason := range strings.Split(trailer.Get(streamErrorReasonsTrailer), ",") {
		if reason = strings.TrimSpace(reason); reason != "" {
			reasons = append(reasons, reason)
		}
	}
	var details map[string]any
	if len(reasons) != 0 {
		details = map[string]any{"reason": reasons[0], "reasons": reasons}
	}
	status := streamFailureStatus(code)
	return &tunnel.Error{Kind: tunnel.ErrResponse, Status: status, Code: code, Detail: message,
		Retryable: status == http.StatusServiceUnavailable, Details: details}
}

// streamFailureStatus is the HTTP status the agent gives a stream failure
// code, which sets the exit code of a failure reported in a trailer.
func streamFailureStatus(code string) int {
	switch code {
	case "stream_key_unavailable", "stream_renewal_failed":
		return http.StatusServiceUnavailable
	case "no_active_targets":
		return http.StatusConflict
	default:
		return http.StatusBadGateway
	}
}

// readErrorReader remembers the error reading the response failed with, other
// than its end, so a broken stream is told apart from a failed write.
type readErrorReader struct {
	reader io.Reader
	err    error
}

func (r *readErrorReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err != nil && err != io.EOF {
		r.err = err
	}
	return n, err
}

func (runner Runner) channelDatagram(ctx context.Context, mode outputMode, action, path string, args []string) error {
	flags := runner.flags("channel datagram " + action)
	relayID := flags.String("relay", "", "coordinator relay placement constraint")
	standby := flags.String("standby-relay", "", "coordinator standby relay placement constraint")
	replace := flags.Bool("replace-primary", false, "replace primary relay")
	ttl := flags.Int64("ttl-millis", 1000, "datagram lifetime in milliseconds")
	literal := flags.String("literal", "", "literal datagram payload")
	stdin := flags.Bool("stdin", false, "read payload from stdin")
	transportMode := flags.String("transport", btrchannel.TransportAuto, "transport selector: auto, quic, or v1")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errUsage
	}
	*transportMode = strings.ToLower(strings.TrimSpace(*transportMode))
	if *transportMode != btrchannel.TransportAuto && *transportMode != btrchannel.TransportQUIC && *transportMode != btrchannel.TransportV1 {
		return runner.usageError("--transport must be auto, quic, or v1")
	}
	query := url.Values{"relay_id": []string{*relayID}, "standby_relay_id": []string{*standby}, "replace_primary": []string{strconv.FormatBool(*replace)}}
	query.Set("transport", *transportMode)
	switch action {
	case "listen":
		response, err := runner.Client.OpenStream(ctx, path+"?"+query.Encode())
		if err != nil {
			return err
		}
		defer response.Body.Close()
		_, err = io.Copy(runner.out(), response.Body)
		return err
	case "send":
		if *stdin == (strings.TrimSpace(*literal) != "") {
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
		return runner.usageError("unknown channel datagram command " + action)
	}
}
