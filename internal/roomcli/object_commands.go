package roomcli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func (runner Runner) channelObject(ctx context.Context, mode outputMode, args []string) error {
	if len(args) < 3 {
		return runner.usageError("channel object requires ACTION ROOM_ID CHANNEL_ID")
	}
	action, roomID, channelID, rest := args[0], args[1], args[2], args[3:]
	base := roomPath(roomID) + "/channels/" + url.PathEscape(channelID) + "/objects"
	switch action {
	case "publish":
		flags := runner.flags("channel object publish")
		file := flags.String("file", "", "local regular source file")
		from := flags.String("from", "", "delegated object-storage source member id")
		objectKey := flags.String("object-key", "", "object key on the delegated bucket source")
		var targets repeatedValues
		flags.Var(&targets, "to", "target room member id; repeat to select a subset")
		ttl := flags.Int64("ttl", 300, "publication lifetime in seconds")
		idempotencyKey := flags.String("idempotency-key", "", "stable local publication retry key")
		allowPartial := flags.Bool("allow-partial", false, "complete when at least one selected recipient succeeds")
		follow := flags.Bool("follow", false, "poll status until the transfer becomes terminal")
		interval := flags.Duration("interval", time.Second, "status polling interval used with --follow")
		if err := flags.Parse(rest); err != nil || flags.NArg() != 0 {
			return runner.usageError("object publish accepts --file or --from MEMBER_ID --object-key KEY")
		}
		localSource := strings.TrimSpace(*file) != ""
		bucketSource := strings.TrimSpace(*from) != "" || strings.TrimSpace(*objectKey) != ""
		if localSource == bucketSource || (bucketSource && (strings.TrimSpace(*from) == "" || strings.TrimSpace(*objectKey) == "")) {
			return runner.usageError("object publish requires exactly one source: --file or --from MEMBER_ID --object-key KEY")
		}
		if *interval < 250*time.Millisecond {
			return runner.usageError("object publish --interval must be at least 250ms")
		}
		key := strings.TrimSpace(*idempotencyKey)
		if key == "" {
			key = newObjectIdempotencyKey()
		}
		body := map[string]any{"file": *file, "source_member_id": *from, "object_key": *objectKey,
			"target_member_ids": []string(targets), "ttl_seconds": *ttl, "allow_partial": *allowPartial,
			"idempotency_key": key}
		var response map[string]any
		if err := runner.Client.DoLong(ctx, http.MethodPost, base+"/publish", "", body, &response); err != nil {
			return err
		}
		publicationID := mapStringFlat(response, "publication_id")
		if !*follow {
			return runner.renderObjectSnapshot(mode, response, publicationID)
		}
		return runner.followObject(ctx, mode, base+"/"+url.PathEscape(publicationID)+"/status", publicationID, *interval, response, true, *allowPartial)
	case "list":
		if len(rest) != 0 {
			return errUsage
		}
		var response map[string]any
		if err := runner.Client.Do(ctx, http.MethodGet, base, "", nil, &response); err != nil {
			return err
		}
		return runner.renderGeneric(mode, response, channelID)
	case "status":
		if len(rest) < 1 {
			return runner.usageError("object status requires PUBLICATION_ID")
		}
		publicationID := rest[0]
		flags := runner.flags("channel object status")
		follow := flags.Bool("follow", false, "poll status until the transfer becomes terminal")
		interval := flags.Duration("interval", time.Second, "status polling interval used with --follow")
		if err := flags.Parse(rest[1:]); err != nil || flags.NArg() != 0 || *interval < 250*time.Millisecond {
			return runner.usageError("object status accepts PUBLICATION_ID [--follow] [--interval DURATION]")
		}
		statusURL := base + "/" + url.PathEscape(publicationID) + "/status"
		var response map[string]any
		if err := runner.Client.Do(ctx, http.MethodGet, statusURL, "", nil, &response); err != nil {
			return err
		}
		if !*follow || objectTerminal(response) {
			return runner.renderObjectSnapshot(mode, response, publicationID)
		}
		return runner.followObject(ctx, mode, statusURL, publicationID, *interval, response, false, false)
	case "cancel":
		if len(rest) != 1 {
			return runner.usageError("object cancel requires PUBLICATION_ID")
		}
		var response map[string]any
		if err := runner.Client.Do(ctx, http.MethodPost, base+"/"+url.PathEscape(rest[0])+"/cancel", "", nil, &response); err != nil {
			return err
		}
		return runner.renderGeneric(mode, response, rest[0])
	default:
		return runner.usageError("unknown channel object command " + action)
	}
}

func (runner Runner) followObject(ctx context.Context, mode outputMode, statusURL, publicationID string, interval time.Duration, initial map[string]any, requireSuccess, allowPartial bool) error {
	interactive := !mode.JSON && !mode.Quiet && objectOutputInteractive(runner.out())
	previousLines := 0
	if interactive {
		snapshot, err := runner.objectSnapshotText(initial, publicationID)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprint(runner.out(), snapshot)
		previousLines = strings.Count(snapshot, "\n")
	} else if err := runner.renderObjectSnapshot(mode, initial, publicationID); err != nil {
		return err
	}
	if objectTerminal(initial) {
		if requireSuccess {
			return objectFollowResult(initial, allowPartial)
		}
		return nil
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	encodedInitial, _ := json.Marshal(initial)
	last := string(encodedInitial)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			var response map[string]any
			if err := runner.Client.Do(ctx, http.MethodGet, statusURL, "", nil, &response); err != nil {
				return err
			}
			encoded, _ := json.Marshal(response)
			if string(encoded) != last && !mode.Quiet {
				if interactive {
					snapshot, renderErr := runner.objectSnapshotText(response, publicationID)
					if renderErr != nil {
						return renderErr
					}
					if previousLines > 0 {
						_, _ = fmt.Fprintf(runner.out(), "\x1b[%dA\r\x1b[J", previousLines)
					}
					_, _ = fmt.Fprint(runner.out(), snapshot)
					previousLines = strings.Count(snapshot, "\n")
				} else if !mode.JSON {
					_, _ = fmt.Fprintln(runner.out())
					if err := runner.renderObjectSnapshot(mode, response, publicationID); err != nil {
						return err
					}
				} else if err := runner.renderObjectSnapshot(mode, response, publicationID); err != nil {
					return err
				}
				last = string(encoded)
			}
			if objectTerminal(response) {
				if requireSuccess {
					return objectFollowResult(response, allowPartial)
				}
				return nil
			}
		}
	}
}

func objectFollowResult(response map[string]any, allowPartial bool) error {
	view := objectStatusView(response)
	state := objectString(objectMap(view, "room_transfer"), "status")
	if state == "" {
		state = objectString(view, "state")
	}
	switch state {
	case "completed":
		return nil
	case "partial":
		if allowPartial {
			return nil
		}
		return fmt.Errorf("room object publication only partially delivered under strict completion")
	case "failed", "cancelled", "expired", "rejected":
		return fmt.Errorf("room object publication %s", state)
	default:
		return nil
	}
}

func (runner Runner) objectSnapshotText(response map[string]any, publicationID string) (string, error) {
	var output bytes.Buffer
	copy := runner
	copy.Out = &output
	if err := copy.renderObjectSnapshot(outputMode{}, response, publicationID); err != nil {
		return "", err
	}
	return output.String(), nil
}

func objectOutputInteractive(writer any) bool {
	file, ok := writer.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func (runner Runner) renderObjectSnapshot(mode outputMode, response map[string]any, publicationID string) error {
	if mode.JSON {
		return runner.json(response)
	}
	if mode.Quiet {
		_, _ = fmt.Fprintln(runner.out(), publicationID)
		return nil
	}
	view := objectStatusView(response)
	state := objectString(view, "state")
	transfer := objectMap(view, "room_transfer")
	if transferState := objectString(transfer, "status"); transferState != "" {
		state = transferState
	}
	if state == "" {
		state = objectString(response, "state")
	}
	filename := objectString(transfer, "filename")
	file := objectMap(transfer, "file")
	chunkCount := objectInt(file, "chunk_count")
	fileBytes := objectInt(file, "size_bytes")
	if chunkCount == 0 {
		chunkCount = objectInt(response, "chunk_count")
		fileBytes = objectInt(response, "file_bytes")
	}
	_, _ = fmt.Fprintf(runner.out(), "Transfer  %s\nState     %s\n", publicationID, firstObjectValue(state, "unknown"))
	if runtimeID := objectString(transfer, "transfer_id"); runtimeID != "" {
		_, _ = fmt.Fprintf(runner.out(), "Runtime   %s\n", runtimeID)
	}
	protection := objectMap(transfer, "protection")
	switch objectString(protection, "scheme") {
	case "btr.object.chunk.aead.v1":
		_, _ = fmt.Fprintln(runner.out(), "Protection Room MLS E2EE")
	case "btr.object.transport.tls.v1":
		_, _ = fmt.Fprintln(runner.out(), "Protection TLS; workers handle plaintext for every recipient")
	}
	if chunkSize := objectInt(file, "chunk_size_bytes"); chunkSize > 0 {
		_, _ = fmt.Fprintf(runner.out(), "Chunk size %s\n", formatObjectBytes(chunkSize))
	}
	if filename != "" {
		_, _ = fmt.Fprintf(runner.out(), "File      %s\n", filename)
	}
	if fileBytes > 0 {
		_, _ = fmt.Fprintf(runner.out(), "Size      %s\n", formatObjectBytes(fileBytes))
	}
	if chunkCount > 0 {
		_, _ = fmt.Fprintf(runner.out(), "Chunks    %d\n", chunkCount)
	}
	deliveries := objectDeliveries(view)
	if len(deliveries) > 0 {
		coverageAvailable := false
		_, _ = fmt.Fprintln(runner.out(), "\nDestinations")
		for _, delivery := range deliveries {
			memberID := objectString(delivery, "member_id")
			deliveryState := objectString(delivery, "state")
			grid, completed, available := objectASCIIGrid(delivery, chunkCount)
			coverageAvailable = coverageAvailable || available
			_, _ = fmt.Fprintf(runner.out(), "%-28s [%s]", memberID, grid)
			if available {
				_, _ = fmt.Fprintf(runner.out(), " %d/%d", completed, chunkCount)
			}
			_, _ = fmt.Fprintf(runner.out(), " %s", deliveryState)
			if reason := objectString(delivery, "unavailable_reason"); reason != "" {
				_, _ = fmt.Fprintf(runner.out(), " (%s)", reason)
			}
			_, _ = fmt.Fprintln(runner.out())
		}
		if coverageAvailable {
			_, _ = fmt.Fprintln(runner.out(), "Legend: # delivered  + partial  . pending  ! failed")
		} else {
			_, _ = fmt.Fprintln(runner.out(), "Legend: # delivered  ! failed  ? chunk coverage unavailable")
		}
	}
	return nil
}

func objectStatusView(response map[string]any) map[string]any {
	status := objectMap(response, "status")
	if publisher := objectMap(status, "publisher"); len(publisher) > 0 {
		return publisher
	}
	if recipient := objectMap(status, "recipient"); len(recipient) > 0 {
		return recipient
	}
	return response
}

func objectDeliveries(view map[string]any) []map[string]any {
	if raw, ok := view["deliveries"].([]any); ok {
		result := make([]map[string]any, 0, len(raw))
		for _, value := range raw {
			if delivery, ok := value.(map[string]any); ok {
				result = append(result, delivery)
			}
		}
		return result
	}
	if delivery := objectMap(view, "delivery"); len(delivery) > 0 {
		return []map[string]any{delivery}
	}
	return nil
}

func objectTerminal(response map[string]any) bool {
	view := objectStatusView(response)
	state := objectString(view, "state")
	if transfer := objectMap(view, "room_transfer"); objectString(transfer, "status") != "" {
		state = objectString(transfer, "status")
	}
	switch state {
	case "completed", "partial", "failed", "cancelled", "expired", "rejected":
		return true
	default:
		return false
	}
}

func objectASCIIGrid(delivery map[string]any, chunkCount int64) (string, int64, bool) {
	width := int64(24)
	if chunkCount > 0 && chunkCount < width {
		width = chunkCount
	}
	state := objectString(delivery, "state")
	encoded := objectString(delivery, "coverage_base64")
	if encoded != "" && chunkCount > 0 {
		coverage, err := base64.StdEncoding.DecodeString(encoded)
		if err == nil && int64(len(coverage)) == (chunkCount+7)/8 {
			completed := objectInt(delivery, "completed_chunks")
			grid := make([]byte, width)
			for cell := int64(0); cell < width; cell++ {
				start := cell * chunkCount / width
				end := (cell+1)*chunkCount/width - 1
				covered := int64(0)
				for chunk := start; chunk <= end; chunk++ {
					if coverage[chunk/8]&(1<<uint(chunk%8)) != 0 {
						covered++
					}
				}
				grid[cell] = '.'
				if covered == end-start+1 {
					grid[cell] = '#'
				} else if covered > 0 {
					grid[cell] = '+'
				} else if state == "failed" || state == "expired" || state == "unavailable" {
					grid[cell] = '!'
				}
			}
			return string(grid), completed, true
		}
	}
	character := byte('?')
	switch state {
	case "delivered", "completed":
		character = '#'
	case "failed", "expired", "unavailable":
		character = '!'
	}
	return strings.Repeat(string(character), int(width)), 0, false
}

func objectMap(value map[string]any, key string) map[string]any {
	result, _ := value[key].(map[string]any)
	return result
}

func objectString(value map[string]any, key string) string {
	result, _ := value[key].(string)
	return result
}

func objectInt(value map[string]any, key string) int64 {
	switch result := value[key].(type) {
	case float64:
		return int64(result)
	case int64:
		return result
	default:
		return 0
	}
}

func formatObjectBytes(value int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	amount := float64(value)
	unit := 0
	for amount >= 1024 && unit < len(units)-1 {
		amount /= 1024
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%d %s", value, units[unit])
	}
	return fmt.Sprintf("%.1f %s", amount, units[unit])
}

func firstObjectValue(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

type repeatedValues []string

func newObjectIdempotencyKey() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return fmt.Sprintf("room-object-%d", time.Now().UnixNano())
	}
	return "room-object-" + hex.EncodeToString(value[:])
}

func (values *repeatedValues) String() string { return strings.Join(*values, ",") }
func (values *repeatedValues) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return flag.ErrHelp
	}
	*values = append(*values, value)
	return nil
}
