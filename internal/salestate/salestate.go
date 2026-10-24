// Package salestate keeps what each party's client needs to carry a data sale to its end across restarts: the sale
// Rooms this machine set up or joined, and one record per sale with the signed terms, the local files and every
// transaction broadcast for it. Nothing here is secret: the seller's sale key derives from the wallet secret and the
// sale id, and is recomputed when needed.
package salestate

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/auth"
	"github.com/Beam-Network/beam-cli-public/internal/escrow"
	"github.com/Beam-Network/beam-cli-public/internal/saleroom"
	"github.com/ethereum/go-ethereum/common"
)

const recordVersion = 1

// Roles a party plays in a sale or a sale Room.
const (
	Seller = "seller"
	Buyer  = "buyer"
)

// Phases of a sale on this machine. The escrow's state is authoritative; the phase records what this client has done
// that the chain does not show.
const (
	PhaseDelivering     = "delivering"      // seller: the package is being delivered
	PhaseDeliveryFailed = "delivery_failed" // seller: the package did not reach the buyer
	PhaseOffered        = "offered"         // seller: delivered, waiting for funding
	PhaseChecked        = "checked"         // buyer: the package passed every check
	PhaseFunding        = "funding"         // buyer: funding was broadcast
	PhaseVerified       = "verified"        // buyer: every leaf decrypted to its committed hash
	PhaseMismatch       = "mismatch"        // buyer: some leaves did not, so the sale is challenged
	PhaseEnded          = "ended"           // the sale reached a final state; Outcome says which
)

// Outcomes of an ended sale.
const (
	OutcomeSettled  = "settled"
	OutcomeRefunded = "refunded"
	OutcomeRevoked  = "revoked"
	OutcomeExpired  = "expired"
	OutcomeFailed   = "delivery_failed"
)

// Room is a sale Room this machine created as the seller or joined as the buyer.
type Room struct {
	RoomID    string    `json:"room_id"`
	Role      string    `json:"role"`
	ChannelID string    `json:"channel_id,omitempty"` // seller: the object channel the package goes on
	MemberID  string    `json:"member_id"`            // this machine's member
	CreatedAt time.Time `json:"created_at"`
	// Delivered counts the packages the seller published in the Room; the first delivery carries the Room's start
	// charge in the delivery budget.
	Delivered int `json:"delivered,omitempty"`
}

// Sale is one sale as this party sees it.
type Sale struct {
	Version  int            `json:"version"`
	SaleID   common.Hash    `json:"sale_id"`
	Role     string         `json:"role"`
	ChainID  *big.Int       `json:"chain_id"`
	Escrow   common.Address `json:"escrow"`
	RoomID   string         `json:"room_id"`
	MemberID string         `json:"buyer_member_id"`
	// Header is the package header: the signed offer and its commitments.
	Header saleroom.Header `json:"header"`
	// StartBlock is a block before the sale could be funded; event scans start there.
	StartBlock uint64 `json:"start_block"`
	Phase      string `json:"phase"`
	Outcome    string `json:"outcome,omitempty"`

	// Seller: the package file and its publication in the Room.
	Package       string `json:"package,omitempty"`
	PublicationID string `json:"publication_id,omitempty"`
	// Buyer: the delivered object, the trees a challenge needs, and where the dataset is written.
	TransferID string   `json:"transfer_id,omitempty"`
	TreeDir    string   `json:"tree_dir,omitempty"`
	Output     string   `json:"output,omitempty"`
	Mismatches []uint64 `json:"mismatches,omitempty"`
	// ChallengeLeaf and ChallengePlainHash are the first mismatching leaf's ciphertext and committed hash, kept when
	// decrypting so a challenge needs nothing from the agent's inbox.
	ChallengeLeaf      []byte      `json:"challenge_leaf,omitempty"`
	ChallengePlainHash common.Hash `json:"challenge_plain_hash,omitempty"`

	// Transactions lists every transaction broadcast for the sale, by action, so an interrupted client resumes them
	// instead of sending new ones.
	Transactions map[string][]escrow.Attempt `json:"transactions,omitempty"`
	CreatedAt    time.Time                   `json:"created_at"`
	UpdatedAt    time.Time                   `json:"updated_at"`
}

// Ended reports whether the sale reached a final state.
func (s Sale) Ended() bool { return s.Phase == PhaseEnded }

// Store keeps sale Rooms and sales under Dir.
type Store struct {
	Dir string
}

func (s Store) salesDir() string { return filepath.Join(s.Dir, "sales") }
func (s Store) roomsDir() string { return filepath.Join(s.Dir, "sale-rooms") }

// SaleDir is the directory that holds a sale's record and working files.
func (s Store) SaleDir(saleID common.Hash) string {
	return filepath.Join(s.salesDir(), strings.TrimPrefix(saleID.Hex(), "0x"))
}

func (s Store) salePath(saleID common.Hash) string {
	return filepath.Join(s.SaleDir(saleID), "sale.json")
}

func (s Store) roomPath(roomID string) (string, error) {
	if roomID == "" || roomID != filepath.Base(roomID) || strings.ContainsAny(roomID, `/\.`) {
		return "", fmt.Errorf("invalid Room id %q", roomID)
	}
	return filepath.Join(s.roomsDir(), roomID+".json"), nil
}

// ErrNotFound means no record exists.
var ErrNotFound = errors.New("no such sale record")

// SaveRoom writes a sale Room record.
func (s Store) SaveRoom(r Room) error {
	path, err := s.roomPath(r.RoomID)
	if err != nil {
		return err
	}
	return writeJSON(path, r)
}

// LoadRoom reads a sale Room record.
func (s Store) LoadRoom(roomID string) (Room, error) {
	path, err := s.roomPath(roomID)
	if err != nil {
		return Room{}, err
	}
	var r Room
	return r, readJSON(path, &r)
}

// Save writes a sale record, stamping its update time.
func (s Store) Save(sale *Sale) error {
	sale.Version = recordVersion
	now := time.Now().UTC()
	if sale.CreatedAt.IsZero() {
		sale.CreatedAt = now
	}
	sale.UpdatedAt = now
	return writeJSON(s.salePath(sale.SaleID), sale)
}

// Load reads a sale record.
func (s Store) Load(saleID common.Hash) (Sale, error) {
	var sale Sale
	if err := readJSON(s.salePath(saleID), &sale); err != nil {
		return Sale{}, err
	}
	if sale.Version != recordVersion {
		return Sale{}, fmt.Errorf("sale record version %d is not supported", sale.Version)
	}
	return sale, nil
}

// Find resolves a sale id, or a unique prefix of one, to its record.
func (s Store) Find(id string) (Sale, error) {
	prefix := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(id), "0x"))
	if prefix == "" {
		return Sale{}, ErrNotFound
	}
	entries, err := os.ReadDir(s.salesDir())
	if errors.Is(err, os.ErrNotExist) {
		return Sale{}, ErrNotFound
	}
	if err != nil {
		return Sale{}, err
	}
	var match string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
			if match != "" {
				return Sale{}, fmt.Errorf("sale id prefix %q matches more than one sale", id)
			}
			match = e.Name()
		}
	}
	if match == "" {
		return Sale{}, ErrNotFound
	}
	return s.Load(common.HexToHash(match))
}

// List returns every sale record, newest first.
func (s Store) List() ([]Sale, error) {
	entries, err := os.ReadDir(s.salesDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sales []Sale
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sale, err := s.Load(common.HexToHash(e.Name()))
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		sales = append(sales, sale)
	}
	sort.Slice(sales, func(i, j int) bool { return sales[i].CreatedAt.After(sales[j].CreatedAt) })
	return sales, nil
}

// AddAttempt records a broadcast transaction for action and saves the record.
func (s Store) AddAttempt(sale *Sale, action string, attempt escrow.Attempt) error {
	if sale.Transactions == nil {
		sale.Transactions = map[string][]escrow.Attempt{}
	}
	sale.Transactions[action] = append(sale.Transactions[action], attempt)
	return s.Save(sale)
}

func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return auth.WriteRestrictedFile(path, append(data, '\n'))
}

func readJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	return nil
}
