package saleroom

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/datasale"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

var testDomain = datasale.Domain{ChainID: big.NewInt(945), Escrow: common.HexToAddress("0x00000000000000000000000000000000000E5C40")}

func testKey(t *testing.T, seed string) *ecdsaKey {
	t.Helper()
	key, err := crypto.ToECDSA(crypto.Keccak256([]byte(seed)))
	if err != nil {
		t.Fatal(err)
	}
	return &ecdsaKey{key}
}

func TestABindingVerifiesOnlyForItsRoomEscrowAndBuyer(t *testing.T) {
	buyer := testKey(t, "buyer")
	b, err := SignBinding(buyer.PrivateKey, testDomain, "room-a", "member-b", time.Unix(1_800_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Verify(testDomain, "room-a"); err != nil {
		t.Fatalf("Verify = %v", err)
	}
	encoded, _ := json.Marshal(b)
	parsed, err := ParseBinding(encoded)
	if err != nil || parsed.Verify(testDomain, "room-a") != nil {
		t.Fatalf("a round-tripped binding does not verify: %v", err)
	}
	otherEscrow := datasale.Domain{ChainID: testDomain.ChainID, Escrow: common.HexToAddress("0x01")}
	otherChain := datasale.Domain{ChainID: big.NewInt(964), Escrow: testDomain.Escrow}
	for name, check := range map[string]func() error{
		"another room":   func() error { return b.Verify(testDomain, "room-z") },
		"another escrow": func() error { return b.Verify(otherEscrow, "room-a") },
		"another chain":  func() error { return b.Verify(otherChain, "room-a") },
		"another member": func() error { c := b; c.MemberID = "member-x"; return c.Verify(testDomain, "room-a") },
		"another buyer": func() error {
			c := b
			c.Buyer = crypto.PubkeyToAddress(testKey(t, "other").PublicKey)
			return c.Verify(testDomain, "room-a")
		},
	} {
		if err := check(); !errors.Is(err, ErrBinding) {
			t.Errorf("%s: err = %v; want ErrBinding", name, err)
		}
	}
}

func TestTheSaleRoomMustHoldOnlyTheSellerAndTheBoundBuyer(t *testing.T) {
	b := Binding{MemberID: "buyer"}
	if err := CheckRoom([]Member{{MemberID: "seller", Kind: "agent"}, {MemberID: "buyer", Kind: "agent"}}, "seller", b); err != nil {
		t.Fatalf("seller and buyer: %v", err)
	}
	if err := CheckRoom([]Member{{MemberID: "seller"}, {MemberID: "buyer"}}, "seller", b); err != nil {
		t.Fatalf("seller and buyer from records without a kind: %v", err)
	}
	for name, members := range map[string][]Member{
		"buyer not joined":    {{MemberID: "seller"}},
		"a third agent":       {{MemberID: "seller"}, {MemberID: "buyer"}, {MemberID: "stranger"}},
		"a storage member":    {{MemberID: "seller"}, {MemberID: "buyer"}, {MemberID: "bucket", Kind: "object_storage"}},
		"the buyer a webhook": {{MemberID: "seller"}, {MemberID: "buyer", Kind: "webhook"}},
	} {
		if err := CheckRoom(members, "seller", b); !errors.Is(err, ErrRoomNotPrivate) {
			t.Errorf("%s: err = %v; want ErrRoomNotPrivate", name, err)
		}
	}
}

// buildTestPackage builds a package for a random dataset and returns its path, the build result and the dataset.
func buildTestPackage(t *testing.T, seller, buyer *ecdsaKey, size int) (string, Built, []byte) {
	t.Helper()
	dir := t.TempDir()
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	plainPath := filepath.Join(dir, "dataset.bin")
	if err := os.WriteFile(plainPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	packagePath := filepath.Join(dir, "out.package")
	built, err := Build(context.Background(), BuildInput{
		PlainPath: plainPath, PackagePath: packagePath, TreeDir: filepath.Join(dir, "seller-trees"),
		Domain: testDomain, Secret: common.HexToHash("0x5ec2e7"), Name: "dataset.bin",
		Terms: func(root common.Hash, sizeBytes uint64) (datasale.Offer, error) {
			return datasale.Offer{
				Seller: crypto.PubkeyToAddress(seller.PublicKey), Buyer: crypto.PubkeyToAddress(buyer.PublicKey),
				PlainRoot: root, SizeBytes: sizeBytes, Price: big.NewInt(1e16), FundDeadline: 1_000_000,
				RevealPeriod: 1800, ChallengePeriod: 7200, Nonce: big.NewInt(42),
			}, nil
		},
		Sign: func(digest common.Hash) ([]byte, error) { return datasale.SignDigest(seller.PrivateKey, digest) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return packagePath, built, data
}

func TestABuiltPackagePassesTheBuyersChecksAndDecrypts(t *testing.T) {
	seller, buyer := testKey(t, "seller"), testKey(t, "buyer")
	path, built, data := buildTestPackage(t, seller, buyer, 3*datasale.LeafBytes+77)
	src, err := OpenFileSource(path)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if src.Size() != PackageSize(4) {
		t.Fatalf("package is %d bytes; want %d", src.Size(), PackageSize(4))
	}
	checked, err := CheckPackage(context.Background(), src, testDomain, crypto.PubkeyToAddress(buyer.PublicKey), t.TempDir())
	if err != nil {
		t.Fatalf("CheckPackage = %v", err)
	}
	if checked.SaleID != built.SaleID || checked.Header.Name != "dataset.bin" {
		t.Fatalf("checked %+v; built sale %s", checked.Header, built.SaleID)
	}
	ciphertext, _ := Ciphertext(src, checked.Header)
	hashes, _ := HashList(src, checked.Header)
	var out bytes.Buffer
	result, err := datasale.Decrypt(context.Background(), ciphertext, checked.Header.Offer.SizeBytes, built.Key, hashes, &out)
	if err != nil || result.MismatchCount != 0 || !bytes.Equal(out.Bytes(), data) {
		t.Fatalf("Decrypt = %s, %v", result, err)
	}

	// Anyone else than the named buyer refuses the package.
	if _, err := CheckPackage(context.Background(), src, testDomain, crypto.PubkeyToAddress(seller.PublicKey), t.TempDir()); !errors.Is(err, ErrPackageCheck) {
		t.Fatalf("another buyer: err = %v", err)
	}
}

func TestATamperedPackageFailsTheBuyersChecks(t *testing.T) {
	seller, buyer := testKey(t, "seller"), testKey(t, "buyer")
	path, _, _ := buildTestPackage(t, seller, buyer, 2*datasale.LeafBytes)
	original, _ := os.ReadFile(path)
	tamper := func(offset int64) string {
		data := append([]byte(nil), original...)
		data[offset] ^= 1
		p := filepath.Join(t.TempDir(), "tampered.package")
		_ = os.WriteFile(p, data, 0o600)
		return p
	}
	for name, p := range map[string]string{
		"a ciphertext byte": tamper(CiphertextOffset(2) + 5),
		"a hash list byte":  tamper(HashListOffset() + 3),
	} {
		src, _ := OpenFileSource(p)
		_, err := CheckPackage(context.Background(), src, testDomain, crypto.PubkeyToAddress(buyer.PublicKey), t.TempDir())
		src.Close()
		if !errors.Is(err, ErrPackageCheck) {
			t.Errorf("%s: err = %v; want ErrPackageCheck", name, err)
		}
	}
	short := filepath.Join(t.TempDir(), "short.package")
	_ = os.WriteFile(short, original[:len(original)-1], 0o600)
	src, _ := OpenFileSource(short)
	defer src.Close()
	if _, err := ReadHeader(src); !errors.Is(err, ErrPackage) {
		t.Fatalf("a truncated package: err = %v; want ErrPackage", err)
	}
}

// rangeServer serves files the way the agent's inbox route does, honouring Range.
type rangeServer struct {
	files map[string][]byte
}

func (s rangeServer) Do(_ context.Context, _, path, _ string, _, out any) error {
	var objects []InboxObject
	for name, data := range s.files {
		objects = append(objects, InboxObject{RoomID: "room-a", TransferID: name, Filename: name, SizeBytes: int64(len(data)), CompletedAt: time.Now()})
	}
	encoded, _ := json.Marshal(map[string]any{"objects": objects})
	return json.Unmarshal(encoded, out)
}

func (s rangeServer) OpenRange(_ context.Context, path string, offset, length int64) (*http.Response, error) {
	name := filepath.Base(path)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Range", "bytes="+itoa(offset)+"-"+itoa(offset+length-1))
	rec := httptest.NewRecorder()
	http.ServeContent(rec, req, name, time.Time{}, bytes.NewReader(s.files[name]))
	resp := rec.Result()
	if resp.StatusCode != http.StatusPartialContent {
		return nil, errors.New(resp.Status)
	}
	return resp, nil
}

func itoa(v int64) string { return big.NewInt(v).String() }

func TestTheBuyerReadsAPackageFromItsInboxThroughRanges(t *testing.T) {
	seller, buyer := testKey(t, "seller"), testKey(t, "buyer")
	path, built, _ := buildTestPackage(t, seller, buyer, 5*datasale.LeafBytes+1)
	data, _ := os.ReadFile(path)
	name := PackageName(built.SaleID)
	local := rangeServer{files: map[string][]byte{name: data, "notes.txt": []byte("not a package")}}
	objects, err := ListInbox(context.Background(), local, "room-a")
	if err != nil {
		t.Fatal(err)
	}
	packages := Packages(objects)
	if len(packages) != 1 || packages[0].Filename != name {
		t.Fatalf("Packages = %+v", packages)
	}
	src := InboxSource{Ctx: context.Background(), Local: local, Object: packages[0]}
	checked, err := CheckPackage(context.Background(), src, testDomain, crypto.PubkeyToAddress(buyer.PublicKey), t.TempDir())
	if err != nil || checked.SaleID != built.SaleID {
		t.Fatalf("CheckPackage over the inbox = %v", err)
	}
	leaf, err := CipherLeaf(src, checked.Header, 5)
	if err != nil || !bytes.Equal(leaf, data[CiphertextOffset(6)+5*datasale.LeafBytes:CiphertextOffset(6)+6*datasale.LeafBytes]) {
		t.Fatalf("CipherLeaf = %v", err)
	}
	if h, err := PlainHash(src, 2); err != nil || !bytes.Equal(h[:], data[HashListOffset()+64:HashListOffset()+96]) {
		t.Fatalf("PlainHash = %v", err)
	}
}

type ecdsaKey struct{ *ecdsa.PrivateKey }
