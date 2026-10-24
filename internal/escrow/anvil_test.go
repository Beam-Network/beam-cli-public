package escrow

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/datasale"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// TestSalesOnALocalChain deploys the frozen escrow bytecode on a local anvil node and runs an honest sale and a
// refunded sale end to end. It is skipped when anvil is not installed.
func TestSalesOnALocalChain(t *testing.T) {
	anvil, err := exec.LookPath("anvil")
	if err != nil {
		t.Skip("anvil is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	raw, client := startAnvil(t, ctx, anvil)
	chainID, err := client.ChainID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seller, buyer := testAccount(t, "seller"), testAccount(t, "buyer")
	for _, a := range []Account{seller, buyer} {
		if err := raw.CallContext(ctx, nil, "anvil_setBalance", a.Address(), hexutil.EncodeBig(big.NewInt(1e18))); err != nil {
			t.Fatal(err)
		}
	}
	escrowAddress := deployEscrow(t, ctx, client, chainID, seller)
	contract := Contract{Address: escrowAddress, Backend: client}
	limits, err := contract.Limits(ctx)
	if err != nil || limits.MaxPrice.Cmp(big.NewInt(1e18)) != 0 || limits.MinRevealPeriod != 5 {
		t.Fatalf("Limits = %+v, %v", limits, err)
	}
	mine := func(blocks uint64) {
		if err := raw.CallContext(ctx, nil, "anvil_mine", hexutil.EncodeUint64(blocks)); err != nil {
			t.Fatal(err)
		}
	}
	senderFor := func(a Account) *Sender {
		return &Sender{Backend: NewFailover(5*time.Second, client), ChainID: chainID, Primary: a, Poll: 20 * time.Millisecond}
	}
	price := big.NewInt(1e15)
	domain := datasale.Domain{ChainID: chainID, Escrow: escrowAddress}

	t.Run("honest sale settles and pays the seller directly", func(t *testing.T) {
		sale := prepareSale(t, ctx, client, domain, seller, buyer, price, 1, -1)
		fund(t, ctx, senderFor(buyer), escrowAddress, sale, price)
		record, err := contract.Record(ctx, sale.id)
		if err != nil || record.State != StateFunded || record.Commitments != sale.commitments.Hash() {
			t.Fatalf("after funding: %+v, %v", record, err)
		}
		out := reveal(t, ctx, senderFor(seller), escrowAddress, sale, record.Deadline)
		events, err := DecodeEvents(escrowAddress, out.Receipt.Logs)
		if err != nil || len(events) != 1 || events[0].Name != "KeyRevealed" || events[0].Key != sale.key {
			t.Fatalf("reveal events = %+v, %v", events, err)
		}
		mine(10)
		before, _ := client.BalanceAt(ctx, seller.Address(), nil)
		settleData, _ := SettleData(sale.offer)
		out, err = senderFor(buyer).Send(ctx, Request{Label: "settle", To: escrowAddress, Data: settleData, GasLimit: 200_000})
		if err != nil {
			t.Fatal(err)
		}
		after, _ := client.BalanceAt(ctx, seller.Address(), nil)
		if new(big.Int).Sub(after, before).Cmp(price) != 0 {
			t.Fatalf("the seller received %s; want %s", new(big.Int).Sub(after, before), price)
		}
		events, _ = DecodeEvents(escrowAddress, out.Receipt.Logs)
		if names(events) != "Settled,Paid" {
			t.Fatalf("settle events = %s", names(events))
		}
	})

	t.Run("a corrupted leaf is challenged and the buyer refunded", func(t *testing.T) {
		sale := prepareSale(t, ctx, client, domain, seller, buyer, price, 2, 1)
		fund(t, ctx, senderFor(buyer), escrowAddress, sale, price)
		record, _ := contract.Record(ctx, sale.id)
		reveal(t, ctx, senderFor(seller), escrowAddress, sale, record.Deadline)
		var plaintext bytes.Buffer
		result, err := datasale.Decrypt(ctx, bytes.NewReader(sale.ciphertext), sale.offer.SizeBytes, sale.key, bytes.NewReader(sale.hashList), &plaintext)
		if err != nil || result.MismatchCount != 1 || result.Mismatches[0] != 1 {
			t.Fatalf("Decrypt = %s %v, %v", result, result.Mismatches, err)
		}
		proof := sale.proof(t, result.Mismatches[0])
		data, err := ChallengeData(sale.offer, sale.commitments, sale.key, proof)
		if err != nil {
			t.Fatal(err)
		}
		before, _ := client.BalanceAt(ctx, buyer.Address(), nil)
		out, err := senderFor(seller).Send(ctx, Request{Label: "challenge", To: escrowAddress, Data: data, GasLimit: 600_000})
		if err != nil {
			t.Fatal(err)
		}
		after, _ := client.BalanceAt(ctx, buyer.Address(), nil)
		events, _ := DecodeEvents(escrowAddress, out.Receipt.Logs)
		if names(events) != "Challenged,Refunded,Paid" || new(big.Int).Sub(after, before).Cmp(price) != 0 {
			t.Fatalf("challenge events = %s, buyer received %s", names(events), new(big.Int).Sub(after, before))
		}
		if record, _ := contract.Record(ctx, sale.id); record.State != StateRefunded {
			t.Fatalf("state = %s; want refunded", record.State)
		}
	})
}

type testSale struct {
	offer       datasale.Offer
	id          common.Hash
	key         common.Hash
	commitments Commitments
	signature   []byte
	ciphertext  []byte
	hashList    []byte
	plainDir    string
	cipherDir   string
}

func (s testSale) proof(t *testing.T, index uint64) LeafProof {
	t.Helper()
	plainTree, err := datasale.OpenTree(s.plainDir)
	if err != nil {
		t.Fatal(err)
	}
	cipherTree, err := datasale.OpenTree(s.cipherDir)
	if err != nil {
		t.Fatal(err)
	}
	pp, _ := plainTree.Proof(index)
	cp, _ := cipherTree.Proof(index)
	return LeafProof{
		Index:       index,
		CipherLeaf:  s.ciphertext[index*datasale.LeafBytes : (index+1)*datasale.LeafBytes],
		PlainHash:   common.BytesToHash(s.hashList[index*32 : (index+1)*32]),
		CipherProof: cp,
		PlainProof:  pp,
	}
}

// prepareSale builds a 3-leaf sale; when corrupt >= 0 the seller delivers other data at that leaf.
func prepareSale(t *testing.T, ctx context.Context, client *ethclient.Client, domain datasale.Domain, seller, buyer Account, price *big.Int, nonce int64, corrupt int) testSale {
	t.Helper()
	size := uint64(2*datasale.LeafBytes + 123)
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	var hashList bytes.Buffer
	plainWriter, _ := datasale.NewTreeWriter(filepath.Join(dir, "plain"))
	plain, err := datasale.BuildPlain(ctx, bytes.NewReader(data), size, &hashList, plainWriter)
	if err != nil {
		t.Fatal(err)
	}
	head, err := client.BlockNumber(ctx)
	if err != nil {
		t.Fatal(err)
	}
	offer := datasale.Offer{
		Seller: seller.Address(), Buyer: buyer.Address(), PlainRoot: plain.Root, SizeBytes: size, Price: price,
		FundDeadline: head + 100, RevealPeriod: 5, ChallengePeriod: 5, Nonce: big.NewInt(nonce),
	}
	id, err := datasale.SaleID(domain, offer)
	if err != nil {
		t.Fatal(err)
	}
	key := datasale.SellerKey(common.BytesToHash([]byte(fmt.Sprintf("secret %d", nonce))), id)
	delivered := data
	if corrupt >= 0 {
		delivered = append([]byte(nil), data...)
		delivered[corrupt*datasale.LeafBytes] ^= 0xFF
	}
	var ciphertext bytes.Buffer
	cipherWriter, _ := datasale.NewTreeWriter(filepath.Join(dir, "cipher"))
	root, err := datasale.Encrypt(ctx, bytes.NewReader(delivered), size, id, key, &ciphertext, cipherWriter)
	if err != nil {
		t.Fatal(err)
	}
	commitments := Commitments{CipherRoot: root, KeyCommitment: datasale.KeyCommitment(id, key)}
	digest, err := datasale.SaleDigest(domain, offer, commitments.CipherRoot, commitments.KeyCommitment)
	if err != nil {
		t.Fatal(err)
	}
	signature, err := datasale.SignDigest(seller.Key, digest)
	if err != nil {
		t.Fatal(err)
	}
	return testSale{offer, id, key, commitments, signature, ciphertext.Bytes(), hashList.Bytes(), filepath.Join(dir, "plain"), filepath.Join(dir, "cipher")}
}

func fund(t *testing.T, ctx context.Context, s *Sender, escrow common.Address, sale testSale, price *big.Int) {
	t.Helper()
	data, err := FundData(sale.offer, sale.commitments, sale.signature)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(ctx, Request{Label: "fund", To: escrow, Data: data, Value: price, GasLimit: 300_000}); err != nil {
		t.Fatalf("fund: %v", err)
	}
}

func reveal(t *testing.T, ctx context.Context, s *Sender, escrow common.Address, sale testSale, deadline uint64) Outcome {
	t.Helper()
	data, err := RevealData(sale.offer, sale.commitments, sale.key)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Send(ctx, Request{Label: "reveal", To: escrow, Data: data, GasLimit: 150_000, Deadline: deadline, Margin: 2, Secret: true, AnySender: true})
	if err != nil {
		t.Fatalf("reveal: %v", err)
	}
	return out
}

func deployEscrow(t *testing.T, ctx context.Context, client *ethclient.Client, chainID *big.Int, deployer Account) common.Address {
	t.Helper()
	return deployEscrowWith(t, ctx, client, chainID, deployer, big.NewInt(1e18), 5, 5)
}

// deployEscrowWith deploys the frozen escrow bytecode with the given constructor arguments.
func deployEscrowWith(t *testing.T, ctx context.Context, client *ethclient.Client, chainID *big.Int, deployer Account, maxPrice *big.Int, minReveal, minChallenge uint64) common.Address {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "DataSaleEscrow.initcode"))
	if err != nil {
		t.Fatal(err)
	}
	initcode, err := hexutil.Decode(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	args, err := abi.Arguments{{Type: mustType("uint256")}, {Type: mustType("uint64")}, {Type: mustType("uint64")}}.Pack(maxPrice, minReveal, minChallenge)
	if err != nil {
		t.Fatal(err)
	}
	return deployCode(t, ctx, client, chainID, deployer, append(initcode, args...))
}

// deployCode sends a contract creation with data and returns the new contract's address.
func deployCode(t *testing.T, ctx context.Context, client *ethclient.Client, chainID *big.Int, deployer Account, data []byte) common.Address {
	t.Helper()
	nonce, _ := client.PendingNonceAt(ctx, deployer.Address())
	gasPrice, _ := client.SuggestGasPrice(ctx)
	tx, err := types.SignNewTx(deployer.Key, types.NewEIP155Signer(chainID), &types.LegacyTx{Nonce: nonce, GasPrice: gasPrice, Gas: 3_000_000, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SendTransaction(ctx, tx); err != nil {
		t.Fatal(err)
	}
	for {
		receipt, err := client.TransactionReceipt(ctx, tx.Hash())
		if err == nil {
			if receipt.Status != types.ReceiptStatusSuccessful {
				t.Fatal("deployment reverted")
			}
			return receipt.ContractAddress
		}
		select {
		case <-ctx.Done():
			t.Fatal("deployment was not mined")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func mustType(name string) abi.Type {
	typ, err := abi.NewType(name, "", nil)
	if err != nil {
		panic(err)
	}
	return typ
}

func names(events []Event) string {
	var out []string
	for _, e := range events {
		out = append(out, e.Name)
	}
	return strings.Join(out, ",")
}

// A call that fails inside the EVM without reverting is an answer about the call: the first endpoint's answer is
// returned at once, rather than every endpoint being tried and the call reported as none answering.
func TestAnEVMFailureIsAnAnswerAboutTheCall(t *testing.T) {
	anvil, err := exec.LookPath("anvil")
	if err != nil {
		t.Skip("anvil is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	raw, client := startAnvil(t, ctx, anvil)
	// A stack underflow, the INVALID opcode, a jump to a non-destination, and a loop that runs out of gas.
	for i, code := range []string{"0x01", "0xfe", "0x600056", "0x5b600056"} {
		target := common.BigToAddress(big.NewInt(int64(0x1000 + i)))
		if err := raw.CallContext(ctx, nil, "anvil_setCode", target, code); err != nil {
			t.Fatal(err)
		}
		first, second := &countingCalls{Backend: client}, &countingCalls{Backend: client}
		_, err := NewFailover(5*time.Second, first, second).CallContract(ctx, ethereum.CallMsg{To: &target, Gas: 100_000}, nil)
		if !IsRequestError(err) || errors.Is(err, ErrUnreachable) || first.calls != 1 || second.calls != 0 {
			t.Errorf("code %s: err = %v after %d and %d calls; want the first endpoint's answer, at once", code, err, first.calls, second.calls)
		}
	}
}

// countingCalls counts the calls an endpoint is asked to simulate.
type countingCalls struct {
	Backend
	calls int
}

func (c *countingCalls) CallContract(ctx context.Context, msg ethereum.CallMsg, block *big.Int) ([]byte, error) {
	c.calls++
	return c.Backend.CallContract(ctx, msg, block)
}

func startAnvil(t *testing.T, ctx context.Context, anvil string) (*rpc.Client, *ethclient.Client) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	cmd := exec.CommandContext(ctx, anvil, "--silent", "--port", fmt.Sprint(port), "--chain-id", "945")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	for {
		raw, err := rpc.DialContext(ctx, url)
		if err == nil {
			client := ethclient.NewClient(raw)
			if _, err := client.BlockNumber(ctx); err == nil {
				return raw, client
			}
			raw.Close()
		}
		select {
		case <-ctx.Done():
			t.Fatal("anvil did not start")
		case <-time.After(50 * time.Millisecond):
		}
	}
}
