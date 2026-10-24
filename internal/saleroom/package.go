package saleroom

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"

	"github.com/Beam-Network/beam-cli-public/internal/datasale"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// A sale package is the single file the seller delivers to the buyer over the Room:
//
//	offset 0      "BEAMSALE", version 1, three zero bytes, header length (uint32, big-endian)
//	offset 16     header JSON, padded with spaces to HeaderRegion
//	HeaderRegion  hash list: h_0 … h_{n-1}, 32 bytes each
//	then          ciphertext: E_0 … E_{n-1}, LeafBytes each
//
// The header region has a fixed size so the seller writes the hash list and the ciphertext in place as it computes
// them, and the header last, once the commitments and the signature exist.
const (
	HeaderRegion   = 8192
	packageVersion = 1
	prefixBytes    = 16
)

var packageMagic = []byte("BEAMSALE")

// ErrPackage reports a malformed sale package.
var ErrPackage = errors.New("malformed sale package")

// Header is the sale package's header: everything the buyer checks before funding, apart from the data itself.
type Header struct {
	Version       int            `json:"version"`
	ChainID       *big.Int       `json:"chain_id"`
	Escrow        common.Address `json:"escrow"`
	Offer         datasale.Offer `json:"offer"`
	CipherRoot    common.Hash    `json:"cipher_root"`
	KeyCommitment common.Hash    `json:"key_commitment"`
	Signature     hexutil.Bytes  `json:"signature"`
	// Name is the dataset's file name, for the buyer's convenience only; nothing checks it.
	Name string `json:"name,omitempty"`
}

// Domain is the escrow the header's offer is for.
func (h Header) Domain() datasale.Domain {
	return datasale.Domain{ChainID: h.ChainID, Escrow: h.Escrow}
}

// Leaves is the number of leaves the offer covers.
func (h Header) Leaves() uint64 { return datasale.LeafCount(h.Offer.SizeBytes) }

// HashListOffset, CiphertextOffset and PackageSize locate a package's sections.
func HashListOffset() int64 { return HeaderRegion }

func CiphertextOffset(leaves uint64) int64 { return HeaderRegion + int64(leaves)*32 }

func PackageSize(leaves uint64) int64 {
	return CiphertextOffset(leaves) + int64(leaves)*datasale.LeafBytes
}

// EncodeHeader renders the prefix and the header JSON, padded to HeaderRegion.
func EncodeHeader(h Header) ([]byte, error) {
	h.Version = packageVersion
	body, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	if len(body) > HeaderRegion-prefixBytes {
		return nil, fmt.Errorf("the sale header is %d bytes; at most %d fit", len(body), HeaderRegion-prefixBytes)
	}
	region := bytes.Repeat([]byte{' '}, HeaderRegion)
	copy(region, packageMagic)
	region[8] = packageVersion
	binary.BigEndian.PutUint32(region[12:16], uint32(len(body)))
	copy(region[prefixBytes:], body)
	return region, nil
}

// DecodeHeader reads and checks a package's header from its first HeaderRegion bytes, given the package's size.
func DecodeHeader(region []byte, size int64) (Header, error) {
	if len(region) < HeaderRegion || !bytes.Equal(region[:8], packageMagic) {
		return Header{}, fmt.Errorf("%w: not a sale package", ErrPackage)
	}
	if region[8] != packageVersion {
		return Header{}, fmt.Errorf("%w: version %d is not supported", ErrPackage, region[8])
	}
	n := binary.BigEndian.Uint32(region[12:16])
	if n == 0 || int(n) > HeaderRegion-prefixBytes {
		return Header{}, fmt.Errorf("%w: header length %d", ErrPackage, n)
	}
	var h Header
	if err := json.Unmarshal(region[prefixBytes:prefixBytes+int(n)], &h); err != nil {
		return Header{}, fmt.Errorf("%w: %v", ErrPackage, err)
	}
	if h.ChainID == nil || h.Offer.Price == nil || h.Offer.Nonce == nil || h.Offer.SizeBytes == 0 {
		return Header{}, fmt.Errorf("%w: the header is incomplete", ErrPackage)
	}
	if want := PackageSize(h.Leaves()); size != want {
		return Header{}, fmt.Errorf("%w: it is %d bytes; a %d-byte dataset needs %d", ErrPackage, size, h.Offer.SizeBytes, want)
	}
	return h, nil
}

// Source is a sale package to read: small reads at an offset, and one stream per large section.
type Source interface {
	io.ReaderAt
	Section(offset, length int64) (io.ReadCloser, error)
	Size() int64
}

// ReadHeader reads and checks a package's header.
func ReadHeader(src Source) (Header, error) {
	if src.Size() < HeaderRegion {
		return Header{}, fmt.Errorf("%w: it is only %d bytes", ErrPackage, src.Size())
	}
	region := make([]byte, HeaderRegion)
	if _, err := src.ReadAt(region, 0); err != nil {
		return Header{}, err
	}
	return DecodeHeader(region, src.Size())
}

// HashList streams the package's hash list.
func HashList(src Source, h Header) (io.ReadCloser, error) {
	return src.Section(HashListOffset(), int64(h.Leaves())*32)
}

// Ciphertext streams the package's ciphertext.
func Ciphertext(src Source, h Header) (io.ReadCloser, error) {
	return src.Section(CiphertextOffset(h.Leaves()), int64(h.Leaves())*datasale.LeafBytes)
}

// CipherLeaf reads one ciphertext leaf, for a challenge.
func CipherLeaf(src Source, h Header, index uint64) ([]byte, error) {
	if index >= h.Leaves() {
		return nil, fmt.Errorf("leaf %d is out of range", index)
	}
	leaf := make([]byte, datasale.LeafBytes)
	_, err := src.ReadAt(leaf, CiphertextOffset(h.Leaves())+int64(index)*datasale.LeafBytes)
	return leaf, err
}

// PlainHash reads one entry of the hash list, for a challenge.
func PlainHash(src Source, index uint64) (common.Hash, error) {
	var h common.Hash
	_, err := src.ReadAt(h[:], HashListOffset()+int64(index)*32)
	return h, err
}
