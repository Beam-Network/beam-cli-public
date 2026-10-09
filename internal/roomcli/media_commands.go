package roomcli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"

	"time"
)

const defaultPublisherName = "main"

type mediaEndpoint struct {
	RoomID     string    `json:"room_id"`
	ChannelID  string    `json:"channel_id"`
	Name       string    `json:"name"`
	WorkloadID string    `json:"workload_id"`
	SessionID  string    `json:"session_id"`
	WHIPURL    string    `json:"whip_url"`
	WHEPURL    string    `json:"whep_url"`
	PlayerURL  string    `json:"player_url"`
	Token      string    `json:"token"`
	ExpiresAt  time.Time `json:"expires_at"`
}

type localStream struct {
	Endpoint mediaEndpoint `json:"endpoint"`
	URL      string        `json:"url"`
	Path     string        `json:"path"`
}

// mediaView names one publisher streaming in a channel. Publishing marks the
// streams this agent is the source of, which is what separates previewing your
// own broadcast from watching another member's.
type mediaView struct {
	WorkloadID string    `json:"workload_id"`
	Name       string    `json:"name"`
	State      string    `json:"state"`
	Publishing bool      `json:"publishing"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type mediaViewListing struct {
	Views []mediaView `json:"views"`
}

func (runner Runner) channelMedia(ctx context.Context, mode outputMode, args []string) error {
	if len(args) < 3 {
		return runner.usageError("channel media requires publish, view, or list, ROOM_ID, and CHANNEL_ID")
	}
	action, roomID, channelID, rest := args[0], args[1], args[2], args[3:]
	base := roomPath(roomID) + "/channels/" + url.PathEscape(channelID) + "/media"
	switch action {
	case "publish":
		return runner.channelMediaPublish(ctx, mode, base, rest)
	case "view":
		return runner.channelMediaView(ctx, mode, base, rest)
	case "list":
		return runner.channelMediaList(ctx, mode, base, rest)
	}
	return runner.usageError("channel media action must be publish, view, or list")
}

func (runner Runner) channelMediaPublish(ctx context.Context, mode outputMode, base string, args []string) error {
	flags := runner.flags("channel media publish")
	name := flags.String("as", "", "publisher name; defaults to "+defaultPublisherName)
	snippets := flags.Bool("snippets", false, "print code for consuming or publishing this stream")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errUsage
	}
	path := base + "/publish"
	if *name != "" {
		path += "?name=" + url.QueryEscape(*name)
	}
	var endpoint mediaEndpoint
	if err := runner.Client.Do(ctx, http.MethodPost, path, "", nil, &endpoint); err != nil {
		return err
	}
	return runner.renderMediaPublish(mode, endpoint, *snippets)
}

// channelMediaList reports every publisher streaming in the channel. Each
// subscription binds exactly one of them, so this is where a member learns that
// a channel carries several streams and picks the one to watch.
func (runner Runner) channelMediaList(ctx context.Context, mode outputMode, base string, args []string) error {
	flags := runner.flags("channel media list")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errUsage
	}
	listing, err := runner.mediaViews(ctx, base)
	if err != nil {
		return err
	}
	return runner.renderMediaList(mode, listing)
}

// channelMediaView subscribes to one publisher, or with --all to every one at
// once. Without a selection the daemon binds whichever workload reported most
// recently, which on a channel with several publishers is nobody in particular.
func (runner Runner) channelMediaView(ctx context.Context, mode outputMode, base string, args []string) error {
	flags := runner.flags("channel media view")
	session := flags.String("session", "", "workload id of the publisher to watch; defaults to the most recent")
	all := flags.Bool("all", false, "subscribe to every publisher streaming in the channel")
	open := flags.Bool("open", false, "hand each stream to the operating system's default handler")
	local := flags.Bool("sdp", false, "describe each stream as loopback RTP in an SDP file")
	snippets := flags.Bool("snippets", false, "print code for consuming or publishing this stream")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errUsage
	}
	if *all && *session != "" {
		return runner.usageError("channel media view takes --all or --session, not both")
	}
	sessions := []string{*session}
	if *all {
		listing, err := runner.mediaViews(ctx, base)
		if err != nil {
			return err
		}
		if len(listing.Views) == 0 {
			return errors.New("no active room media session is ready")
		}
		sessions = sessions[:0]
		for _, view := range listing.Views {
			sessions = append(sessions, view.WorkloadID)
		}
	}
	for _, workloadID := range sessions {
		if err := runner.viewOneStream(ctx, mode, base, workloadID, *local, *open, *snippets); err != nil {
			return err
		}
	}
	return nil
}

// viewOneStream binds a single publisher. Every stream gets its own session and
// its own loopback ports, so several of these run side by side.
func (runner Runner) viewOneStream(ctx context.Context, mode outputMode, base, workloadID string,
	local, open, snippets bool) error {
	query := ""
	if workloadID != "" {
		query = "?workload_id=" + url.QueryEscape(workloadID)
	}
	if local {
		var egress localStream
		if err := runner.Client.Do(ctx, http.MethodPost, base+"/local"+query, "", nil, &egress); err != nil {
			return err
		}
		return runner.renderMediaLocal(mode, egress, open, snippets)
	}
	var endpoint mediaEndpoint
	if err := runner.Client.Do(ctx, http.MethodGet, base+query, "", nil, &endpoint); err != nil {
		return err
	}
	return runner.renderMediaView(mode, endpoint, open, snippets)
}

func (runner Runner) mediaViews(ctx context.Context, base string) (mediaViewListing, error) {
	var listing mediaViewListing
	if err := runner.Client.Do(ctx, http.MethodGet, base+"/views", "", nil, &listing); err != nil {
		return mediaViewListing{}, err
	}
	return listing, nil
}

func (runner Runner) renderMediaList(mode outputMode, listing mediaViewListing) error {
	if mode.JSON {
		return runner.json(listing)
	}
	if mode.Quiet {
		for _, view := range listing.Views {
			_, _ = fmt.Fprintln(runner.out(), view.WorkloadID)
		}
		return nil
	}
	if len(listing.Views) == 0 {
		_, _ = fmt.Fprintln(runner.out(), "No publisher is streaming in this channel.")
		return nil
	}
	for _, view := range listing.Views {
		source := "remote"
		if view.Publishing {
			source = "this agent"
		}
		if view.Name != "" {
			source += " as " + view.Name
		}
		_, _ = fmt.Fprintf(runner.out(), "%s  %-12s  %s\n", view.WorkloadID, view.State, source)
	}
	_, _ = fmt.Fprintf(runner.out(),
		"\n%d publisher(s) streaming. Watch one with --session SESSION, or every one with --all.\n",
		len(listing.Views))
	return nil
}

func (runner Runner) renderMediaPublish(mode outputMode, endpoint mediaEndpoint, snippets bool) error {
	if mode.JSON {
		return runner.json(endpoint)
	}
	if mode.Quiet {
		_, _ = fmt.Fprintln(runner.out(), endpoint.WHIPURL)
		return nil
	}
	_, _ = fmt.Fprintf(runner.out(), "Publisher: %s\nWHIP URL: %s\nBearer token: %s\n\n"+
		"This URL and token are stable. They stay valid across streams and agent restarts.\n"+
		"The token is valid only for publisher %s.\n",
		endpoint.Name, endpoint.WHIPURL, endpoint.Token, endpoint.Name)
	if snippets {
		_, _ = fmt.Fprint(runner.out(), publishSnippets(endpoint))
	}
	return nil
}

func (runner Runner) renderMediaView(mode outputMode, endpoint mediaEndpoint, open, snippets bool) error {
	if mode.JSON {
		return runner.json(endpoint)
	}
	if mode.Quiet {
		_, _ = fmt.Fprintln(runner.out(), endpoint.PlayerURL)
	} else {
		_, _ = fmt.Fprintf(runner.out(), "Player URL: %s\nWHEP URL: %s\nBearer token: %s\nSession: %s\n",
			endpoint.PlayerURL, endpoint.WHEPURL, endpoint.Token, endpoint.WorkloadID)
	}
	if snippets {
		_, _ = fmt.Fprint(runner.out(), viewSnippets(endpoint))
	}
	if open {
		return openWithDefaultHandler(endpoint.PlayerURL)
	}
	return nil
}

func (runner Runner) renderMediaLocal(mode outputMode, egress localStream, open, snippets bool) error {
	if mode.JSON {
		return runner.json(egress)
	}
	if mode.Quiet {
		_, _ = fmt.Fprintln(runner.out(), egress.URL)
	} else {
		_, _ = fmt.Fprintf(runner.out(), "Stream URL:  %s\nDescription: %s\nSession:     %s\n\n"+
			"The stream is republished as RTP on loopback. Give the URL to any player that\n"+
			"opens a network stream; it is served as application/sdp, which players dispatch\n"+
			"on. The file holds the same description for tooling that takes a path.\n",
			egress.URL, egress.Path, egress.Endpoint.WorkloadID)
	}
	if snippets {
		_, _ = fmt.Fprint(runner.out(), localSnippets(egress))
	}
	if open {
		// The URL is served as application/sdp and players dispatch on that; the
		// same bytes as a bare file are guessed from the name, and at least one
		// player takes them for a playlist and plays nothing.
		return openWithDefaultHandler(egress.URL)
	}
	return nil
}

// openWithDefaultHandler defers the choice of application to the operating
// system so the CLI never depends on a particular player being installed.
func openWithDefaultHandler(target string) error {
	if strings.TrimSpace(target) == "" {
		return fmt.Errorf("nothing to open")
	}
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		// -n forces a fresh instance. Players default to a single window that the
		// next stream replaces, so without it --all opens one stream and discards
		// the rest.
		command = exec.Command("open", "-n", target)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		command = exec.Command("xdg-open", target)
	}
	if err := command.Start(); err != nil {
		return fmt.Errorf("open %s: %w", target, err)
	}
	return nil
}

func publishSnippets(endpoint mediaEndpoint) string {
	return fmt.Sprintf(`
# Publish with ffmpeg
ffmpeg -re -i INPUT -c:v libx264 -profile:v baseline -tune zerolatency \
  -c:a libopus -f whip -authorization %q %q

# Publish with GStreamer
gst-launch-1.0 whipclientsink signaller::whip-endpoint=%q \
  signaller::auth-token=%q name=whip

# Any WHIP client works: POST an SDP offer with
#   Content-Type: application/sdp
#   Authorization: Bearer %s
`, endpoint.Token, endpoint.WHIPURL, endpoint.WHIPURL, endpoint.Token, endpoint.Token)
}

func viewSnippets(endpoint mediaEndpoint) string {
	return fmt.Sprintf(`
# Embed the player in a local page (loopback only, this machine)
<iframe src=%q allow="autoplay" style="border:0;width:100%%;aspect-ratio:16/9"></iframe>

# Consume the WHEP stream directly (loopback only, this machine)
const peer = new RTCPeerConnection();
peer.addTransceiver('video', {direction: 'recvonly'});
peer.addTransceiver('audio', {direction: 'recvonly'});
peer.ontrack = event => { video.srcObject = event.streams[0]; };
await peer.setLocalDescription(await peer.createOffer());
const answer = await fetch(%q, {
  method: 'POST',
  headers: {Authorization: 'Bearer %s', 'Content-Type': 'application/sdp'},
  body: peer.localDescription.sdp,
}).then(response => response.text());
await peer.setRemoteDescription({type: 'answer', sdp: answer});
`, endpoint.PlayerURL, endpoint.WHEPURL, endpoint.Token)
}

func localSnippets(egress localStream) string {
	return fmt.Sprintf(`
# Open the network stream in a player. Prefer the URL: it is served as
# application/sdp, and a player handed the bare file may guess from the name.
#   %s

# Or point your own tooling at the description on disk
ffplay -protocol_whitelist file,udp,rtp %q
ffmpeg -protocol_whitelist file,udp,rtp -i %q -c copy OUTPUT.mkv
`, egress.URL, egress.Path, egress.Path)
}
