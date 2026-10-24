package datasale

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

// vectors is testdata/vectors-v1.json: one complete sale computed by the protocol's reference implementation, which
// the escrow contract's own tests also check.
type vectors struct {
	ChainID          uint64            `json:"chainId"`
	Escrow           common.Address    `json:"escrow"`
	SellerPrivateKey string            `json:"sellerPrivateKey"`
	Offer            map[string]string `json:"offer"`
	PlainHashes      []common.Hash     `json:"plainHashes"`
	PlainLeafHashes  []common.Hash     `json:"plainLeafHashes"`
	PlainRoot        common.Hash       `json:"plainRoot"`
	SaleID           common.Hash       `json:"saleId"`
	SellerSecret     common.Hash       `json:"sellerSecret"`
	Key              common.Hash       `json:"key"`
	KeyCommitment    common.Hash       `json:"keyCommitment"`
	CipherLeafHashes []common.Hash     `json:"cipherLeafHashes"`
	CipherRoot       common.Hash       `json:"cipherRoot"`
	SaleDigest       common.Hash       `json:"saleDigest"`
	Signature        hexutil.Bytes     `json:"signature"`
	Leaves           []struct {
		Index         uint64        `json:"index"`
		Plain         hexutil.Bytes `json:"plain"`
		Cipher        hexutil.Bytes `json:"cipher"`
		DecryptedHash common.Hash   `json:"decryptedHash"`
		CipherProof   []common.Hash `json:"cipherProof"`
		PlainProof    []common.Hash `json:"plainProof"`
	} `json:"leaves"`
}

func loadVectors(t *testing.T) (vectors, Domain, Offer) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "vectors-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	u64 := func(k string) uint64 {
		n, ok := new(big.Int).SetString(v.Offer[k], 10)
		if !ok || !n.IsUint64() {
			t.Fatalf("offer.%s = %q", k, v.Offer[k])
		}
		return n.Uint64()
	}
	big10 := func(k string) *big.Int {
		n, ok := new(big.Int).SetString(v.Offer[k], 10)
		if !ok {
			t.Fatalf("offer.%s = %q", k, v.Offer[k])
		}
		return n
	}
	offer := Offer{
		Seller:          common.HexToAddress(v.Offer["seller"]),
		Buyer:           common.HexToAddress(v.Offer["buyer"]),
		PlainRoot:       common.HexToHash(v.Offer["plainRoot"]),
		SizeBytes:       u64("sizeBytes"),
		Price:           big10("price"),
		FundDeadline:    u64("fundDeadline"),
		RevealPeriod:    u64("revealPeriod"),
		ChallengePeriod: u64("challengePeriod"),
		Nonce:           big10("nonce"),
	}
	return v, Domain{ChainID: new(big.Int).SetUint64(v.ChainID), Escrow: v.Escrow}, offer
}

func TestVectorsOfferSignatureAndKeys(t *testing.T) {
	v, domain, offer := loadVectors(t)
	saleID, err := SaleID(domain, offer)
	if err != nil || saleID != v.SaleID {
		t.Fatalf("SaleID = %s, %v; want %s", saleID, err, v.SaleID)
	}
	digest, err := SaleDigest(domain, offer, v.CipherRoot, v.KeyCommitment)
	if err != nil || digest != v.SaleDigest {
		t.Fatalf("SaleDigest = %s, %v; want %s", digest, err, v.SaleDigest)
	}
	if got := SellerKey(v.SellerSecret, saleID); got != v.Key {
		t.Fatalf("SellerKey = %s; want %s", got, v.Key)
	}
	if got := KeyCommitment(saleID, v.Key); got != v.KeyCommitment {
		t.Fatalf("KeyCommitment = %s; want %s", got, v.KeyCommitment)
	}
	signer, err := RecoverSigner(digest, v.Signature)
	if err != nil || signer != offer.Seller {
		t.Fatalf("RecoverSigner = %s, %v; want %s", signer, err, offer.Seller)
	}
	key, err := crypto.HexToECDSA(v.SellerPrivateKey[2:])
	if err != nil {
		t.Fatal(err)
	}
	sig, err := SignDigest(key, digest)
	if err != nil || !bytes.Equal(sig, v.Signature) {
		t.Fatalf("SignDigest = %x, %v; want the vector's deterministic signature %x", sig, err, []byte(v.Signature))
	}
}

func TestVectorsLeavesRootsAndProofs(t *testing.T) {
	v, _, offer := loadVectors(t)
	if got := LeafCount(offer.SizeBytes); got != uint64(len(v.Leaves)) {
		t.Fatalf("LeafCount = %d; want %d", got, len(v.Leaves))
	}
	plainTree := NewTestTree(t, v.PlainLeafHashes)
	cipherTree := NewTestTree(t, v.CipherLeafHashes)
	for _, leaf := range v.Leaves {
		i := leaf.Index
		if got := crypto.Keccak256Hash(leaf.Plain); got != v.PlainHashes[i] {
			t.Fatalf("leaf %d plain hash = %s; want %s", i, got, v.PlainHashes[i])
		}
		if got := PlainLeafHash(i, v.PlainHashes[i]); got != v.PlainLeafHashes[i] {
			t.Fatalf("PlainLeafHash(%d) = %s; want %s", i, got, v.PlainLeafHashes[i])
		}
		if got := CipherLeafHash(v.SaleID, i, leaf.Cipher); got != v.CipherLeafHashes[i] {
			t.Fatalf("CipherLeafHash(%d) = %s; want %s", i, got, v.CipherLeafHashes[i])
		}
		decrypted := make([]byte, len(leaf.Cipher))
		XORKeystream(v.Key, i, decrypted, leaf.Cipher)
		if !bytes.Equal(decrypted, leaf.Plain) || crypto.Keccak256Hash(decrypted) != leaf.DecryptedHash {
			t.Fatalf("leaf %d does not decrypt to its plaintext", i)
		}
		for _, check := range []struct {
			name string
			tree *Tree
			want []common.Hash
			leaf common.Hash
			root common.Hash
		}{
			{"cipher", cipherTree, leaf.CipherProof, v.CipherLeafHashes[i], v.CipherRoot},
			{"plain", plainTree, leaf.PlainProof, v.PlainLeafHashes[i], v.PlainRoot},
		} {
			proof, err := check.tree.Proof(i)
			if err != nil || fmt.Sprint(proof) != fmt.Sprint(check.want) {
				t.Fatalf("%s proof %d = %v, %v; want %v", check.name, i, proof, err, check.want)
			}
			if got := ProcessProof(proof, check.leaf); got != check.root {
				t.Fatalf("%s proof %d hashes to %s; want %s", check.name, i, got, check.root)
			}
		}
	}
	for _, check := range []struct {
		tree *Tree
		want common.Hash
	}{{plainTree, v.PlainRoot}, {cipherTree, v.CipherRoot}} {
		if root, err := check.tree.Root(); err != nil || root != check.want {
			t.Fatalf("root = %s, %v; want %s", root, err, check.want)
		}
	}
}

// TestVectorsSaleRebuiltFromScratch runs the seller's and the buyer's whole pipeline on the vectors' dataset.
func TestVectorsSaleRebuiltFromScratch(t *testing.T) {
	v, _, offer := loadVectors(t)
	data := vectorDataset(offer.SizeBytes)
	ctx := context.Background()
	dir := t.TempDir()

	var hashList bytes.Buffer
	plain, err := BuildPlain(ctx, bytes.NewReader(data), offer.SizeBytes, &hashList, mustTreeWriter(t, filepath.Join(dir, "plain")))
	if err != nil || plain.Root != v.PlainRoot || plain.Leaves != uint64(len(v.Leaves)) {
		t.Fatalf("BuildPlain = %+v, %v; want root %s", plain, err, v.PlainRoot)
	}
	var ciphertext bytes.Buffer
	cipherRoot, err := Encrypt(ctx, bytes.NewReader(data), offer.SizeBytes, v.SaleID, v.Key, &ciphertext, mustTreeWriter(t, filepath.Join(dir, "cipher")))
	if err != nil || cipherRoot != v.CipherRoot {
		t.Fatalf("Encrypt root = %s, %v; want %s", cipherRoot, err, v.CipherRoot)
	}
	if ciphertext.Len() != len(v.Leaves)*LeafBytes {
		t.Fatalf("ciphertext is %d bytes; want %d", ciphertext.Len(), len(v.Leaves)*LeafBytes)
	}

	// The buyer's checks before funding.
	received, err := CipherTree(ctx, bytes.NewReader(ciphertext.Bytes()), plain.Leaves, v.SaleID, mustTreeWriter(t, filepath.Join(dir, "received")))
	if err != nil || received != v.CipherRoot {
		t.Fatalf("CipherTree = %s, %v; want %s", received, err, v.CipherRoot)
	}
	rebuilt, err := PlainTreeFromHashes(ctx, bytes.NewReader(hashList.Bytes()), plain.Leaves, mustTreeWriter(t, filepath.Join(dir, "list")))
	if err != nil || rebuilt != v.PlainRoot {
		t.Fatalf("PlainTreeFromHashes = %s, %v; want %s", rebuilt, err, v.PlainRoot)
	}

	// After the reveal.
	var out bytes.Buffer
	result, err := Decrypt(ctx, bytes.NewReader(ciphertext.Bytes()), offer.SizeBytes, v.Key, bytes.NewReader(hashList.Bytes()), &out)
	if err != nil || result.MismatchCount != 0 || !bytes.Equal(out.Bytes(), data) {
		t.Fatalf("Decrypt = %s, %v; plaintext equal = %v", result, err, bytes.Equal(out.Bytes(), data))
	}
}

// vectorDataset rebuilds the reference generator's data: word at offset o is keccak256("vector:<o>").
func vectorDataset(size uint64) []byte {
	data := make([]byte, size)
	for offset := uint64(0); offset < size; offset += 32 {
		word := crypto.Keccak256([]byte(fmt.Sprintf("vector:%d", offset)))
		copy(data[offset:], word)
	}
	return data
}

func mustTreeWriter(t *testing.T, dir string) *TreeWriter {
	t.Helper()
	w, err := NewTreeWriter(dir)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// NewTestTree builds a finished tree over the given leaves.
func NewTestTree(t *testing.T, leaves []common.Hash) *Tree {
	t.Helper()
	w := mustTreeWriter(t, t.TempDir())
	for _, leaf := range leaves {
		if err := w.Add(leaf); err != nil {
			t.Fatal(err)
		}
	}
	tree, err := w.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return tree
}
