package escrow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math/big"
	"os/exec"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// The embedded runtime is the released build byte for byte: 8,821 bytes with this SHA-256.
func TestTheEmbeddedRuntimeIsTheReleasedBuild(t *testing.T) {
	sum := sha256.Sum256(releasedRuntime)
	if len(releasedRuntime) != 8821 || hex.EncodeToString(sum[:]) != "853a271a780d80d0f297a4cf3b2485e55fa50e3dae0620c9782218b593231615" {
		t.Fatalf("the embedded runtime is %d bytes with SHA-256 %x", len(releasedRuntime), sum)
	}
	end := 0
	for _, r := range releasedImmutables {
		if r.start < end || r.length != 32 || r.start+r.length > len(releasedRuntime) {
			t.Fatalf("immutable range %+v overlaps the one before or leaves the code", r)
		}
		end = r.start + r.length
	}
}

func TestOnlyTheImmutablesOfTheReleasedBuildMayDiffer(t *testing.T) {
	if !IsReleasedBuild(releasedRuntime) {
		t.Fatal("the released runtime itself was refused")
	}
	deployment := bytes.Clone(releasedRuntime)
	for _, r := range releasedImmutables {
		for i := r.start; i < r.start+r.length; i++ {
			deployment[i] ^= 0xa5
		}
	}
	if !IsReleasedBuild(deployment) {
		t.Fatal("a deployment that differs only in its immutables was refused")
	}
	outside := map[string]int{"the first byte": 0, "a byte between two immutables": releasedImmutables[0].start + 32,
		"the byte just before an immutable": releasedImmutables[3].start - 1, "the metadata hash at the end": len(releasedRuntime) - 3}
	for name, index := range outside {
		changed := bytes.Clone(deployment)
		changed[index] ^= 0x01
		if IsReleasedBuild(changed) {
			t.Errorf("a change to %s (byte %d) was accepted", name, index)
		}
	}
	for name, code := range map[string][]byte{
		"no code":       nil,
		"one byte less": releasedRuntime[:len(releasedRuntime)-1],
		"one byte more": append(bytes.Clone(releasedRuntime), 0x00),
	} {
		if IsReleasedBuild(code) {
			t.Errorf("%s was accepted", name)
		}
	}
}

// Deployments of the released initcode pass whatever their constructor arguments; an account without code and a
// deployment of the released runtime with one instruction byte changed are refused. Skipped without anvil.
func TestDeployedEscrowsAreCheckedAgainstTheReleasedBuild(t *testing.T) {
	anvil, err := exec.LookPath("anvil")
	if err != nil {
		t.Skip("anvil is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	raw, client := startAnvil(t, ctx, anvil)
	chainID, err := client.ChainID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	deployer := testAccount(t, "deployer")
	if err := raw.CallContext(ctx, nil, "anvil_setBalance", deployer.Address(), hexutil.EncodeBig(big.NewInt(1e18))); err != nil {
		t.Fatal(err)
	}
	backend := NewFailover(5*time.Second, client)
	for _, escrow := range []common.Address{
		deployEscrowWith(t, ctx, client, chainID, deployer, big.NewInt(1e18), 5, 5),
		deployEscrowWith(t, ctx, client, chainID, deployer, big.NewInt(3e18), 300, 7200),
	} {
		if err := VerifyRelease(ctx, backend, escrow); err != nil {
			t.Fatalf("a deployment of the released initcode at %s was refused: %v", escrow, err)
		}
	}

	// The released runtime with one byte outside the immutables changed, returned as is by a minimal constructor that
	// copies the code after it into memory.
	modified := bytes.Clone(releasedRuntime)
	modified[0] ^= 0x01
	constructor := []byte{0x61, 0, 0, 0x61, 0, 15, 0x60, 0x00, 0x39, 0x61, 0, 0, 0x60, 0x00, 0xf3}
	binary.BigEndian.PutUint16(constructor[1:], uint16(len(modified)))
	binary.BigEndian.PutUint16(constructor[10:], uint16(len(modified)))
	forged := deployCode(t, ctx, client, chainID, deployer, append(constructor, modified...))
	if code, err := client.CodeAt(ctx, forged, nil); err != nil || !bytes.Equal(code, modified) {
		t.Fatalf("the forged contract's code was not deployed as written: %v", err)
	}
	for name, address := range map[string]common.Address{"a modified build": forged, "an account without code": deployer.Address()} {
		if err := VerifyRelease(ctx, backend, address); !errors.Is(err, ErrUnknownEscrow) {
			t.Errorf("%s: err = %v; want ErrUnknownEscrow", name, err)
		}
	}
}
