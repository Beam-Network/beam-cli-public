// Package saleroom carries a data sale over a Beam Room: the buyer's binding of its Room member to its wallet, the
// sale package the seller delivers, reading that package from the agent's inbox, and the rule that a sale Room holds
// only the seller and the buyer.
package saleroom

import (
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/datasale"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

// BindingKind identifies a buyer binding document.
const BindingKind = "beam.data-sale.buyer-binding.v1"

// Binding is the buyer's signed statement that its wallet buys through one Room member. The seller checks it before
// naming the wallet in an offer, so the encrypted dataset only ever goes to the member whose owner will pay.
type Binding struct {
	Kind      string         `json:"kind"`
	RoomID    string         `json:"room_id"`
	MemberID  string         `json:"member_id"`
	Buyer     common.Address `json:"buyer"`
	ChainID   *big.Int       `json:"chain_id"`
	Escrow    common.Address `json:"escrow"`
	IssuedAt  time.Time      `json:"issued_at"`
	Signature hexutil.Bytes  `json:"signature"`
}

// ErrBinding reports a binding that does not hold for this sale.
var ErrBinding = errors.New("the buyer binding is not valid for this sale")

// message is the text the buyer's wallet signs, readable in any wallet that shows personal messages.
func (b Binding) message() string {
	return strings.Join([]string{
		"Beam data sale: buyer binding",
		"room: " + b.RoomID,
		"member: " + b.MemberID,
		"buyer: " + b.Buyer.Hex(),
		"chain: " + b.ChainID.String(),
		"escrow: " + b.Escrow.Hex(),
		"issued: " + b.IssuedAt.UTC().Format(time.RFC3339),
	}, "\n")
}

// SignBinding binds member in room to the wallet key, for the escrow on chain.
func SignBinding(key *ecdsa.PrivateKey, domain datasale.Domain, roomID, memberID string, now time.Time) (Binding, error) {
	b := Binding{
		Kind: BindingKind, RoomID: roomID, MemberID: memberID, ChainID: domain.ChainID, Escrow: domain.Escrow,
		IssuedAt: now.UTC().Truncate(time.Second),
	}
	b.Buyer = addressOf(key)
	sig, err := datasale.SignDigest(key, common.BytesToHash(accounts.TextHash([]byte(b.message()))))
	if err != nil {
		return Binding{}, err
	}
	b.Signature = sig
	return b, nil
}

// Verify checks that the binding was signed by its buyer for this room and escrow. It does not check membership;
// see CheckRoom.
func (b Binding) Verify(domain datasale.Domain, roomID string) error {
	switch {
	case b.Kind != BindingKind:
		return fmt.Errorf("%w: unknown kind %q", ErrBinding, b.Kind)
	case b.RoomID != roomID:
		return fmt.Errorf("%w: it is for room %s", ErrBinding, b.RoomID)
	case b.ChainID == nil || b.ChainID.Cmp(domain.ChainID) != 0:
		return fmt.Errorf("%w: it is for chain %v", ErrBinding, b.ChainID)
	case b.Escrow != domain.Escrow:
		return fmt.Errorf("%w: it is for escrow %s", ErrBinding, b.Escrow.Hex())
	case b.MemberID == "":
		return fmt.Errorf("%w: it names no member", ErrBinding)
	}
	signer, err := datasale.RecoverSigner(common.BytesToHash(accounts.TextHash([]byte(b.message()))), b.Signature)
	if err != nil || signer != b.Buyer {
		return fmt.Errorf("%w: the signature is not the buyer's", ErrBinding)
	}
	return nil
}

// ParseBinding decodes a binding document.
func ParseBinding(data []byte) (Binding, error) {
	var b Binding
	if err := json.Unmarshal(data, &b); err != nil {
		return Binding{}, fmt.Errorf("%w: %v", ErrBinding, err)
	}
	return b, nil
}

// Member is the part of a Room membership the sale rule reads.
type Member struct {
	MemberID string
	Kind     string // "agent", or "" in older records; "object_storage" or "webhook" for other members
}

// agentKind is the membership kind of an agent member.
const agentKind = "agent"

// ErrRoomNotPrivate means the Room holds a member other than the seller and the bound buyer.
var ErrRoomNotPrivate = errors.New("the sale Room must hold only the seller and the buyer")

// CheckRoom enforces the two-member rule: after the key is revealed anyone holding the ciphertext can read the data,
// so it may reach only the buyer. Every membership counts, whatever its state, because an expired or suspended member
// can return. Every member must be an agent, and the members must be exactly the seller and the buyer the binding
// names.
func CheckRoom(members []Member, sellerMemberID string, b Binding) error {
	var sawSeller, sawBuyer bool
	for _, m := range members {
		switch {
		case m.Kind != "" && m.Kind != agentKind:
			return fmt.Errorf("%w: member %s is a %s member", ErrRoomNotPrivate, m.MemberID, m.Kind)
		case m.MemberID == sellerMemberID:
			sawSeller = true
		case m.MemberID == b.MemberID:
			sawBuyer = true
		default:
			return fmt.Errorf("%w: member %s is neither", ErrRoomNotPrivate, m.MemberID)
		}
	}
	if !sawSeller || !sawBuyer {
		return fmt.Errorf("%w: the buyer %s has not joined", ErrRoomNotPrivate, b.MemberID)
	}
	return nil
}

// DeliveryTTL is how long the seller's object channel lets a delivery run: 30 minutes plus the time the file takes at
// 4 MB/s. The publication must complete within it.
func DeliveryTTL(sizeBytes uint64) time.Duration {
	return 30*time.Minute + time.Duration(sizeBytes/(4<<20))*time.Second
}

func addressOf(key *ecdsa.PrivateKey) common.Address {
	return crypto.PubkeyToAddress(key.PublicKey)
}
