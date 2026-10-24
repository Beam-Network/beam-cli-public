//go:build !windows

package command

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/Beam-Network/beam-cli-public/internal/auth"
	"github.com/Beam-Network/beam-cli-public/internal/config"
	"github.com/Beam-Network/beam-cli-public/internal/datasale"
	"github.com/Beam-Network/beam-cli-public/internal/escrow"
	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/saleroom"
	"github.com/Beam-Network/beam-cli-public/internal/salestate"
	"github.com/Beam-Network/beam-cli-public/internal/testutil"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
)

// saleChain is a local anvil node running the frozen escrow bytecode, deployed with 5-block floors so a test can
// move past every deadline.
type saleChain struct {
	url    string
	raw    *rpc.Client
	client *ethclient.Client
	escrow common.Address
	domain datasale.Domain
}

func init() {
	newSaleKeyring = memorySaleKeyring
}

// memorySaleKeyring stands in for the operating-system keyring the sale wallet keeps its keys in, with one keyring per
// state directory.
func memorySaleKeyring(dir string) auth.Keyring {
	keyring, _ := memoryKeyrings.LoadOrStore("sale wallet "+dir, &testutil.MemoryKeyring{})
	return keyring.(*testutil.MemoryKeyring)
}

func startSaleChain(t *testing.T, ctx context.Context) *saleChain {
	t.Helper()
	anvil, err := exec.LookPath("anvil")
	if err != nil {
		t.Skip("anvil is not installed")
	}
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
	chain := &saleChain{url: fmt.Sprintf("http://127.0.0.1:%d", port)}
	for {
		if chain.raw, err = rpc.DialContext(ctx, chain.url); err == nil {
			chain.client = ethclient.NewClient(chain.raw)
			if _, err = chain.client.BlockNumber(ctx); err == nil {
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("anvil did not start")
		case <-time.After(50 * time.Millisecond):
		}
	}
	deployer := saleTestKey(t, "deployer")
	chain.fund(t, ctx, crypto.PubkeyToAddress(deployer.PublicKey))
	raw, err := os.ReadFile(filepath.Join("..", "escrow", "testdata", "DataSaleEscrow.initcode"))
	if err != nil {
		t.Fatal(err)
	}
	initcode, err := hexutil.Decode(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	types3 := abi.Arguments{{Type: abiType(t, "uint256")}, {Type: abiType(t, "uint64")}, {Type: abiType(t, "uint64")}}
	args, err := types3.Pack(big.NewInt(1e18), uint64(5), uint64(5))
	if err != nil {
		t.Fatal(err)
	}
	gasPrice, _ := chain.client.SuggestGasPrice(ctx)
	tx, err := types.SignNewTx(deployer, types.NewEIP155Signer(big.NewInt(945)), &types.LegacyTx{GasPrice: gasPrice, Gas: 3_000_000, Data: append(initcode, args...)})
	if err != nil {
		t.Fatal(err)
	}
	if err := chain.client.SendTransaction(ctx, tx); err != nil {
		t.Fatal(err)
	}
	receipt := chain.waitReceipt(t, ctx, tx.Hash())
	chain.escrow = receipt.ContractAddress
	chain.domain = datasale.Domain{ChainID: big.NewInt(945), Escrow: chain.escrow}
	return chain
}

func abiType(t *testing.T, name string) abi.Type {
	typ, err := abi.NewType(name, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return typ
}

func (c *saleChain) waitReceipt(t *testing.T, ctx context.Context, hash common.Hash) *types.Receipt {
	t.Helper()
	for {
		if receipt, err := c.client.TransactionReceipt(ctx, hash); err == nil {
			if receipt.Status != types.ReceiptStatusSuccessful {
				t.Fatalf("transaction %s reverted", hash)
			}
			return receipt
		}
		select {
		case <-ctx.Done():
			t.Fatalf("transaction %s was not mined", hash)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (c *saleChain) fund(t *testing.T, ctx context.Context, account common.Address) {
	t.Helper()
	if err := c.raw.CallContext(ctx, nil, "anvil_setBalance", account, hexutil.EncodeBig(new(big.Int).Mul(big.NewInt(10), big.NewInt(1e18)))); err != nil {
		t.Fatal(err)
	}
}

func (c *saleChain) mine(t *testing.T, ctx context.Context, blocks uint64) {
	t.Helper()
	if err := c.raw.CallContext(ctx, nil, "anvil_mine", hexutil.EncodeUint64(blocks)); err != nil {
		t.Fatal(err)
	}
}

func (c *saleChain) record(t *testing.T, ctx context.Context, saleID common.Hash) escrow.Record {
	t.Helper()
	record, err := escrow.Contract{Address: c.escrow, Backend: c.client}.Record(ctx, saleID)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func (c *saleChain) balance(t *testing.T, ctx context.Context, account common.Address) *big.Int {
	t.Helper()
	balance, err := c.client.BalanceAt(ctx, account, nil)
	if err != nil {
		t.Fatal(err)
	}
	return balance
}

func saleTestKey(t *testing.T, seed string) *ecdsa.PrivateKey {
	t.Helper()
	key, err := crypto.ToECDSA(crypto.Keccak256([]byte("sale test " + seed)))
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// saleRoomNet stands in for two agents in one sale Room: the seller's publishes files, and delivering one copies it
// into the buyer's inbox, as a Room delivery does.
type saleRoomNet struct {
	mu        sync.Mutex
	dir       string
	published []string
	inbox     []string // by transfer index; "" once deleted
	// The first missed publications fail with missReason instead of reaching the buyer.
	publications int
	missed       int
	missReason   string
}

func newSaleRoomNet(t *testing.T) *saleRoomNet {
	return &saleRoomNet{dir: t.TempDir()}
}

const (
	saleTestRoom   = "btr_room_sale"
	saleTestSeller = "btr_member_seller"
	saleTestBuyer  = "btr_member_buyer"
)

func (n *saleRoomNet) deliver(path string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.published = append(n.published, path)
	copied := filepath.Join(n.dir, fmt.Sprint(len(n.inbox)), filepath.Base(path))
	data, err := os.ReadFile(path)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(copied), 0o700)
	}
	if err == nil {
		err = os.WriteFile(copied, data, 0o600)
	}
	if err != nil {
		copied = ""
	}
	n.inbox = append(n.inbox, copied)
}

func (n *saleRoomNet) sources() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.published...)
}

// held counts the objects still in the buyer's inbox.
func (n *saleRoomNet) held() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	count := 0
	for _, path := range n.inbox {
		if path != "" {
			count++
		}
	}
	return count
}

func (n *saleRoomNet) object(index int) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if index < 0 || index >= len(n.inbox) {
		return ""
	}
	return n.inbox[index]
}

func (n *saleRoomNet) drop(index int) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if index < 0 || index >= len(n.inbox) || n.inbox[index] == "" {
		return false
	}
	_ = os.Remove(n.inbox[index])
	n.inbox[index] = ""
	return true
}

// agent serves one party's local agent API: the room routes a sale uses, and the inbox.
func (n *saleRoomNet) agent(t *testing.T, member string) string {
	return testutil.LocalHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		reply := func(status int, payload any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(payload)
		}
		room := "/v1/rooms/" + saleTestRoom
		record := map[string]any{"room": map[string]any{"room_id": saleTestRoom}, "membership": map[string]any{"member_id": member}}
		switch {
		case r.Method == http.MethodGet && path == tunnel.StatusPath:
			reply(http.StatusOK, tunnel.Status{Ready: true, ProtocolVersion: tunnel.ProtocolVersion, ProtocolMinVersion: tunnel.ProtocolMinVersion,
				Capabilities: []string{"btr.object.publish", "btr.inbox.list", "btr.inbox.read", "btr.inbox.delete"}})
		case r.Method == http.MethodGet && path == "/v1/agent-connection":
			reply(http.StatusOK, tunnel.AgentConnectionStatus{Prepared: true, Registered: true, Connected: true, State: "connected",
				CoordinatorURL: "https://coordinator.example"})
		case r.Method == http.MethodPost && (path == "/v1/rooms" || path == room+"/join"):
			reply(http.StatusCreated, record)
		case r.Method == http.MethodGet && path == room:
			reply(http.StatusOK, record)
		case r.Method == http.MethodPost && path == room+"/channels":
			reply(http.StatusCreated, map[string]any{"channel": map[string]any{"channel_id": "btr_channel_sale", "kind": "object"}})
		case r.Method == http.MethodGet && path == room+"/channels/btr_channel_sale":
			reply(http.StatusOK, map[string]any{"channel": map[string]any{"channel_id": "btr_channel_sale", "kind": "object",
				"limits": map[string]any{"publication_ttl": int64(2 * time.Hour)}}})
		case r.Method == http.MethodPost && path == room+"/invitations":
			reply(http.StatusCreated, map[string]any{"invitation": map[string]any{"invitation_id": "btr_inv_1", "expires_at": time.Now().Add(time.Hour)}, "invitation_token": "invitation-token"})
		case r.Method == http.MethodGet && path == room+"/memberships":
			reply(http.StatusOK, map[string]any{"memberships": []any{map[string]any{"member_id": saleTestSeller, "kind": "agent"}, map[string]any{"member_id": saleTestBuyer, "kind": "agent"}}})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/objects/publish"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			file, _ := body["file"].(string)
			n.mu.Lock()
			n.publications++
			publication, miss := n.publications, n.publications <= n.missed
			n.mu.Unlock()
			if !miss {
				n.deliver(file)
			}
			reply(http.StatusAccepted, map[string]any{"publication_id": fmt.Sprintf("btr_pub_%d", publication), "state": "pending"})
		case r.Method == http.MethodGet && strings.Contains(path, "/objects/btr_pub_") && strings.HasSuffix(path, "/status"):
			var publication int
			fmt.Sscanf(path[strings.Index(path, "/objects/btr_pub_")+len("/objects/btr_pub_"):], "%d", &publication)
			n.mu.Lock()
			miss, reason := publication <= n.missed, n.missReason
			n.mu.Unlock()
			transfer, delivery := "completed", map[string]any{"member_id": saleTestBuyer, "state": "delivered"}
			if miss {
				transfer, delivery = "failed", map[string]any{"member_id": saleTestBuyer, "state": "unavailable", "unavailable_reason": reason}
			}
			reply(http.StatusOK, map[string]any{"status": map[string]any{"publisher": map[string]any{
				"room_transfer": map[string]any{"transfer_id": fmt.Sprintf("transfer-%d", publication), "status": transfer},
				"deliveries":    []any{delivery},
			}}})
		case r.Method == http.MethodGet && path == room+"/inbox":
			var objects []any
			for i := range n.sources() {
				file := n.object(i)
				info, err := os.Stat(file)
				if file == "" || err != nil {
					continue
				}
				objects = append(objects, map[string]any{"room_id": saleTestRoom, "transfer_id": fmt.Sprintf("transfer-%d", i),
					"filename": filepath.Base(file), "size_bytes": info.Size(), "completed_at": info.ModTime()})
			}
			reply(http.StatusOK, map[string]any{"objects": objects})
		case strings.HasPrefix(path, room+"/inbox/transfer-"):
			var index int
			fmt.Sscanf(strings.TrimPrefix(path, room+"/inbox/transfer-"), "%d", &index)
			if r.Method == http.MethodDelete {
				if !n.drop(index) {
					reply(http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found"}})
					return
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			f, err := os.Open(n.object(index))
			if err != nil {
				reply(http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found"}})
				return
			}
			defer f.Close()
			http.ServeContent(w, r, filepath.Base(f.Name()), time.Time{}, f)
		default:
			reply(http.StatusNotFound, map[string]any{"error": map[string]any{"code": "not_found", "message": "unexpected " + r.Method + " " + path}})
		}
	}))
}

// saleParty is one side of a sale: an App with its own state directory, agent and wallet. Everything its commands
// print is kept, to check that no secret ever appears in it.
type saleParty struct {
	app     *App
	cfg     config.Config
	paths   config.Paths
	address common.Address
	mu      sync.Mutex
	printed bytes.Buffer
}

func newSaleParty(t *testing.T, ctx context.Context, chain *saleChain, net *saleRoomNet, member string) *saleParty {
	t.Helper()
	dir := t.TempDir()
	app := newWebhookApp()
	app.sales.poll = 20 * time.Millisecond
	app.sales.margin = 2
	app.sales.finalized = func(ctx context.Context, backend escrow.Backend) (uint64, error) { return backend.BlockNumber(ctx) }
	p := &saleParty{
		app: app,
		cfg: config.Config{
			AgentSocket: net.agent(t, member), Output: "json",
			SaleChainID: "945", SaleRPCURLs: chain.url, SaleEscrow: chain.escrow.Hex(),
		},
		paths: config.Paths{Dir: dir},
	}
	result, err := p.run(ctx, "wallet", "create")
	if err != nil {
		t.Fatalf("wallet create: %v", err)
	}
	p.address = common.HexToAddress(result["address"].(string))
	chain.fund(t, ctx, p.address)
	return p
}

func (p *saleParty) run(ctx context.Context, args ...string) (map[string]any, error) {
	var stdout, stderr bytes.Buffer
	renderer := output.Renderer{Mode: output.JSON, Out: &stdout, Err: &stderr}
	err := p.app.saleCommand(ctx, args, p.cfg, p.paths, renderer)
	p.mu.Lock()
	p.printed.Write(stdout.Bytes())
	p.printed.Write(stderr.Bytes())
	if err != nil {
		cliErr := asCLIError(err)
		fmt.Fprintln(&p.printed, cliErr.Message, cliErr.Hint)
	}
	p.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return nil, fmt.Errorf("output %q: %w", stdout.String(), err)
	}
	return result, nil
}

func randomDataset(t *testing.T, size int) (string, []byte) {
	t.Helper()
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "dataset.bin")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, data
}

func errorKind(err error) string {
	if err == nil {
		return ""
	}
	return asCLIError(err).Kind
}

func TestAnHonestSaleRunsFromTheRoomToSettlement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	chain := startSaleChain(t, ctx)
	net := newSaleRoomNet(t)
	seller := newSaleParty(t, ctx, chain, net, saleTestSeller)
	buyer := newSaleParty(t, ctx, chain, net, saleTestBuyer)

	invitation := filepath.Join(t.TempDir(), "invitation")
	room, err := seller.run(ctx, "room", "create", "--api-key", "beam-key-test", "--out", invitation)
	if err != nil || room["room_id"] != saleTestRoom {
		t.Fatalf("room create = %v, %v", room, err)
	}
	binding := filepath.Join(t.TempDir(), "binding.json")
	if _, err := buyer.run(ctx, "join", saleTestRoom, "--invitation-file", invitation, "--out", binding); err != nil {
		t.Fatalf("join: %v", err)
	}

	dataset, data := randomDataset(t, 5*datasale.LeafBytes+321)
	output := filepath.Join(t.TempDir(), "bought.bin")
	before := chain.balance(t, ctx, seller.address)
	var wg sync.WaitGroup
	var offered, bought map[string]any
	var offerErr, buyErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		offered, offerErr = seller.run(ctx, "offer", saleTestRoom, "--file", dataset, "--price", "0.05", "--binding", binding,
			"--reveal-period", "5", "--challenge-period", "5", "--fund-within", "50", "--tao-usd", "300")
	}()
	go func() {
		defer wg.Done()
		bought, buyErr = buyer.run(ctx, "buy", saleTestRoom, "--out", output, "--max-price", "0.05", "--wait", "1m")
	}()
	wg.Wait()
	if offerErr != nil || buyErr != nil {
		t.Fatalf("offer: %v\nbuy: %v", offerErr, buyErr)
	}
	if offered["escrow_state"] != "key_released" || bought["phase"] != salestate.PhaseVerified {
		t.Fatalf("offer = %v\nbuy = %v", offered, bought)
	}
	got, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("the bought dataset differs from the one sold: %v", err)
	}
	saleID := offered["sale_id"].(string)
	if bought["sale_id"] != saleID {
		t.Fatalf("the parties disagree on the sale: %s and %s", saleID, bought["sale_id"])
	}
	if _, err := os.Stat(net.sources()[0]); !os.IsNotExist(err) {
		t.Fatalf("the seller kept its package after delivering it: %v", err)
	}
	if net.held() != 0 {
		t.Fatal("the buyer kept the delivered package after every leaf checked out")
	}

	// Before the challenge window closes, resuming leaves the sale as it is.
	if early, err := seller.run(ctx, "resume", saleID); err != nil || early["escrow_state"] != "key_released" {
		t.Fatalf("early resume = %v, %v", early, err)
	}
	chain.mine(t, ctx, 6)
	settled, err := seller.run(ctx, "resume", saleID)
	if err != nil || settled["outcome"] != salestate.OutcomeSettled {
		t.Fatalf("resume = %v, %v", settled, err)
	}
	gained := new(big.Int).Sub(chain.balance(t, ctx, seller.address), before)
	if price := big.NewInt(5e16); gained.Cmp(price) >= 0 || gained.Cmp(new(big.Int).Sub(price, big.NewInt(1e15))) < 0 {
		t.Fatalf("the seller gained %s wei; want the 0.05 TAO price less gas", gained)
	}
	if closed, err := buyer.run(ctx, "resume", saleID, "--wait"); err != nil || closed["outcome"] != salestate.OutcomeSettled {
		t.Fatalf("buyer resume = %v, %v", closed, err)
	}
	status, err := seller.run(ctx, "status")
	if err != nil || len(status["sales"].([]any)) != 1 {
		t.Fatalf("status = %v, %v", status, err)
	}
	for _, p := range []*saleParty{seller, buyer} {
		p.assertNoSecretPrinted(t, common.HexToHash(saleID))
	}
}

// assertNoSecretPrinted checks that no command printed the wallet's keys, its secret, or the sale key derived from it.
func (p *saleParty) assertNoSecretPrinted(t *testing.T, saleID common.Hash) {
	t.Helper()
	wallet, err := saleWalletStore(p.paths).Load()
	if err != nil {
		t.Fatal(err)
	}
	printed := strings.ToLower(p.printed.String())
	secrets := map[string]string{
		"wallet key":    hex.EncodeToString(crypto.FromECDSA(wallet.Key)),
		"wallet secret": hex.EncodeToString(wallet.Secret[:]),
		"sale key":      hex.EncodeToString(datasale.SellerKey(wallet.Secret, saleID).Bytes()),
	}
	for name, value := range secrets {
		if strings.Contains(printed, value) {
			t.Errorf("the %s appeared in the output", name)
		}
	}
}

func TestTheSellerEnforcesItsRulesAndCanRevokeAWaitingOffer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	chain := startSaleChain(t, ctx)
	net := newSaleRoomNet(t)
	seller := newSaleParty(t, ctx, chain, net, saleTestSeller)
	buyer := newSaleParty(t, ctx, chain, net, saleTestBuyer)
	if _, err := seller.run(ctx, "room", "create", "--api-key", "beam-key-test"); err != nil {
		t.Fatalf("room create: %v", err)
	}
	binding := filepath.Join(t.TempDir(), "binding.json")
	if _, err := buyer.run(ctx, "join", saleTestRoom, "--invitation-token", "invitation-token", "--out", binding); err != nil {
		t.Fatalf("join: %v", err)
	}
	dataset, _ := randomDataset(t, 3*datasale.LeafBytes)
	offer := func(price, revealPeriod string) []string {
		return []string{"offer", saleTestRoom, "--file", dataset, "--price", price, "--binding", binding,
			"--reveal-period", revealPeriod, "--challenge-period", "5", "--tao-usd", "300"}
	}
	if _, err := seller.run(ctx, offer("0.03", "5")...); errorKind(err) != "sale_price_too_low" {
		t.Fatalf("an offer of $9 = %v; want sale_price_too_low", err)
	}
	if _, err := seller.run(ctx, offer("1.5", "5")...); errorKind(err) != "sale_price_too_high" {
		t.Fatalf("an offer above the escrow cap = %v; want sale_price_too_high", err)
	}
	if _, err := seller.run(ctx, offer("0.05", "4")...); err == nil || !strings.Contains(err.Error(), "--reveal-period must be 5 to") {
		t.Fatalf("a reveal period below the escrow floor = %v", err)
	}
	forged := filepath.Join(t.TempDir(), "forged.json")
	data, _ := os.ReadFile(binding)
	_ = os.WriteFile(forged, bytes.Replace(data, []byte(saleTestBuyer), []byte("btr_member_other"), 1), 0o600)
	if _, err := seller.run(ctx, "offer", saleTestRoom, "--file", dataset, "--price", "0.05", "--binding", forged, "--tao-usd", "300"); errorKind(err) != "sale_binding_invalid" {
		t.Fatalf("an offer with a tampered binding = %v; want sale_binding_invalid", err)
	}
	if len(net.sources()) != 0 {
		t.Fatal("a refused offer delivered something")
	}

	// An unfunded offer to this buyer on another escrow does not count against it here.
	other := salestate.Sale{SaleID: common.HexToHash("0x0ff1ce"), Role: salestate.Seller, ChainID: chain.domain.ChainID,
		Escrow: common.HexToAddress("0x00000000000000000000000000000000000E5C41"), Phase: salestate.PhaseOffered,
		Header: saleroom.Header{Offer: datasale.Offer{Buyer: buyer.address, FundDeadline: 1 << 40, Price: big.NewInt(1)}}}
	if err := (salestate.Store{Dir: seller.paths.Dir}).Save(&other); err != nil {
		t.Fatal(err)
	}

	// The seller waits for funding; revoking the offer from another command ends the wait.
	offered := make(chan error, 1)
	var result map[string]any
	go func() {
		var err error
		result, err = seller.run(ctx, offer("0.05", "5")...)
		offered <- err
	}()
	var saleID string
	for saleID == "" {
		select {
		case err := <-offered:
			t.Fatalf("the offer ended before it was delivered: %v", err)
		default:
		}
		status, err := seller.run(ctx, "status")
		if err != nil {
			t.Fatal(err)
		}
		for _, raw := range status["sales"].([]any) { // newest first
			if sale := raw.(map[string]any); sale["phase"] == salestate.PhaseOffered && sale["sale_id"] != other.SaleID.Hex() {
				saleID = sale["sale_id"].(string)
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := seller.run(ctx, "offer", saleTestRoom, "--file", dataset, "--price", "0.05", "--binding", binding, "--tao-usd", "300"); errorKind(err) != "sale_buyer_has_unfunded_offer" {
		t.Fatalf("a second offer to a buyer that has not funded = %v; want sale_buyer_has_unfunded_offer", err)
	}
	if revoked, err := seller.run(ctx, "revoke", saleID); err != nil || revoked["outcome"] != salestate.OutcomeRevoked {
		t.Fatalf("revoke = %v, %v", revoked, err)
	}
	if err := <-offered; err != nil || result["outcome"] != salestate.OutcomeRevoked {
		t.Fatalf("offer after the revoke = %v, %v", result, err)
	}
	if state := chain.record(t, ctx, common.HexToHash(saleID)).State; state != escrow.StateRevoked {
		t.Fatalf("escrow state = %s; want revoked", state)
	}
}

// cheatPackage writes the package a cheating seller delivers: the hash list commits to data, but the ciphertext
// encrypts data with one leaf changed. Every commitment is consistent, so the buyer's checks pass before funding.
func cheatPackage(t *testing.T, ctx context.Context, chain *saleChain, seller *ecdsa.PrivateKey, buyer common.Address, data []byte, corrupt int, price *big.Int) (string, common.Hash, common.Hash) {
	t.Helper()
	size := uint64(len(data))
	dir := t.TempDir()
	var hashList, ciphertext bytes.Buffer
	plainTree, _ := datasale.NewTreeWriter(filepath.Join(dir, "plain"))
	plain, err := datasale.BuildPlain(ctx, bytes.NewReader(data), size, &hashList, plainTree)
	if err != nil {
		t.Fatal(err)
	}
	head, _ := chain.client.BlockNumber(ctx)
	offer := datasale.Offer{
		Seller: crypto.PubkeyToAddress(seller.PublicKey), Buyer: buyer, PlainRoot: plain.Root, SizeBytes: size, Price: price,
		FundDeadline: head + 100, RevealPeriod: 5, ChallengePeriod: 5, Nonce: big.NewInt(time.Now().UnixNano()),
	}
	saleID, _ := datasale.SaleID(chain.domain, offer)
	key := datasale.SellerKey(common.HexToHash("0x5ec2e7"), saleID)
	delivered := append([]byte(nil), data...)
	if corrupt >= 0 {
		delivered[corrupt*datasale.LeafBytes] ^= 0xFF
	}
	cipherTree, _ := datasale.NewTreeWriter(filepath.Join(dir, "cipher"))
	cipherRoot, err := datasale.Encrypt(ctx, bytes.NewReader(delivered), size, saleID, key, &ciphertext, cipherTree)
	if err != nil {
		t.Fatal(err)
	}
	keyCommitment := datasale.KeyCommitment(saleID, key)
	digest, _ := datasale.SaleDigest(chain.domain, offer, cipherRoot, keyCommitment)
	signature, _ := datasale.SignDigest(seller, digest)
	region, err := saleroom.EncodeHeader(saleroom.Header{ChainID: chain.domain.ChainID, Escrow: chain.escrow, Offer: offer,
		CipherRoot: cipherRoot, KeyCommitment: keyCommitment, Signature: signature, Name: "cheat.bin"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, saleroom.PackageName(saleID))
	if err := os.WriteFile(path, append(append(region, hashList.Bytes()...), ciphertext.Bytes()...), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, saleID, key
}

// whenFunded waits, off the test goroutine, for the buyer to fund the sale, then runs then.
func whenFunded(ctx context.Context, chain *saleChain, saleID common.Hash, then func() error) <-chan error {
	done := make(chan error, 1)
	go func() {
		contract := escrow.Contract{Address: chain.escrow, Backend: chain.client}
		for {
			record, err := contract.Record(ctx, saleID)
			if err != nil {
				done <- err
				return
			}
			if record.State == escrow.StateFunded {
				done <- then()
				return
			}
			select {
			case <-ctx.Done():
				done <- ctx.Err()
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	return done
}

// revealAs reveals the key from the seller's key.
func revealAs(ctx context.Context, chain *saleChain, seller *ecdsa.PrivateKey, key common.Hash, pkg string) error {
	src, err := saleroom.OpenFileSource(pkg)
	if err != nil {
		return err
	}
	defer src.Close()
	header, err := saleroom.ReadHeader(src)
	if err != nil {
		return err
	}
	data, err := escrow.RevealData(header.Offer, escrow.Commitments{CipherRoot: header.CipherRoot, KeyCommitment: header.KeyCommitment}, key)
	if err != nil {
		return err
	}
	sender := &escrow.Sender{Backend: escrow.NewFailover(5*time.Second, chain.client), ChainID: chain.domain.ChainID,
		Primary: escrow.Account{Key: seller}, Poll: 20 * time.Millisecond}
	_, err = sender.Send(ctx, escrow.Request{To: chain.escrow, Data: data, GasLimit: gasReveal, Secret: true})
	return err
}

func TestABuyerChallengesACheatingSellerAndIsRefunded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	chain := startSaleChain(t, ctx)
	net := newSaleRoomNet(t)
	buyer := newSaleParty(t, ctx, chain, net, saleTestBuyer)
	if _, err := buyer.run(ctx, "join", saleTestRoom, "--invitation-token", "invitation-token"); err != nil {
		t.Fatalf("join: %v", err)
	}
	seller := saleTestKey(t, "cheating seller")
	chain.fund(t, ctx, crypto.PubkeyToAddress(seller.PublicKey))
	_, data := randomDataset(t, 4*datasale.LeafBytes+9)
	price := big.NewInt(5e16)
	pkg, saleID, key := cheatPackage(t, ctx, chain, seller, buyer.address, data, 2, price)
	net.deliver(pkg)

	before := chain.balance(t, ctx, buyer.address)
	output := filepath.Join(t.TempDir(), "bought.bin")
	revealed := whenFunded(ctx, chain, saleID, func() error { return revealAs(ctx, chain, seller, key, pkg) })
	result, err := buyer.run(ctx, "buy", saleTestRoom, "--out", output, "--max-price", "0.05")
	if err != nil || result["outcome"] != salestate.OutcomeRefunded {
		t.Fatalf("buy = %v, %v", result, err)
	}
	if err := <-revealed; err != nil {
		t.Fatalf("reveal: %v", err)
	}
	if state := chain.record(t, ctx, saleID).State; state != escrow.StateRefunded {
		t.Fatalf("escrow state = %s; want refunded", state)
	}
	if spent := new(big.Int).Sub(before, chain.balance(t, ctx, buyer.address)); spent.Cmp(big.NewInt(1e15)) > 0 {
		t.Fatalf("the buyer spent %s wei; want only gas", spent)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("a dataset that failed its checks was written to --out")
	}
	if _, err := os.Stat(output + ".unverified"); err != nil {
		t.Fatalf("the unverified dataset was not kept: %v", err)
	}
	if net.held() != 0 {
		t.Fatal("the buyer kept the delivered package after the sale ended")
	}
}

func TestABuyerReclaimsThePaymentWhenTheSellerNeverReveals(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	chain := startSaleChain(t, ctx)
	net := newSaleRoomNet(t)
	buyer := newSaleParty(t, ctx, chain, net, saleTestBuyer)
	if _, err := buyer.run(ctx, "join", saleTestRoom, "--invitation-token", "invitation-token"); err != nil {
		t.Fatalf("join: %v", err)
	}
	seller := saleTestKey(t, "silent seller")
	_, data := randomDataset(t, 2*datasale.LeafBytes)
	pkg, saleID, _ := cheatPackage(t, ctx, chain, seller, buyer.address, data, -1, big.NewInt(5e16))
	net.deliver(pkg)

	mined := whenFunded(ctx, chain, saleID, func() error {
		return chain.raw.CallContext(ctx, nil, "anvil_mine", hexutil.EncodeUint64(6))
	})
	result, err := buyer.run(ctx, "buy", saleTestRoom, "--out", filepath.Join(t.TempDir(), "bought.bin"), "--max-price", "0.05")
	if err != nil || result["outcome"] != salestate.OutcomeRefunded {
		t.Fatalf("buy = %v, %v", result, err)
	}
	if err := <-mined; err != nil {
		t.Fatalf("mine: %v", err)
	}
	if state := chain.record(t, ctx, saleID).State; state != escrow.StateRefunded {
		t.Fatalf("escrow state = %s; want refunded", state)
	}
}

func TestASpendAboveTheApprovalThresholdNeedsAPersonAndFundsNothing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	chain := startSaleChain(t, ctx)
	net := newSaleRoomNet(t)
	buyer := newSaleParty(t, ctx, chain, net, saleTestBuyer)
	if _, err := buyer.run(ctx, "join", saleTestRoom, "--invitation-token", "invitation-token"); err != nil {
		t.Fatalf("join: %v", err)
	}
	_, data := randomDataset(t, datasale.LeafBytes)
	pkg, saleID, _ := cheatPackage(t, ctx, chain, saleTestKey(t, "seller"), buyer.address, data, -1, big.NewInt(3e17))
	net.deliver(pkg)

	_, err := buyer.run(ctx, "buy", saleTestRoom, "--out", filepath.Join(t.TempDir(), "bought.bin"), "--max-price", "1")
	if errorKind(err) != "sale_approval_required" {
		t.Fatalf("buy = %v; want sale_approval_required", err)
	}
	if state := chain.record(t, ctx, saleID).State; state != escrow.StateNone {
		t.Fatalf("escrow state = %s; nothing should be funded", state)
	}
	// An agent cannot widen its own allowance either.
	if _, err := buyer.run(ctx, "wallet", "limits", "--approve-above", "1"); errorKind(err) != "sale_approval_required" {
		t.Fatalf("raising a limit = %v; want sale_approval_required", err)
	}
	if limits, err := buyer.run(ctx, "wallet", "limits", "--approve-above", "0.1"); err != nil || limits["changed"] != true {
		t.Fatalf("lowering a limit = %v, %v", limits, err)
	}
}

func TestTheBuyerRefusesAnOfferAboveItsMaximumPrice(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	chain := startSaleChain(t, ctx)
	net := newSaleRoomNet(t)
	buyer := newSaleParty(t, ctx, chain, net, saleTestBuyer)
	if _, err := buyer.run(ctx, "join", saleTestRoom, "--invitation-token", "invitation-token"); err != nil {
		t.Fatalf("join: %v", err)
	}
	_, data := randomDataset(t, datasale.LeafBytes)
	pkg, saleID, _ := cheatPackage(t, ctx, chain, saleTestKey(t, "seller"), buyer.address, data, -1, big.NewInt(5e16))
	net.deliver(pkg)
	if _, err := buyer.run(ctx, "buy", saleTestRoom, "--out", filepath.Join(t.TempDir(), "bought.bin"), "--max-price", "0.04"); errorKind(err) != "sale_terms_rejected" {
		t.Fatalf("buy = %v; want sale_terms_rejected", err)
	}
	if state := chain.record(t, ctx, saleID).State; state != escrow.StateNone {
		t.Fatalf("escrow state = %s; nothing should be funded", state)
	}
}

func TestSaleCommandsAreInHelpAndCompletion(t *testing.T) {
	app := newWebhookApp()
	for _, args := range [][]string{{"help", "sale"}, {"sale", "--help"}} {
		if stdout, stderr, code := runWebhookCLI(app, args...); code != ExitOK || !strings.Contains(stdout, "offer") {
			t.Errorf("%v: exit %d, %q %q", args, code, stdout, stderr)
		}
	}
	if stdout, _, code := runWebhookCLI(app, "help"); code != ExitOK || !strings.Contains(stdout, "sale") {
		t.Errorf("root help: exit %d, %q", code, stdout)
	}
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		script, _ := rawCompletionScript(shell)
		for _, word := range []string{"sale", "offer", "withdraw"} {
			if !strings.Contains(script, word) {
				t.Errorf("%s completion lacks %q", shell, word)
			}
		}
	}
}

// Without a context or variable that overrides them, sale commands use the escrow the build was given.
func TestSaleSettingsDefaultToTheBuildsEscrow(t *testing.T) {
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	for _, name := range []string{"BEAM_SALE_CHAIN_ID", "BEAM_SALE_RPC_URLS", "BEAM_SALE_ESCROW"} {
		t.Setenv(name, "")
	}
	cfg, _, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SaleChainID != config.DefaultSaleChainID || cfg.SaleRPCURLs != config.DefaultSaleRPCURLs ||
		cfg.SaleEscrow != config.DefaultSaleEscrow {
		t.Fatalf("sale settings = %q, %q, %q; want the build's defaults", cfg.SaleChainID, cfg.SaleRPCURLs, cfg.SaleEscrow)
	}
	if !common.IsHexAddress(cfg.SaleEscrow) {
		t.Fatalf("the default escrow %q is not an address", cfg.SaleEscrow)
	}
}

func TestASaleWalletImportsAKeyFromStandardInput(t *testing.T) {
	key := saleTestKey(t, "imported")
	app := newWebhookApp()
	app.stdin = strings.NewReader(hex.EncodeToString(crypto.FromECDSA(key)) + "\n")
	p := &saleParty{app: app, cfg: config.Config{Output: "json"}, paths: config.Paths{Dir: t.TempDir()}}
	result, err := p.run(context.Background(), "wallet", "create", "--import-stdin")
	if err != nil || common.HexToAddress(result["address"].(string)) != crypto.PubkeyToAddress(key.PublicKey) {
		t.Fatalf("wallet create --import-stdin = %v, %v", result, err)
	}
	if strings.Contains(strings.ToLower(p.printed.String()), hex.EncodeToString(crypto.FromECDSA(key))) {
		t.Fatal("the imported key appeared in the output")
	}
	if _, err := p.run(context.Background(), "wallet", "create", "--import-stdin", "extra"); err == nil {
		t.Fatal("a stray argument was accepted")
	}
}

func TestTAOAmountsParseExactly(t *testing.T) {
	for text, want := range map[string]string{"1": "1000000000000000000", "0.05": "50000000000000000", "0.000000000000000001": "1"} {
		got, err := parseTAO(text)
		if err != nil || got.String() != want {
			t.Errorf("parseTAO(%q) = %v, %v; want %s", text, got, err, want)
		}
		if formatTAO(got) != text {
			t.Errorf("formatTAO(%s) = %q; want %q", got, formatTAO(got), text)
		}
	}
	for _, text := range []string{"", "-1", "1e3", "1/3", "0.0000000000000000001", " 1 2"} {
		if _, err := parseTAO(text); err == nil {
			t.Errorf("parseTAO(%q) accepted", text)
		}
	}
	if cents := usdCents(big.NewInt(5e16), big.NewRat(300, 1)); cents != 1500 {
		t.Errorf("0.05 TAO at $300 = %d cents; want 1500", cents)
	}
	for text, want := range map[string]uint64{"1048576": 1 << 20, "500MB": 500e6, "10GiB": 10 << 30, "100 GB": 100e9} {
		if got, err := parseSaleSize(text); err != nil || got != want {
			t.Errorf("parseSaleSize(%q) = %d, %v; want %d", text, got, err, want)
		}
	}
	if _, err := parseSaleSize("101GB"); err == nil {
		t.Error("a size above 100 GB was accepted")
	}
}

// flakyRPC forwards JSON-RPC requests to the chain, but fails the next calls of the methods it is armed for, as an
// endpoint that stops answering would.
type flakyRPC struct {
	mu       sync.Mutex
	failing  map[string]int
	limiting map[string]int // answered with a 429 that asks for a one-second wait
	failed   int
	limited  int
}

func (f *flakyRPC) limit(method string, times int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.limiting == nil {
		f.limiting = map[string]int{}
	}
	f.limiting[method] += times
}

func (f *flakyRPC) arm(method string, times int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failing[method] += times
}

func (f *flakyRPC) start(t *testing.T, chain *saleChain) string {
	target, _ := url.Parse(chain.url)
	proxy := httputil.NewSingleHostReverseProxy(target)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var call struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &call)
		f.mu.Lock()
		fail := f.failing[call.Method] > 0
		if fail {
			f.failing[call.Method]--
			f.failed++
		}
		limit := f.limiting[call.Method] > 0
		if limit {
			f.limiting[call.Method]--
			f.limited++
		}
		f.mu.Unlock()
		if limit {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":"Too many requests from this source.","retry_after_seconds":1}`)
			return
		}
		if fail {
			http.Error(w, "endpoint down", http.StatusBadGateway)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func TestTheSellerWaitsOutAnRPCOutageAndStillReveals(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	chain := startSaleChain(t, ctx)
	net := newSaleRoomNet(t)
	seller := newSaleParty(t, ctx, chain, net, saleTestSeller)
	buyer := newSaleParty(t, ctx, chain, net, saleTestBuyer)
	flaky := &flakyRPC{failing: map[string]int{}}
	seller.cfg.SaleRPCURLs = flaky.start(t, chain)
	if _, err := seller.run(ctx, "room", "create", "--api-key", "beam-key-test"); err != nil {
		t.Fatalf("room create: %v", err)
	}
	binding := filepath.Join(t.TempDir(), "binding.json")
	if _, err := buyer.run(ctx, "join", saleTestRoom, "--invitation-token", "invitation-token", "--out", binding); err != nil {
		t.Fatalf("join: %v", err)
	}
	dataset, data := randomDataset(t, 3*datasale.LeafBytes+5)
	output := filepath.Join(t.TempDir(), "bought.bin")

	// Once the package is delivered, the seller's only endpoint fails its next reads of the gas price and the block
	// height: the seller is then waiting for funding, and must reveal through the outage.
	go func() {
		for len(net.sources()) == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		flaky.arm("eth_gasPrice", 3)
		flaky.arm("eth_blockNumber", 3)
	}()
	var wg sync.WaitGroup
	var offered, bought map[string]any
	var offerErr, buyErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		offered, offerErr = seller.run(ctx, "offer", saleTestRoom, "--file", dataset, "--price", "0.05", "--binding", binding,
			"--reveal-period", "5", "--challenge-period", "5", "--tao-usd", "300")
	}()
	go func() {
		defer wg.Done()
		bought, buyErr = buyer.run(ctx, "buy", saleTestRoom, "--out", output, "--max-price", "0.05", "--wait", "1m")
	}()
	wg.Wait()
	if offerErr != nil || buyErr != nil {
		t.Fatalf("offer: %v\nbuy: %v", offerErr, buyErr)
	}
	if offered["escrow_state"] != "key_released" || bought["phase"] != salestate.PhaseVerified {
		t.Fatalf("offer = %v\nbuy = %v", offered, bought)
	}
	if got, _ := os.ReadFile(output); !bytes.Equal(got, data) {
		t.Fatal("the bought dataset differs from the one sold")
	}
	flaky.mu.Lock()
	failed := flaky.failed
	flaky.mu.Unlock()
	if failed != 6 {
		t.Fatalf("the endpoint failed %d calls; want the 6 it was armed for", failed)
	}
}

func TestAnOfferWaitsOutARateLimitBeforeDelivering(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	chain := startSaleChain(t, ctx)
	net := newSaleRoomNet(t)
	seller := newSaleParty(t, ctx, chain, net, saleTestSeller)
	buyer := newSaleParty(t, ctx, chain, net, saleTestBuyer)
	flaky := &flakyRPC{failing: map[string]int{}}
	seller.cfg.SaleRPCURLs = flaky.start(t, chain)
	if _, err := seller.run(ctx, "room", "create", "--api-key", "beam-key-test"); err != nil {
		t.Fatalf("room create: %v", err)
	}
	binding := filepath.Join(t.TempDir(), "binding.json")
	if _, err := buyer.run(ctx, "join", saleTestRoom, "--invitation-token", "invitation-token", "--out", binding); err != nil {
		t.Fatalf("join: %v", err)
	}
	dataset, _ := randomDataset(t, datasale.LeafBytes)
	// The seller's only endpoint refuses its first checks for too many requests, as a public endpoint does.
	flaky.limit("eth_gasPrice", 2)
	flaky.limit("eth_getBalance", 1)
	var wg sync.WaitGroup
	var offered, bought map[string]any
	var offerErr, buyErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		offered, offerErr = seller.run(ctx, "offer", saleTestRoom, "--file", dataset, "--price", "0.05", "--binding", binding,
			"--reveal-period", "5", "--challenge-period", "5", "--tao-usd", "300")
	}()
	go func() {
		defer wg.Done()
		bought, buyErr = buyer.run(ctx, "buy", saleTestRoom, "--out", filepath.Join(t.TempDir(), "bought.bin"), "--max-price", "0.05", "--wait", "1m")
	}()
	wg.Wait()
	if offerErr != nil || buyErr != nil || offered["escrow_state"] != "key_released" || bought["phase"] != salestate.PhaseVerified {
		t.Fatalf("offer = %v, %v\nbuy = %v, %v", offered, offerErr, bought, buyErr)
	}
	flaky.mu.Lock()
	limited := flaky.limited
	flaky.mu.Unlock()
	if limited != 3 {
		t.Fatalf("the endpoint refused %d calls; want the 3 it was set to", limited)
	}
}

func TestTheSellerPublishesAgainWhenTheBuyersAgentLacksTheKeyYet(t *testing.T) {
	for _, test := range []struct {
		name, reason string
		missed       int
		want         string // the offer's error kind, or "" for a delivered sale
		publications int
	}{
		{name: "key not held yet", reason: keyNotHeldReason, missed: 1, publications: 2},
		{name: "key never held", reason: keyNotHeldReason, missed: maxPublishAttempts, want: "sale_delivery_failed", publications: maxPublishAttempts},
		{name: "another failure", reason: "member_offline", missed: 1, want: "sale_delivery_failed", publications: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			chain := startSaleChain(t, ctx)
			net := newSaleRoomNet(t)
			net.missed, net.missReason = test.missed, test.reason
			seller := newSaleParty(t, ctx, chain, net, saleTestSeller)
			buyer := newSaleParty(t, ctx, chain, net, saleTestBuyer)
			if _, err := seller.run(ctx, "room", "create", "--api-key", "beam-key-test"); err != nil {
				t.Fatalf("room create: %v", err)
			}
			binding := filepath.Join(t.TempDir(), "binding.json")
			if _, err := buyer.run(ctx, "join", saleTestRoom, "--invitation-token", "invitation-token", "--out", binding); err != nil {
				t.Fatalf("join: %v", err)
			}
			dataset, _ := randomDataset(t, datasale.LeafBytes)
			offer := func() (map[string]any, error) {
				return seller.run(ctx, "offer", saleTestRoom, "--file", dataset, "--price", "0.05", "--binding", binding,
					"--reveal-period", "5", "--challenge-period", "5", "--tao-usd", "300")
			}
			if test.want != "" {
				if _, err := offer(); errorKind(err) != test.want {
					t.Fatalf("offer = %v; want %s", err, test.want)
				}
			} else {
				bought := make(chan error, 1)
				go func() {
					_, err := buyer.run(ctx, "buy", saleTestRoom, "--out", filepath.Join(t.TempDir(), "bought.bin"), "--max-price", "0.05", "--wait", "1m")
					bought <- err
				}()
				if result, err := offer(); err != nil || result["escrow_state"] != "key_released" {
					t.Fatalf("offer = %v, %v", result, err)
				}
				if err := <-bought; err != nil {
					t.Fatalf("buy: %v", err)
				}
			}
			net.mu.Lock()
			publications := net.publications
			net.mu.Unlock()
			if publications != test.publications {
				t.Fatalf("the seller published %d times; want %d", publications, test.publications)
			}
		})
	}
}

func TestTheSellerRevealsOnlyOnceTheFundingIsFinal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	chain := startSaleChain(t, ctx)
	net := newSaleRoomNet(t)
	seller := newSaleParty(t, ctx, chain, net, saleTestSeller)
	buyer := newSaleParty(t, ctx, chain, net, saleTestBuyer)
	if _, err := seller.run(ctx, "room", "create", "--api-key", "beam-key-test"); err != nil {
		t.Fatalf("room create: %v", err)
	}
	binding := filepath.Join(t.TempDir(), "binding.json")
	if _, err := buyer.run(ctx, "join", saleTestRoom, "--invitation-token", "invitation-token", "--out", binding); err != nil {
		t.Fatalf("join: %v", err)
	}
	// The chain's finalized block stays where it was before the offer until the test lets it follow the head.
	var held atomic.Bool
	held.Store(true)
	frozen, err := chain.client.BlockNumber(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seller.app.sales.finalized = func(ctx context.Context, backend escrow.Backend) (uint64, error) {
		if held.Load() {
			return frozen, nil
		}
		return backend.BlockNumber(ctx)
	}
	dataset, data := randomDataset(t, 2*datasale.LeafBytes+9)
	output := filepath.Join(t.TempDir(), "bought.bin")
	var wg sync.WaitGroup
	var offered, bought map[string]any
	var offerErr, buyErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		offered, offerErr = seller.run(ctx, "offer", saleTestRoom, "--file", dataset, "--price", "0.05", "--binding", binding,
			"--reveal-period", "50", "--challenge-period", "5", "--tao-usd", "300")
	}()
	go func() {
		defer wg.Done()
		bought, buyErr = buyer.run(ctx, "buy", saleTestRoom, "--out", output, "--max-price", "0.05", "--wait", "1m")
	}()
	for len(net.sources()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	src, err := saleroom.OpenFileSource(net.sources()[0])
	if err != nil {
		t.Fatal(err)
	}
	header, err := saleroom.ReadHeader(src)
	src.Close()
	if err != nil {
		t.Fatal(err)
	}
	saleID, err := datasale.SaleID(chain.domain, header.Offer)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-whenFunded(ctx, chain, saleID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * seller.app.sales.poll)
	if state := chain.record(t, ctx, saleID).State; state != escrow.StateFunded {
		t.Fatalf("the seller acted on funding that was not final: state %d", state)
	}
	held.Store(false)
	wg.Wait()
	if offerErr != nil || buyErr != nil {
		t.Fatalf("offer: %v\nbuy: %v", offerErr, buyErr)
	}
	if offered["escrow_state"] != "key_released" || bought["phase"] != salestate.PhaseVerified {
		t.Fatalf("offer = %v\nbuy = %v", offered, bought)
	}
	if got, _ := os.ReadFile(output); !bytes.Equal(got, data) {
		t.Fatal("the bought dataset differs from the one sold")
	}
}

// A reveal broadcast once has made the key public, so it goes out again even inside the safety margin, where a key
// still secret would be held back.
func TestAnExposedKeyIsRevealedEvenInsideTheMargin(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	chain := startSaleChain(t, ctx)
	net := newSaleRoomNet(t)
	seller := newSaleParty(t, ctx, chain, net, saleTestSeller)
	buyer := newSaleParty(t, ctx, chain, net, saleTestBuyer)
	if _, err := seller.run(ctx, "room", "create", "--api-key", "beam-key-test"); err != nil {
		t.Fatalf("room create: %v", err)
	}
	binding := filepath.Join(t.TempDir(), "binding.json")
	if _, err := buyer.run(ctx, "join", saleTestRoom, "--invitation-token", "invitation-token", "--out", binding); err != nil {
		t.Fatalf("join: %v", err)
	}
	dataset, data := randomDataset(t, 2*datasale.LeafBytes+77)
	output := filepath.Join(t.TempDir(), "bought.bin")

	// The seller stops once the package is delivered, before the buyer funds.
	offerCtx, stopOffer := context.WithCancel(ctx)
	offered := make(chan error, 1)
	go func() {
		_, err := seller.run(offerCtx, "offer", saleTestRoom, "--file", dataset, "--price", "0.05", "--binding", binding,
			"--reveal-period", "10", "--challenge-period", "5", "--tao-usd", "300")
		offered <- err
	}()
	store := salestate.Store{Dir: seller.paths.Dir}
	var sale salestate.Sale
	for sale.Phase != salestate.PhaseOffered {
		time.Sleep(5 * time.Millisecond)
		if sales, _ := store.List(); len(sales) == 1 {
			sale = sales[0]
		}
	}
	stopOffer()
	if err := <-offered; !errors.Is(err, context.Canceled) {
		t.Fatalf("the stopped offer = %v", err)
	}
	var bought map[string]any
	buyErr := make(chan error, 1)
	go func() {
		var err error
		bought, err = buyer.run(ctx, "buy", saleTestRoom, "--out", output, "--max-price", "0.05", "--wait", "1m")
		buyErr <- err
	}()
	if err := <-whenFunded(ctx, chain, sale.SaleID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	// An earlier reveal was broadcast and reverted, then the chain moved inside the margin.
	sale, err := store.Load(sale.SaleID)
	if err != nil {
		t.Fatal(err)
	}
	head, err := chain.client.BlockNumber(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddAttempt(&sale, "reveal.reverted", escrow.Attempt{Hash: common.HexToHash("0xdead"), From: seller.address, Block: head}); err != nil {
		t.Fatal(err)
	}
	deadline := chain.record(t, ctx, sale.SaleID).Deadline
	chain.mine(t, ctx, deadline-1-head)
	if head, _ := chain.client.BlockNumber(ctx); head+seller.app.sales.margin <= deadline {
		t.Fatalf("block %d is not inside the margin of deadline %d", head, deadline)
	}
	resumed, err := seller.run(ctx, "resume", sale.SaleID.Hex())
	if err != nil || resumed["escrow_state"] != "key_released" {
		t.Fatalf("resume = %v, %v", resumed, err)
	}
	if err := <-buyErr; err != nil || bought["phase"] != salestate.PhaseVerified {
		t.Fatalf("buy = %v, %v", bought, err)
	}
	if got, _ := os.ReadFile(output); !bytes.Equal(got, data) {
		t.Fatal("the bought dataset differs from the one sold")
	}
}

// Once a mismatch is found, the challenge carries its own evidence: it succeeds after the agent removed the package.
func TestAChallengeNeedsNothingFromTheInboxOnceDecrypted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	chain := startSaleChain(t, ctx)
	net := newSaleRoomNet(t)
	buyer := newSaleParty(t, ctx, chain, net, saleTestBuyer)
	if _, err := buyer.run(ctx, "join", saleTestRoom, "--invitation-token", "invitation-token"); err != nil {
		t.Fatalf("join: %v", err)
	}
	seller := saleTestKey(t, "cheating seller")
	chain.fund(t, ctx, crypto.PubkeyToAddress(seller.PublicKey))
	_, data := randomDataset(t, 3*datasale.LeafBytes+1)
	pkg, saleID, key := cheatPackage(t, ctx, chain, seller, buyer.address, data, 1, big.NewInt(5e16))
	net.deliver(pkg)

	// The buyer runs out of gas once it funds, so it stops after decrypting, before it can challenge.
	revealed := whenFunded(ctx, chain, saleID, func() error {
		if err := chain.raw.CallContext(ctx, nil, "anvil_setBalance", buyer.address, "0x0"); err != nil {
			return err
		}
		return revealAs(ctx, chain, seller, key, pkg)
	})
	output := filepath.Join(t.TempDir(), "bought.bin")
	if _, err := buyer.run(ctx, "buy", saleTestRoom, "--out", output, "--max-price", "0.05"); errorKind(err) != "sale_wallet_no_gas" {
		t.Fatalf("buy = %v; want sale_wallet_no_gas", err)
	}
	if err := <-revealed; err != nil {
		t.Fatalf("reveal: %v", err)
	}
	if !net.drop(0) {
		t.Fatal("the package was not in the inbox")
	}
	chain.fund(t, ctx, buyer.address)
	result, err := buyer.run(ctx, "resume", saleID.Hex())
	if err != nil || result["outcome"] != salestate.OutcomeRefunded {
		t.Fatalf("resume = %v, %v", result, err)
	}
}

func TestTheBuyerRefusesAPackageNamedForAnotherSale(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	chain := startSaleChain(t, ctx)
	net := newSaleRoomNet(t)
	buyer := newSaleParty(t, ctx, chain, net, saleTestBuyer)
	if _, err := buyer.run(ctx, "join", saleTestRoom, "--invitation-token", "invitation-token"); err != nil {
		t.Fatalf("join: %v", err)
	}
	_, data := randomDataset(t, 2*datasale.LeafBytes)
	pkg, saleID, _ := cheatPackage(t, ctx, chain, saleTestKey(t, "seller"), buyer.address, data, -1, big.NewInt(5e16))
	renamed := filepath.Join(t.TempDir(), saleroom.PackageName(common.HexToHash("0x0be1")))
	if err := os.Rename(pkg, renamed); err != nil {
		t.Fatal(err)
	}
	net.deliver(renamed)
	_, err := buyer.run(ctx, "buy", saleTestRoom, "--out", filepath.Join(t.TempDir(), "bought.bin"), "--max-price", "0.05")
	if errorKind(err) != "sale_package_rejected" {
		t.Fatalf("buy = %v; want sale_package_rejected", err)
	}
	if _, err := os.Stat((salestate.Store{Dir: buyer.paths.Dir}).SaleDir(saleID)); !os.IsNotExist(err) {
		t.Fatalf("a refused package left a sale directory: %v", err)
	}
	if state := chain.record(t, ctx, saleID).State; state != escrow.StateNone {
		t.Fatalf("escrow state = %s; want the sale unfunded", state)
	}
}

func TestDatasetNamesAreShownSafely(t *testing.T) {
	for name, want := range map[string]string{
		"":                       "an unnamed dataset",
		"prices.csv":             `"prices.csv"`,
		"a\x1b[2Jb\r\nc":         `"a\x1b[2Jb\r\nc"`,
		"invoice\u202efdp.exe":   `"invoice\u202efdp.exe"`,
		strings.Repeat("x", 100): `"` + strings.Repeat("x", 64) + `…"`,
	} {
		if got := saleDatasetName(name); got != want {
			t.Errorf("saleDatasetName(%q) = %s; want %s", name, got, want)
		}
	}
}

// deployForgedEscrow deploys the released runtime with one instruction byte changed, which no immutable explains.
func (c *saleChain) deployForgedEscrow(t *testing.T, ctx context.Context) common.Address {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "escrow", "DataSaleEscrow.runtime"))
	if err != nil {
		t.Fatal(err)
	}
	runtime := common.FromHex(strings.TrimSpace(string(raw)))
	runtime[0] ^= 0x01
	// A minimal constructor that copies the code after it into memory and returns it.
	constructor := []byte{0x61, byte(len(runtime) >> 8), byte(len(runtime)), 0x61, 0, 15, 0x60, 0x00, 0x39,
		0x61, byte(len(runtime) >> 8), byte(len(runtime)), 0x60, 0x00, 0xf3}
	deployer := saleTestKey(t, "forger")
	c.fund(t, ctx, crypto.PubkeyToAddress(deployer.PublicKey))
	gasPrice, _ := c.client.SuggestGasPrice(ctx)
	tx, err := types.SignNewTx(deployer, types.NewEIP155Signer(big.NewInt(945)), &types.LegacyTx{GasPrice: gasPrice, Gas: 3_000_000, Data: append(constructor, runtime...)})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.client.SendTransaction(ctx, tx); err != nil {
		t.Fatal(err)
	}
	return c.waitReceipt(t, ctx, tx.Hash()).ContractAddress
}

func TestSaleCommandsRefuseAnEscrowThatIsNotTheReleasedBuild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	chain := startSaleChain(t, ctx)
	net := newSaleRoomNet(t)
	seller := newSaleParty(t, ctx, chain, net, saleTestSeller)
	buyer := newSaleParty(t, ctx, chain, net, saleTestBuyer)
	if _, err := seller.run(ctx, "room", "create", "--api-key", "beam-key-test"); err != nil {
		t.Fatalf("room create: %v", err)
	}
	binding := filepath.Join(t.TempDir(), "binding.json")
	if _, err := buyer.run(ctx, "join", saleTestRoom, "--invitation-token", "invitation-token", "--out", binding); err != nil {
		t.Fatalf("join: %v", err)
	}
	dataset, _ := randomDataset(t, 2*datasale.LeafBytes)

	forged := chain.deployForgedEscrow(t, ctx)
	for name, escrowAddress := range map[string]common.Address{"a modified build": forged, "an account without code": buyer.address} {
		seller.cfg.SaleEscrow, buyer.cfg.SaleEscrow = escrowAddress.Hex(), escrowAddress.Hex()
		if _, err := seller.run(ctx, "offer", saleTestRoom, "--file", dataset, "--price", "0.05", "--binding", binding, "--tao-usd", "300"); errorKind(err) != "sale_escrow_unverified" {
			t.Fatalf("%s: offer = %v; want sale_escrow_unverified", name, err)
		}
		if _, err := buyer.run(ctx, "buy", saleTestRoom, "--out", filepath.Join(t.TempDir(), "bought.bin"), "--max-price", "0.05", "--wait", "1s"); errorKind(err) != "sale_escrow_unverified" {
			t.Fatalf("%s: buy = %v; want sale_escrow_unverified", name, err)
		}
		if _, err := buyer.run(ctx, "join", saleTestRoom, "--invitation-token", "invitation-token", "--out", filepath.Join(t.TempDir(), "binding.json")); errorKind(err) != "sale_escrow_unverified" {
			t.Fatalf("%s: join = %v; want sale_escrow_unverified", name, err)
		}
		if status, err := seller.run(ctx, "status"); err != nil || status["sales"] == nil {
			t.Fatalf("%s: status = %v, %v; want the records without escrow states", name, status, err)
		}
	}
	if len(net.sources()) != 0 {
		t.Fatal("a sale was delivered through an escrow that is not the released build")
	}
}
