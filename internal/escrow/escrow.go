// Package escrow talks to the data-sale escrow contract on Bittensor EVM: calldata for every function, the read
// calls, event decoding, RPC failover, and a transaction sender built to get time-critical transactions included.
package escrow

import (
	"context"
	_ "embed"
	"fmt"
	"math/big"
	"strings"

	"github.com/Beam-Network/beam-cli-public/internal/datasale"
	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// abiJSON is the ABI of the frozen v1 contract.
//
//go:embed abi.json
var abiJSON string

var contractABI = func() abi.ABI {
	parsed, err := abi.JSON(strings.NewReader(abiJSON))
	if err != nil {
		panic(fmt.Sprintf("escrow ABI: %v", err))
	}
	return parsed
}()

// State is a sale record's state, numbered as in the contract.
type State uint8

const (
	StateNone State = iota
	StateFunded
	StateKeyReleased
	StateSettled
	StateRefunded
	StateRevoked
)

func (s State) String() string {
	switch s {
	case StateNone:
		return "none"
	case StateFunded:
		return "funded"
	case StateKeyReleased:
		return "key_released"
	case StateSettled:
		return "settled"
	case StateRefunded:
		return "refunded"
	case StateRevoked:
		return "revoked"
	}
	return fmt.Sprintf("state_%d", uint8(s))
}

// Record is a sale's onchain record: the hash of its funded commitments, its current deadline and its state.
type Record struct {
	Commitments common.Hash
	Deadline    uint64
	State       State
}

// Commitments are the ciphertext root and the key commitment the seller signed and the buyer funded.
type Commitments struct {
	CipherRoot    common.Hash `json:"cipherRoot"`
	KeyCommitment common.Hash `json:"keyCommitment"`
}

// Hash is H(enc(R_c, K)), what the contract stores at funding.
func (c Commitments) Hash() common.Hash {
	return crypto.Keccak256Hash(c.CipherRoot[:], c.KeyCommitment[:])
}

// LeafProof is one ciphertext leaf and the proofs a challenge carries.
type LeafProof struct {
	Index       uint64
	CipherLeaf  []byte
	PlainHash   common.Hash
	CipherProof []common.Hash
	PlainProof  []common.Hash
}

// abiOffer and abiProof mirror the contract's tuples for packing.
type abiOffer struct {
	Seller          common.Address
	Buyer           common.Address
	PlainRoot       [32]byte
	SizeBytes       uint64
	Price           *big.Int
	FundDeadline    uint64
	RevealPeriod    uint64
	ChallengePeriod uint64
	Nonce           *big.Int
}

type abiProof struct {
	Index       *big.Int
	CipherLeaf  []byte
	PlainHash   [32]byte
	CipherProof [][32]byte
	PlainProof  [][32]byte
}

func toABIOffer(o datasale.Offer) abiOffer {
	return abiOffer{o.Seller, o.Buyer, o.PlainRoot, o.SizeBytes, o.Price, o.FundDeadline, o.RevealPeriod, o.ChallengePeriod, o.Nonce}
}

func hashes(hs []common.Hash) [][32]byte {
	out := make([][32]byte, len(hs))
	for i, h := range hs {
		out[i] = h
	}
	return out
}

// FundData is the calldata of fund(offer, cipherRoot, keyCommitment, signature).
func FundData(o datasale.Offer, c Commitments, signature []byte) ([]byte, error) {
	return contractABI.Pack("fund", toABIOffer(o), [32]byte(c.CipherRoot), [32]byte(c.KeyCommitment), signature)
}

// RevealData is the calldata of revealKey(offer, cipherRoot, keyCommitment, key).
func RevealData(o datasale.Offer, c Commitments, key common.Hash) ([]byte, error) {
	return contractABI.Pack("revealKey", toABIOffer(o), [32]byte(c.CipherRoot), [32]byte(c.KeyCommitment), [32]byte(key))
}

// ChallengeData is the calldata of challenge(offer, cipherRoot, keyCommitment, key, proof).
func ChallengeData(o datasale.Offer, c Commitments, key common.Hash, p LeafProof) ([]byte, error) {
	proof := abiProof{new(big.Int).SetUint64(p.Index), p.CipherLeaf, p.PlainHash, hashes(p.CipherProof), hashes(p.PlainProof)}
	return contractABI.Pack("challenge", toABIOffer(o), [32]byte(c.CipherRoot), [32]byte(c.KeyCommitment), [32]byte(key), proof)
}

// SettleData, RefundUnrevealedData and RevokeData take only the offer.
func SettleData(o datasale.Offer) ([]byte, error) { return contractABI.Pack("settle", toABIOffer(o)) }

func RefundUnrevealedData(o datasale.Offer) ([]byte, error) {
	return contractABI.Pack("refundUnrevealed", toABIOffer(o))
}

func RevokeData(o datasale.Offer) ([]byte, error) { return contractABI.Pack("revoke", toABIOffer(o)) }

// WithdrawData is the calldata of withdraw().
func WithdrawData() ([]byte, error) { return contractABI.Pack("withdraw") }

// Contract reads one deployed escrow.
type Contract struct {
	Address common.Address
	Backend Backend
}

func (c Contract) call(ctx context.Context, block *big.Int, method string, args ...any) ([]any, error) {
	data, err := contractABI.Pack(method, args...)
	if err != nil {
		return nil, err
	}
	raw, err := c.Backend.CallContract(ctx, ethereum.CallMsg{To: &c.Address, Data: data}, block)
	if err != nil {
		return nil, err
	}
	out, err := contractABI.Unpack(method, raw)
	if err != nil {
		return nil, err
	}
	if len(out) != 1 {
		return nil, fmt.Errorf("%s returned %d values", method, len(out))
	}
	return out, nil
}

// Record reads a sale's record at the latest block.
func (c Contract) Record(ctx context.Context, saleID common.Hash) (Record, error) {
	return c.RecordAt(ctx, saleID, nil)
}

// RecordAt reads a sale's record as of block, or the latest block when block is nil. Reading at a block a caller
// already holds keeps the record consistent with it, whichever endpoint answers.
func (c Contract) RecordAt(ctx context.Context, saleID common.Hash, block *big.Int) (Record, error) {
	out, err := c.call(ctx, block, "getRecord", [32]byte(saleID))
	if err != nil {
		return Record{}, err
	}
	record := *abi.ConvertType(out[0], new(struct {
		Commitments [32]byte
		Deadline    uint64
		State       uint8
	})).(*struct {
		Commitments [32]byte
		Deadline    uint64
		State       uint8
	})
	return Record{Commitments: record.Commitments, Deadline: record.Deadline, State: State(record.State)}, nil
}

// Credit reads what the escrow holds for account after a payment it could not send directly.
func (c Contract) Credit(ctx context.Context, account common.Address) (*big.Int, error) {
	out, err := c.call(ctx, nil, "balances", account)
	if err != nil {
		return nil, err
	}
	return out[0].(*big.Int), nil
}

// Limits are the deployment's immutable bounds on offers.
type Limits struct {
	MaxPrice           *big.Int
	MinRevealPeriod    uint64
	MinChallengePeriod uint64
}

// Limits reads the deployment's bounds.
func (c Contract) Limits(ctx context.Context) (Limits, error) {
	maxPrice, err := c.call(ctx, nil, "maxPrice")
	if err != nil {
		return Limits{}, err
	}
	minReveal, err := c.call(ctx, nil, "minRevealPeriod")
	if err != nil {
		return Limits{}, err
	}
	minChallenge, err := c.call(ctx, nil, "minChallengePeriod")
	if err != nil {
		return Limits{}, err
	}
	return Limits{MaxPrice: maxPrice[0].(*big.Int), MinRevealPeriod: minReveal[0].(uint64), MinChallengePeriod: minChallenge[0].(uint64)}, nil
}

// Event is one decoded escrow event.
type Event struct {
	Name        string
	SaleID      common.Hash
	Key         common.Hash    // KeyRevealed
	Deadline    uint64         // Funded: the reveal deadline; KeyRevealed: the challenge deadline
	Commitments Commitments    // Funded
	Account     common.Address // Paid, Credited, Withdrawn
	Amount      *big.Int       // Paid, Credited, Withdrawn
	Index       uint64         // Challenged
	BlockNumber uint64
	TxHash      common.Hash
}

// DecodeEvents decodes the escrow's events in logs, skipping logs from other contracts.
func DecodeEvents(address common.Address, logs []*types.Log) ([]Event, error) {
	var events []Event
	for _, l := range logs {
		if l.Address != address || len(l.Topics) == 0 {
			continue
		}
		spec, err := contractABI.EventByID(l.Topics[0])
		if err != nil {
			continue
		}
		values := map[string]any{}
		if err := contractABI.UnpackIntoMap(values, spec.Name, l.Data); err != nil {
			return nil, fmt.Errorf("decode %s: %w", spec.Name, err)
		}
		e := Event{Name: spec.Name, BlockNumber: l.BlockNumber, TxHash: l.TxHash}
		switch spec.Name {
		case "Funded", "KeyRevealed", "Challenged", "Settled", "Refunded", "Revoked":
			if len(l.Topics) > 1 {
				e.SaleID = l.Topics[1]
			}
		case "Paid", "Credited", "Withdrawn":
			if len(l.Topics) > 1 {
				e.Account = common.BytesToAddress(l.Topics[1][:])
			}
			e.Amount, _ = values["amount"].(*big.Int)
		}
		switch spec.Name {
		case "Funded":
			e.Deadline, _ = values["revealDeadline"].(uint64)
			root, _ := values["cipherRoot"].([32]byte)
			commitment, _ := values["keyCommitment"].([32]byte)
			e.Commitments = Commitments{CipherRoot: root, KeyCommitment: commitment}
		case "KeyRevealed":
			key, _ := values["key"].([32]byte)
			e.Key = key
			e.Deadline, _ = values["challengeDeadline"].(uint64)
		case "Challenged":
			if index, ok := values["index"].(*big.Int); ok && index.IsUint64() {
				e.Index = index.Uint64()
			}
		}
		events = append(events, e)
	}
	return events, nil
}

// SaleEvents returns the escrow's events for one sale from block from onwards.
func (c Contract) SaleEvents(ctx context.Context, saleID common.Hash, from uint64) ([]Event, error) {
	logs, err := c.Backend.FilterLogs(ctx, ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(from),
		Addresses: []common.Address{c.Address},
		Topics:    [][]common.Hash{nil, {saleID}},
	})
	if err != nil {
		return nil, err
	}
	ptrs := make([]*types.Log, len(logs))
	for i := range logs {
		ptrs[i] = &logs[i]
	}
	return DecodeEvents(c.Address, ptrs)
}
