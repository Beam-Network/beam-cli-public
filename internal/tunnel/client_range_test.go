//go:build !windows

package tunnel

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/testutil"
)

func TestOpenRangeReadsPartOfALocalResource(t *testing.T) {
	content := []byte("0123456789abcdefghij")
	socket := testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/rooms/room-a/inbox/t-1":
			http.ServeContent(w, r, "object", time.Time{}, bytes.NewReader(content))
		case "/v1/rooms/room-a/inbox/whole":
			_, _ = w.Write(content) // ignores Range
		default:
			http.NotFound(w, r)
		}
	}))
	client := NewLocalClient(socket)
	for _, tc := range []struct {
		offset, length int64
		want           string
	}{{10, 5, "abcde"}, {0, 3, "012"}, {15, -1, "fghij"}} {
		resp, err := client.OpenRange(context.Background(), "/v1/rooms/room-a/inbox/t-1", tc.offset, tc.length)
		if err != nil {
			t.Fatalf("OpenRange(%d, %d) = %v", tc.offset, tc.length, err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(got) != tc.want {
			t.Fatalf("OpenRange(%d, %d) = %q; want %q", tc.offset, tc.length, got, tc.want)
		}
	}
	_, err := client.OpenRange(context.Background(), "/v1/rooms/room-a/inbox/whole", 10, 5)
	var tunnelErr *Error
	if !errors.As(err, &tunnelErr) || tunnelErr.Kind != ErrResponse {
		t.Fatalf("a resource that ignores Range: err = %v; want a response error", err)
	}
}
