package escrow

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// releasedRuntimeHex is the runtime code of DataSaleEscrow v1 as released, built with solc 0.8.26 and 200 optimizer
// runs for cancun. Its initcode is testdata/DataSaleEscrow.initcode.
//
//go:embed DataSaleEscrow.runtime
var releasedRuntimeHex string

var releasedRuntime = common.FromHex(strings.TrimSpace(releasedRuntimeHex))

// releasedImmutables are the byte ranges of the released runtime code that a deployment fills in: maxPrice and the two
// period floors the constructor sets, two references each, and the seven immutables of OpenZeppelin's EIP712, which
// include the deployment's address and chain id. Every deployment differs from the others only there.
var releasedImmutables = []struct{ start, length int }{
	{1287, 32}, {1362, 32}, {1435, 32}, {5441, 32}, {5490, 32}, {5643, 32}, {5697, 32},
	{5809, 32}, {6007, 32}, {6049, 32}, {6091, 32}, {6172, 32}, {6212, 32},
}

// ErrUnknownEscrow means the code at an escrow address is not the released build.
var ErrUnknownEscrow = errors.New("the code at the escrow address is not the DataSaleEscrow v1 build")

// IsReleasedBuild reports whether runtime is the released contract's code, whatever the deployment's immutables.
func IsReleasedBuild(runtime []byte) bool {
	if len(runtime) != len(releasedRuntime) {
		return false
	}
	masked := bytes.Clone(runtime)
	for _, r := range releasedImmutables {
		copy(masked[r.start:r.start+r.length], releasedRuntime[r.start:r.start+r.length])
	}
	return bytes.Equal(masked, releasedRuntime)
}

// VerifyRelease reads the code at address and returns ErrUnknownEscrow unless it is the released build.
func VerifyRelease(ctx context.Context, backend Backend, address common.Address) error {
	code, err := backend.CodeAt(ctx, address, nil)
	if err != nil {
		return err
	}
	if !IsReleasedBuild(code) {
		return fmt.Errorf("%w (%s, %d bytes of code)", ErrUnknownEscrow, address.Hex(), len(code))
	}
	return nil
}
