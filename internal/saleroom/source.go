package saleroom

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// PackageName is the file name a seller gives a sale package, so the buyer can find it in its inbox.
func PackageName(saleID common.Hash) string {
	return "beam-sale-" + strings.TrimPrefix(saleID.Hex(), "0x") + ".package"
}

// IsPackageName reports whether an inbox object's file name is a sale package's.
func IsPackageName(name string) bool {
	return strings.HasPrefix(name, "beam-sale-") && strings.HasSuffix(name, ".package")
}

// FileSource reads a sale package from a local file.
type FileSource struct {
	File *os.File
	size int64
}

// OpenFileSource opens a package file.
func OpenFileSource(path string) (*FileSource, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &FileSource{File: f, size: info.Size()}, nil
}

func (s *FileSource) ReadAt(p []byte, off int64) (int, error) { return s.File.ReadAt(p, off) }
func (s *FileSource) Size() int64                             { return s.size }
func (s *FileSource) Close() error                            { return s.File.Close() }

func (s *FileSource) Section(offset, length int64) (io.ReadCloser, error) {
	return io.NopCloser(io.NewSectionReader(s.File, offset, length)), nil
}

// Local is the part of the agent's local API the inbox reader uses. *tunnel.LocalClient implements it.
type Local interface {
	Do(ctx context.Context, method, path, idempotencyKey string, body, out any) error
	OpenRange(ctx context.Context, path string, offset, length int64) (*http.Response, error)
}

// InboxObject is one object delivered to the local agent.
type InboxObject struct {
	RoomID      string    `json:"room_id"`
	TransferID  string    `json:"transfer_id"`
	MemberID    string    `json:"member_id"`
	Filename    string    `json:"filename"`
	SizeBytes   int64     `json:"size_bytes"`
	CompletedAt time.Time `json:"completed_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

func inboxPath(roomID string) string { return "/v1/rooms/" + url.PathEscape(roomID) + "/inbox" }

// ListInbox lists the objects delivered to the local agent in a Room.
func ListInbox(ctx context.Context, local Local, roomID string) ([]InboxObject, error) {
	var out struct {
		Objects []InboxObject `json:"objects"`
	}
	if err := local.Do(ctx, http.MethodGet, inboxPath(roomID), "", nil, &out); err != nil {
		return nil, err
	}
	return out.Objects, nil
}

// DeleteInbox removes one delivered object from the local agent's inbox.
func DeleteInbox(ctx context.Context, local Local, roomID, transferID string) error {
	return local.Do(ctx, http.MethodDelete, inboxPath(roomID)+"/"+url.PathEscape(transferID), "", nil, nil)
}

// ErrNoPackage means no sale package has been delivered in the Room yet.
var ErrNoPackage = errors.New("no sale package has been delivered in this Room yet")

// Packages returns the sale packages among delivered objects, newest first.
func Packages(objects []InboxObject) []InboxObject {
	var found []InboxObject
	for _, o := range objects {
		if IsPackageName(o.Filename) {
			found = append(found, o)
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].CompletedAt.After(found[j].CompletedAt) })
	return found
}

// InboxSource reads a delivered sale package from the local agent's inbox through ranged requests, so the buyer
// streams the ciphertext without copying it.
type InboxSource struct {
	Ctx    context.Context
	Local  Local
	Object InboxObject
}

func (s InboxSource) path() string {
	return inboxPath(s.Object.RoomID) + "/" + url.PathEscape(s.Object.TransferID)
}

func (s InboxSource) Size() int64 { return s.Object.SizeBytes }

func (s InboxSource) ReadAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > s.Object.SizeBytes {
		return 0, io.ErrUnexpectedEOF
	}
	body, err := s.Section(off, int64(len(p)))
	if err != nil {
		return 0, err
	}
	defer body.Close()
	return io.ReadFull(body, p)
}

func (s InboxSource) Section(offset, length int64) (io.ReadCloser, error) {
	resp, err := s.Local.OpenRange(s.Ctx, s.path(), offset, length)
	if err != nil {
		return nil, err
	}
	if resp.ContentLength >= 0 && resp.ContentLength != length {
		resp.Body.Close()
		return nil, fmt.Errorf("the agent returned %d bytes of a %d-byte range", resp.ContentLength, length)
	}
	return struct {
		io.Reader
		io.Closer
	}{io.LimitReader(resp.Body, length), resp.Body}, nil
}
