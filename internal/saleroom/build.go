package saleroom

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Beam-Network/beam-cli-public/internal/datasale"
	"github.com/ethereum/go-ethereum/common"
)

// Built is what the seller keeps from building a sale package: the header it delivers, the sale's id and key, and
// the trees it needs if it is ever asked to prove a leaf.
type Built struct {
	Header     Header
	SaleID     common.Hash
	Key        common.Hash
	PlainTree  string
	CipherTree string
}

// BuildInput describes one sale package to build.
type BuildInput struct {
	PlainPath   string // the dataset
	PackagePath string // created; must not exist
	TreeDir     string // created; must not exist
	Domain      datasale.Domain
	Secret      common.Hash // the wallet secret the per-sale key derives from
	Name        string      // the dataset's name, shown to the buyer
	// Terms returns the offer once the dataset's fingerprint and size are known.
	Terms func(plainRoot common.Hash, sizeBytes uint64) (datasale.Offer, error)
	// Sign signs the sale digest with the seller's wallet.
	Sign func(digest common.Hash) ([]byte, error)
}

// Build writes a sale package in two passes over the dataset. The first computes the plaintext hashes, writing the
// hash list straight into the package, and fixes the offer; the second encrypts under the per-sale key, writing the
// ciphertext straight into the package. The header goes in last, with the commitments and the signature.
func Build(ctx context.Context, in BuildInput) (Built, error) {
	plain, err := os.Open(in.PlainPath)
	if err != nil {
		return Built{}, err
	}
	defer plain.Close()
	info, err := plain.Stat()
	if err != nil {
		return Built{}, err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return Built{}, errors.New("the dataset must be a regular, non-empty file")
	}
	size := uint64(info.Size())
	leaves := datasale.LeafCount(size)

	pkg, err := os.OpenFile(in.PackagePath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return Built{}, err
	}
	built, err := build(ctx, in, plain, pkg, size, leaves)
	closeErr := pkg.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(in.PackagePath)
		return Built{}, err
	}
	return built, nil
}

func build(ctx context.Context, in BuildInput, plain *os.File, pkg *os.File, size, leaves uint64) (Built, error) {
	if err := pkg.Truncate(PackageSize(leaves)); err != nil {
		return Built{}, err
	}
	plainTree, cipherTree := filepath.Join(in.TreeDir, "plain"), filepath.Join(in.TreeDir, "cipher")
	plainWriter, err := datasale.NewTreeWriter(plainTree)
	if err != nil {
		return Built{}, err
	}
	fingerprint, err := datasale.BuildPlain(ctx, plain, size, io.NewOffsetWriter(pkg, HashListOffset()), plainWriter)
	if err != nil {
		return Built{}, fmt.Errorf("hash the dataset: %w", err)
	}
	offer, err := in.Terms(fingerprint.Root, size)
	if err != nil {
		return Built{}, err
	}
	if offer.PlainRoot != fingerprint.Root || offer.SizeBytes != size {
		return Built{}, errors.New("the offer does not describe this dataset")
	}
	saleID, err := datasale.SaleID(in.Domain, offer)
	if err != nil {
		return Built{}, err
	}
	key := datasale.SellerKey(in.Secret, saleID)
	if _, err := plain.Seek(0, io.SeekStart); err != nil {
		return Built{}, err
	}
	cipherWriter, err := datasale.NewTreeWriter(cipherTree)
	if err != nil {
		return Built{}, err
	}
	cipherRoot, err := datasale.Encrypt(ctx, plain, size, saleID, key, io.NewOffsetWriter(pkg, CiphertextOffset(leaves)), cipherWriter)
	if err != nil {
		return Built{}, fmt.Errorf("encrypt the dataset: %w", err)
	}
	keyCommitment := datasale.KeyCommitment(saleID, key)
	digest, err := datasale.SaleDigest(in.Domain, offer, cipherRoot, keyCommitment)
	if err != nil {
		return Built{}, err
	}
	signature, err := in.Sign(digest)
	if err != nil {
		return Built{}, err
	}
	header := Header{
		ChainID: in.Domain.ChainID, Escrow: in.Domain.Escrow, Offer: offer,
		CipherRoot: cipherRoot, KeyCommitment: keyCommitment, Signature: signature, Name: in.Name,
	}
	region, err := EncodeHeader(header)
	if err != nil {
		return Built{}, err
	}
	if _, err := pkg.WriteAt(region, 0); err != nil {
		return Built{}, err
	}
	if err := pkg.Sync(); err != nil {
		return Built{}, err
	}
	header.Version = packageVersion
	return Built{Header: header, SaleID: saleID, Key: key, PlainTree: plainTree, CipherTree: cipherTree}, nil
}
