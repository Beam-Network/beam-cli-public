// Package datasale implements the data-sale protocol that the escrow contract checks: the sale material a seller
// builds and a buyer verifies, the EIP-712 offer the seller signs, the per-sale key, and challenge proofs. Every
// function here must agree byte for byte with the contract; the shared test vectors in testdata pin that.
package datasale

import (
	"encoding/binary"
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// LeafBytes is the size of every leaf; the last one is zero-padded.
const LeafBytes = 4096

var (
	TagPlain     = crypto.Keccak256Hash([]byte("beam.data-sale.plain-leaf.v1"))
	TagCipher    = crypto.Keccak256Hash([]byte("beam.data-sale.cipher-leaf.v1"))
	TagStream    = crypto.Keccak256Hash([]byte("beam.data-sale.keystream.v1"))
	TagKey       = crypto.Keccak256Hash([]byte("beam.data-sale.key.v1"))
	TagSellerKey = crypto.Keccak256Hash([]byte("beam.data-sale.seller-key.v1"))

	domainTypeHash = crypto.Keccak256Hash([]byte("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"))
	offerTypeHash  = crypto.Keccak256Hash([]byte(offerType))
	saleTypeHash   = crypto.Keccak256Hash([]byte("Sale(Offer offer,bytes32 cipherRoot,bytes32 keyCommitment)" + offerType))
	domainName     = crypto.Keccak256Hash([]byte("Beam Data Sale"))
	domainVersion  = crypto.Keccak256Hash([]byte("1"))
)

const offerType = "Offer(address seller,address buyer,bytes32 plainRoot,uint64 sizeBytes,uint256 price,uint64 fundDeadline,uint64 revealPeriod,uint64 challengePeriod,uint256 nonce)"

// Offer is the set of terms the seller signs. Deadlines and periods count blocks.
type Offer struct {
	Seller          common.Address `json:"seller"`
	Buyer           common.Address `json:"buyer"`
	PlainRoot       common.Hash    `json:"plainRoot"`
	SizeBytes       uint64         `json:"sizeBytes"`
	Price           *big.Int       `json:"price"`
	FundDeadline    uint64         `json:"fundDeadline"`
	RevealPeriod    uint64         `json:"revealPeriod"`
	ChallengePeriod uint64         `json:"challengePeriod"`
	Nonce           *big.Int       `json:"nonce"`
}

// Domain identifies one escrow deployment: the EIP-712 domain binds every offer to a chain and a contract.
type Domain struct {
	ChainID *big.Int
	Escrow  common.Address
}

// ErrOutOfRange reports an integer that does not fit the uint256 word the contract reads.
var ErrOutOfRange = errors.New("value does not fit in a uint256")

// Separator is the EIP-712 domain separator.
func (d Domain) Separator() (common.Hash, error) {
	chainID, err := uintWord(d.ChainID)
	if err != nil {
		return common.Hash{}, err
	}
	return crypto.Keccak256Hash(domainTypeHash[:], domainName[:], domainVersion[:], chainID, addressWord(d.Escrow)), nil
}

// structHash is the EIP-712 hash of the offer's terms alone.
func (o Offer) structHash() (common.Hash, error) {
	price, err := uintWord(o.Price)
	if err != nil {
		return common.Hash{}, err
	}
	nonce, err := uintWord(o.Nonce)
	if err != nil {
		return common.Hash{}, err
	}
	return crypto.Keccak256Hash(offerTypeHash[:], addressWord(o.Seller), addressWord(o.Buyer), o.PlainRoot[:],
		u64Word(o.SizeBytes), price, u64Word(o.FundDeadline), u64Word(o.RevealPeriod), u64Word(o.ChallengePeriod), nonce), nil
}

// SaleID is the EIP-712 hash of the offer's terms. It does not cover the commitments, because every ciphertext leaf
// and the key commitment are computed from it.
func SaleID(d Domain, o Offer) (common.Hash, error) {
	offerHash, err := o.structHash()
	if err != nil {
		return common.Hash{}, err
	}
	return typedDataHash(d, offerHash)
}

// SaleDigest is what the seller signs: the offer's terms and both commitments.
func SaleDigest(d Domain, o Offer, cipherRoot, keyCommitment common.Hash) (common.Hash, error) {
	offerHash, err := o.structHash()
	if err != nil {
		return common.Hash{}, err
	}
	return typedDataHash(d, crypto.Keccak256Hash(saleTypeHash[:], offerHash[:], cipherRoot[:], keyCommitment[:]))
}

func typedDataHash(d Domain, structHash common.Hash) (common.Hash, error) {
	separator, err := d.Separator()
	if err != nil {
		return common.Hash{}, err
	}
	return crypto.Keccak256Hash([]byte{0x19, 0x01}, separator[:], structHash[:]), nil
}

// KeyCommitment is K = H(enc(TAG_KEY, saleId, k)).
func KeyCommitment(saleID, key common.Hash) common.Hash {
	return crypto.Keccak256Hash(TagKey[:], saleID[:], key[:])
}

// SellerKey derives the per-sale key k = H(enc(TAG_SELLER_KEY, s, saleId)) from the seller's 32-byte secret. Sale ids
// never repeat, so neither do keys, and a seller can derive the key again after a crash.
func SellerKey(secret, saleID common.Hash) common.Hash {
	return crypto.Keccak256Hash(TagSellerKey[:], secret[:], saleID[:])
}

// PlainLeafHash is leafP_i = H(H(enc(TAG_PLAIN, i, h_i))).
func PlainLeafHash(index uint64, plainHash common.Hash) common.Hash {
	inner := crypto.Keccak256(TagPlain[:], u64Word(index), plainHash[:])
	return crypto.Keccak256Hash(inner)
}

// CipherLeafHash is leafC_i = H(H(enc(TAG_CIPHER, saleId, i, E_i))), with the leaf ABI-encoded as dynamic bytes.
func CipherLeafHash(saleID common.Hash, index uint64, leaf []byte) common.Hash {
	inner := crypto.Keccak256(TagCipher[:], saleID[:], u64Word(index), u64Word(4*32), u64Word(uint64(len(leaf))), padWords(leaf))
	return crypto.Keccak256Hash(inner)
}

// XORKeystream sets dst = src ⊕ KS(k, i), where word j of the keystream is H(enc(TAG_STREAM, k, i, j)). It encrypts
// and decrypts; dst and src may be the same slice. src must be a whole number of 32-byte words.
func XORKeystream(key common.Hash, index uint64, dst, src []byte) {
	state := crypto.NewKeccakState()
	var pre [128]byte
	copy(pre[0:32], TagStream[:])
	copy(pre[32:64], key[:])
	binary.BigEndian.PutUint64(pre[88:96], index)
	var word [32]byte
	for j := 0; j*32 < len(src); j++ {
		binary.BigEndian.PutUint64(pre[120:128], uint64(j))
		state.Reset()
		state.Write(pre[:])
		state.Read(word[:])
		for b := 0; b < 32; b++ {
			dst[j*32+b] = src[j*32+b] ^ word[b]
		}
	}
}

// LeafCount is n = ceil(size / LeafBytes).
func LeafCount(size uint64) uint64 {
	return (size + LeafBytes - 1) / LeafBytes
}

func u64Word(v uint64) []byte {
	w := make([]byte, 32)
	binary.BigEndian.PutUint64(w[24:], v)
	return w
}

func addressWord(a common.Address) []byte {
	w := make([]byte, 32)
	copy(w[12:], a[:])
	return w
}

func uintWord(v *big.Int) ([]byte, error) {
	if v == nil || v.Sign() < 0 || v.BitLen() > 256 {
		return nil, ErrOutOfRange
	}
	w := make([]byte, 32)
	v.FillBytes(w)
	return w, nil
}

func padWords(b []byte) []byte {
	if len(b)%32 == 0 {
		return b
	}
	out := make([]byte, (len(b)+31)/32*32)
	copy(out, b)
	return out
}
