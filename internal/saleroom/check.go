package saleroom

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/Beam-Network/beam-cli-public/internal/datasale"
	"github.com/ethereum/go-ethereum/common"
)

// ErrPackageCheck reports a sale package the buyer must not fund.
var ErrPackageCheck = errors.New("the sale package failed the buyer's checks")

// Checked is a sale package that passed the buyer's checks, with the trees a challenge needs.
type Checked struct {
	Header     Header
	SaleID     common.Hash
	PlainTree  string
	CipherTree string
}

// CheckPackage runs the buyer's checks before funding: the offer is for this escrow and this buyer, the seller signed
// it with these commitments, the delivered ciphertext rebuilds R_c, and the hash list rebuilds R_p. Funding after
// these pass is the buyer's acknowledgement that it holds exactly what the commitments describe.
func CheckPackage(ctx context.Context, src Source, domain datasale.Domain, buyer common.Address, treeDir string) (Checked, error) {
	h, err := ReadHeader(src)
	if err != nil {
		return Checked{}, err
	}
	fail := func(format string, args ...any) (Checked, error) {
		return Checked{}, fmt.Errorf("%w: "+format, append([]any{ErrPackageCheck}, args...)...)
	}
	if h.ChainID.Cmp(domain.ChainID) != 0 || h.Escrow != domain.Escrow {
		return fail("it is for escrow %s on chain %s, not %s on chain %s", h.Escrow.Hex(), h.ChainID, domain.Escrow.Hex(), domain.ChainID)
	}
	if h.Offer.Buyer != buyer {
		return fail("the offer names buyer %s, not this wallet (%s)", h.Offer.Buyer.Hex(), buyer.Hex())
	}
	saleID, err := datasale.SaleID(domain, h.Offer)
	if err != nil {
		return fail("%v", err)
	}
	digest, err := datasale.SaleDigest(domain, h.Offer, h.CipherRoot, h.KeyCommitment)
	if err != nil {
		return fail("%v", err)
	}
	if signer, err := datasale.RecoverSigner(digest, h.Signature); err != nil || signer != h.Offer.Seller {
		return fail("the offer is not signed by its seller %s", h.Offer.Seller.Hex())
	}

	cipherTree := filepath.Join(treeDir, "cipher")
	cipherWriter, err := datasale.NewTreeWriter(cipherTree)
	if err != nil {
		return Checked{}, err
	}
	ciphertext, err := Ciphertext(src, h)
	if err != nil {
		return Checked{}, err
	}
	root, err := datasale.CipherTree(ctx, ciphertext, h.Leaves(), saleID, cipherWriter)
	ciphertext.Close()
	if err != nil {
		return Checked{}, fmt.Errorf("read the delivered ciphertext: %w", err)
	}
	if root != h.CipherRoot {
		return fail("the delivered ciphertext does not match the signed ciphertext root")
	}

	plainTree := filepath.Join(treeDir, "plain")
	plainWriter, err := datasale.NewTreeWriter(plainTree)
	if err != nil {
		return Checked{}, err
	}
	hashes, err := HashList(src, h)
	if err != nil {
		return Checked{}, err
	}
	plainRoot, err := datasale.PlainTreeFromHashes(ctx, hashes, h.Leaves(), plainWriter)
	hashes.Close()
	if err != nil {
		return Checked{}, fmt.Errorf("read the delivered hash list: %w", err)
	}
	if plainRoot != h.Offer.PlainRoot {
		return fail("the delivered hash list does not match the dataset fingerprint in the offer")
	}
	return Checked{Header: h, SaleID: saleID, PlainTree: plainTree, CipherTree: cipherTree}, nil
}
