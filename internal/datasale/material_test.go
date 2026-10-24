package datasale

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// referenceRoot is the tree shape written the obvious way, in memory, to check the disk-backed tree against.
func referenceRoot(level []common.Hash) common.Hash {
	for len(level) > 1 {
		var next []common.Hash
		for i := 0; i < len(level); i += 2 {
			if i+1 < len(level) {
				next = append(next, hashPair(level[i], level[i+1]))
			} else {
				next = append(next, level[i])
			}
		}
		level = next
	}
	return level[0]
}

func TestTreeShapeMatchesTheReferenceForEveryLeafCount(t *testing.T) {
	for n := 1; n <= 37; n++ {
		leaves := make([]common.Hash, n)
		for i := range leaves {
			leaves[i] = crypto.Keccak256Hash([]byte(fmt.Sprintf("leaf %d of %d", i, n)))
		}
		tree := NewTestTree(t, leaves)
		root, err := tree.Root()
		if err != nil || root != referenceRoot(leaves) {
			t.Fatalf("n=%d: root %s, %v; want %s", n, root, err, referenceRoot(leaves))
		}
		for i := range leaves {
			proof, err := tree.Proof(uint64(i))
			if err != nil || ProcessProof(proof, leaves[i]) != root {
				t.Fatalf("n=%d: proof for leaf %d does not reach the root (%v)", n, i, err)
			}
		}
		if _, err := tree.Proof(uint64(n)); err == nil {
			t.Fatalf("n=%d: a proof for an index past the end was produced", n)
		}
	}
}

func TestOpenTreeReadsAFinishedTree(t *testing.T) {
	dir := t.TempDir()
	w := mustTreeWriter(t, dir)
	for i := 0; i < 5; i++ {
		_ = w.Add(crypto.Keccak256Hash([]byte{byte(i)}))
	}
	built, err := w.Finish()
	if err != nil {
		t.Fatal(err)
	}
	opened, err := OpenTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := built.Root()
	b, _ := opened.Root()
	if a != b || opened.Leaves() != 5 {
		t.Fatalf("reopened tree differs: %s vs %s, %d leaves", a, b, opened.Leaves())
	}
	if _, err := NewTreeWriter(dir); err == nil {
		t.Fatal("a second tree was started over a finished one")
	}
}

func TestDecryptReportsEveryCorruptedLeaf(t *testing.T) {
	ctx := context.Background()
	size := uint64(10*LeafBytes + 7)
	data := randomBytes(t, int(size))
	saleID := crypto.Keccak256Hash([]byte("sale"))
	key := crypto.Keccak256Hash([]byte("key"))
	var hashList, ciphertext bytes.Buffer
	dir := t.TempDir()
	if _, err := BuildPlain(ctx, bytes.NewReader(data), size, &hashList, mustTreeWriter(t, filepath.Join(dir, "p"))); err != nil {
		t.Fatal(err)
	}
	if _, err := Encrypt(ctx, bytes.NewReader(data), size, saleID, key, &ciphertext, mustTreeWriter(t, filepath.Join(dir, "c"))); err != nil {
		t.Fatal(err)
	}
	corrupted := ciphertext.Bytes()
	corrupted[3*LeafBytes+100] ^= 1
	corrupted[10*LeafBytes] ^= 1 // inside the zero-padded last leaf's data
	var out bytes.Buffer
	result, err := Decrypt(ctx, bytes.NewReader(corrupted), size, key, bytes.NewReader(hashList.Bytes()), &out)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(result.Mismatches) != "[3 10]" || result.MismatchCount != 2 || uint64(out.Len()) != size {
		t.Fatalf("Decrypt = %s %v, %d bytes out", result, result.Mismatches, out.Len())
	}

	// A wrong key makes every leaf mismatch.
	out.Reset()
	result, err = Decrypt(ctx, bytes.NewReader(ciphertext.Bytes()), size, crypto.Keccak256Hash([]byte("other")), bytes.NewReader(hashList.Bytes()), &out)
	if err != nil || result.MismatchCount != LeafCount(size) {
		t.Fatalf("wrong key: %s, %v", result, err)
	}
}

func TestInputsThatDoNotMatchTheSaleSizeAreRefused(t *testing.T) {
	ctx := context.Background()
	size := uint64(3 * LeafBytes)
	data := randomBytes(t, int(size))
	saleID := crypto.Keccak256Hash([]byte("sale"))
	for name, run := range map[string]func(dir string) error{
		"plaintext shorter than its size": func(dir string) error {
			_, err := BuildPlain(ctx, bytes.NewReader(data[:size-1]), size, &bytes.Buffer{}, mustTreeWriter(t, dir))
			return err
		},
		"plaintext longer than its size": func(dir string) error {
			_, err := BuildPlain(ctx, bytes.NewReader(append(data, 0)), size, &bytes.Buffer{}, mustTreeWriter(t, dir))
			return err
		},
		"ciphertext with an extra byte": func(dir string) error {
			_, err := CipherTree(ctx, bytes.NewReader(append(data, 0)), 3, saleID, mustTreeWriter(t, dir))
			return err
		},
		"ciphertext missing a leaf": func(dir string) error {
			_, err := CipherTree(ctx, bytes.NewReader(data[:2*LeafBytes]), 3, saleID, mustTreeWriter(t, dir))
			return err
		},
		"hash list with an extra entry": func(dir string) error {
			_, err := PlainTreeFromHashes(ctx, bytes.NewReader(make([]byte, 4*32)), 3, mustTreeWriter(t, dir))
			return err
		},
		"empty plaintext": func(dir string) error {
			_, err := BuildPlain(ctx, bytes.NewReader(nil), 0, &bytes.Buffer{}, mustTreeWriter(t, dir))
			return err
		},
	} {
		if err := run(t.TempDir()); !errors.Is(err, ErrSizeMismatch) {
			t.Errorf("%s: err = %v; want ErrSizeMismatch", name, err)
		}
	}
}

func TestRecoverSignerAppliesTheEscrowsChecks(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	digest := crypto.Keccak256Hash([]byte("digest"))
	sig, err := SignDigest(key, digest)
	if err != nil {
		t.Fatal(err)
	}
	if signer, err := RecoverSigner(digest, sig); err != nil || signer != crypto.PubkeyToAddress(key.PublicKey) {
		t.Fatalf("RecoverSigner = %s, %v", signer, err)
	}
	n := crypto.S256().Params().N
	highS := append([]byte{}, sig...)
	new(big.Int).Sub(n, new(big.Int).SetBytes(sig[32:64])).FillBytes(highS[32:64])
	highS[64] ^= 1 // 27 <-> 28, the malleable twin
	badV := append([]byte{}, sig...)
	badV[64] = 29
	for name, bad := range map[string][]byte{"64 bytes": sig[:64], "high s": highS, "v = 29": badV, "zeros": make([]byte, 65)} {
		if _, err := RecoverSigner(digest, bad); !errors.Is(err, ErrInvalidSignature) {
			t.Errorf("%s: err = %v; want ErrInvalidSignature", name, err)
		}
	}
}

func TestOfferWordsRejectValuesTheContractCannotRead(t *testing.T) {
	domain := Domain{ChainID: big.NewInt(945), Escrow: common.HexToAddress("0x01")}
	for name, offer := range map[string]Offer{
		"negative price": {Price: big.NewInt(-1), Nonce: big.NewInt(1)},
		"nil nonce":      {Price: big.NewInt(1)},
		"257-bit nonce":  {Price: big.NewInt(1), Nonce: new(big.Int).Lsh(big.NewInt(1), 256)},
	} {
		if _, err := SaleID(domain, offer); !errors.Is(err, ErrOutOfRange) {
			t.Errorf("%s: err = %v; want ErrOutOfRange", name, err)
		}
	}
}

func randomBytes(t testing.TB, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// BenchmarkDecryptAndCheck measures the buyer's decrypt-and-check throughput, which sizes the challenge window.
func BenchmarkDecryptAndCheck(b *testing.B) {
	size := uint64(64 << 20)
	data := randomBytes(b, int(size))
	ctx := context.Background()
	key := crypto.Keccak256Hash([]byte("key"))
	var hashList, ciphertext bytes.Buffer
	dir := b.TempDir()
	if _, err := BuildPlain(ctx, bytes.NewReader(data), size, &hashList, mustTreeWriterB(b, filepath.Join(dir, "p"))); err != nil {
		b.Fatal(err)
	}
	if _, err := Encrypt(ctx, bytes.NewReader(data), size, key, key, &ciphertext, mustTreeWriterB(b, filepath.Join(dir, "c"))); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(size))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Decrypt(ctx, bytes.NewReader(ciphertext.Bytes()), size, key, bytes.NewReader(hashList.Bytes()), discard{}); err != nil {
			b.Fatal(err)
		}
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func mustTreeWriterB(b *testing.B, dir string) *TreeWriter {
	w, err := NewTreeWriter(dir)
	if err != nil {
		b.Fatal(err)
	}
	return w
}
