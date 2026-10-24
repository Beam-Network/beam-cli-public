package datasale

import (
	"crypto/ecdsa"
	"errors"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// ErrInvalidSignature reports a signature the escrow would refuse: not 65 bytes, a v other than 27 or 28, an s in the
// upper half of the curve order, or one that recovers no key.
var ErrInvalidSignature = errors.New("invalid sale signature")

var halfOrder = new(big.Int).Rsh(crypto.S256().Params().N, 1)

// SignDigest signs an EIP-712 digest and returns r ‖ s ‖ v with v in {27, 28}, the form the escrow verifies.
func SignDigest(key *ecdsa.PrivateKey, digest common.Hash) ([]byte, error) {
	sig, err := crypto.Sign(digest[:], key)
	if err != nil {
		return nil, err
	}
	sig[64] += 27
	return sig, nil
}

// RecoverSigner returns the address that signed digest, applying the same checks as the escrow's ECDSA library.
func RecoverSigner(digest common.Hash, sig []byte) (common.Address, error) {
	if len(sig) != 65 || (sig[64] != 27 && sig[64] != 28) {
		return common.Address{}, ErrInvalidSignature
	}
	if new(big.Int).SetBytes(sig[32:64]).Cmp(halfOrder) > 0 {
		return common.Address{}, ErrInvalidSignature
	}
	normalized := make([]byte, 65)
	copy(normalized, sig)
	normalized[64] -= 27
	pub, err := crypto.SigToPub(digest[:], normalized)
	if err != nil {
		return common.Address{}, ErrInvalidSignature
	}
	return crypto.PubkeyToAddress(*pub), nil
}
