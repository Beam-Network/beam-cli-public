package escrow

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"testing"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// fakeChain is a scriptable chain and mempool: each BlockNumber call mines a block, a transaction is mined when its
// nonce is next for its sender and its gas price clears the floor, and faults can be injected per test.
type fakeChain struct {
	mu         sync.Mutex
	chainID    *big.Int
	head       uint64
	nonces     map[common.Address]uint64
	pool       map[common.Address]map[uint64]*types.Transaction
	receipts   map[common.Hash]*types.Receipt
	floor      map[common.Address]*big.Int // per sender; transactions below it are never mined
	suggested  *big.Int
	revert     func(tx *types.Transaction) bool
	dropSends  int // the next sends are acknowledged but the transactions are lost
	down       bool
	balance    *big.Int // every account's balance; nil is 1 TAO
	calls      []ethereum.CallMsg
	sends      []*types.Transaction
	beforeMine func(head uint64)
}

func newFakeChain() *fakeChain {
	return &fakeChain{
		chainID:   big.NewInt(945),
		head:      100,
		nonces:    map[common.Address]uint64{},
		pool:      map[common.Address]map[uint64]*types.Transaction{},
		receipts:  map[common.Hash]*types.Receipt{},
		floor:     map[common.Address]*big.Int{},
		suggested: big.NewInt(10_000_000_000),
	}
}

var errTransport = errors.New("dial tcp 192.0.2.1:443: connect: connection refused")

func (c *fakeChain) sender(tx *types.Transaction) common.Address {
	from, err := types.Sender(types.NewEIP155Signer(c.chainID), tx)
	if err != nil {
		panic(err)
	}
	return from
}

// addForeign puts a pending transaction the client did not send at from's next nonce, with a gas price that is never
// mined, so from is stuck behind it.
func (c *fakeChain) addForeign(from common.Address) {
	c.mu.Lock()
	defer c.mu.Unlock()
	nonce := c.nonces[from]
	if c.pool[from] == nil {
		c.pool[from] = map[uint64]*types.Transaction{}
	}
	c.pool[from][nonce] = types.NewTx(&types.LegacyTx{Nonce: nonce, GasPrice: big.NewInt(1), Gas: 21000, To: &from})
	c.floor[from] = big.NewInt(2)
}

func (c *fakeChain) mine() {
	if c.beforeMine != nil {
		c.beforeMine(c.head)
	}
	for from, txs := range c.pool {
		for {
			tx, ok := txs[c.nonces[from]]
			if !ok {
				break
			}
			if floor := c.floor[from]; floor != nil && tx.GasPrice().Cmp(floor) < 0 {
				break
			}
			status := types.ReceiptStatusSuccessful
			if c.revert != nil && c.revert(tx) {
				status = types.ReceiptStatusFailed
			}
			c.receipts[tx.Hash()] = &types.Receipt{Status: status, TxHash: tx.Hash(), BlockNumber: new(big.Int).SetUint64(c.head)}
			delete(txs, c.nonces[from])
			c.nonces[from]++
		}
	}
}

func (c *fakeChain) fail() error {
	if c.down {
		return errTransport
	}
	return nil
}

func (c *fakeChain) ChainID(context.Context) (*big.Int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.chainID, c.fail()
}

func (c *fakeChain) HeaderByNumber(context.Context, *big.Int) (*types.Header, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.fail(); err != nil {
		return nil, err
	}
	return &types.Header{Number: new(big.Int).SetUint64(c.head)}, nil
}

func (c *fakeChain) BlockNumber(context.Context) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.fail(); err != nil {
		return 0, err
	}
	c.head++
	c.mine()
	return c.head, nil
}

func (c *fakeChain) BalanceAt(context.Context, common.Address, *big.Int) (*big.Int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.balance != nil {
		return new(big.Int).Set(c.balance), c.fail()
	}
	return big.NewInt(1e18), c.fail()
}

func (c *fakeChain) CodeAt(context.Context, common.Address, *big.Int) ([]byte, error) {
	return bytes.Clone(releasedRuntime), c.fail()
}

func (c *fakeChain) NonceAt(_ context.Context, a common.Address, _ *big.Int) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nonces[a], c.fail()
}

func (c *fakeChain) PendingNonceAt(_ context.Context, a common.Address) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.nonces[a]
	for {
		if _, ok := c.pool[a][n]; !ok {
			break
		}
		n++
	}
	return n, c.fail()
}

func (c *fakeChain) SuggestGasPrice(context.Context) (*big.Int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return new(big.Int).Set(c.suggested), c.fail()
}

func (c *fakeChain) CallContract(_ context.Context, msg ethereum.CallMsg, _ *big.Int) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, msg)
	return nil, c.fail()
}

func (c *fakeChain) SendTransaction(_ context.Context, tx *types.Transaction) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.fail(); err != nil {
		return err
	}
	c.sends = append(c.sends, tx)
	from := c.sender(tx)
	if tx.Nonce() < c.nonces[from] {
		return errors.New("nonce too low")
	}
	if c.pool[from] == nil {
		c.pool[from] = map[uint64]*types.Transaction{}
	}
	if old, ok := c.pool[from][tx.Nonce()]; ok {
		if old.Hash() == tx.Hash() {
			return errors.New("already known")
		}
		min := new(big.Int).Div(new(big.Int).Mul(old.GasPrice(), big.NewInt(110)), big.NewInt(100))
		if tx.GasPrice().Cmp(min) < 0 {
			return errors.New("replacement transaction underpriced")
		}
	}
	if c.dropSends > 0 {
		c.dropSends--
		return nil
	}
	c.pool[from][tx.Nonce()] = tx
	return nil
}

func (c *fakeChain) TransactionReceipt(_ context.Context, h common.Hash) (*types.Receipt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.fail(); err != nil {
		return nil, err
	}
	if r, ok := c.receipts[h]; ok {
		return r, nil
	}
	return nil, ethereum.NotFound
}

func (c *fakeChain) FilterLogs(context.Context, ethereum.FilterQuery) ([]types.Log, error) {
	return nil, c.fail()
}

func testAccount(t *testing.T, seed string) Account {
	t.Helper()
	key, err := crypto.ToECDSA(crypto.Keccak256([]byte(seed)))
	if err != nil {
		t.Fatal(err)
	}
	return Account{Key: key}
}

func testSender(t *testing.T, backend Backend, backups ...Account) *Sender {
	return &Sender{
		Backend:     backend,
		ChainID:     big.NewInt(945),
		Primary:     testAccount(t, "primary"),
		Backups:     backups,
		StallBlocks: 2,
		BumpPercent: 25,
		Sleep:       func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
	}
}

func testRequest() Request {
	return Request{Label: "test", To: common.HexToAddress("0x00000000000000000000000000000000000E5C40"), Data: []byte{1, 2, 3}, GasLimit: 150_000}
}

func TestSendIncludesATransaction(t *testing.T) {
	chain := newFakeChain()
	out, err := testSender(t, chain).Send(context.Background(), testRequest())
	if err != nil || out.Receipt == nil || len(out.Attempts) != 1 {
		t.Fatalf("Send = %+v, %v", out, err)
	}
}

func TestRPCFailuresFailOverToTheNextEndpoint(t *testing.T) {
	down, up := newFakeChain(), newFakeChain()
	down.down = true
	out, err := testSender(t, NewFailover(time.Second, down, up)).Send(context.Background(), testRequest())
	if err != nil || out.Receipt == nil {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	if len(up.sends) != 1 {
		t.Fatalf("the reachable endpoint got %d transactions", len(up.sends))
	}
}

func TestAnEndpointThatAnswersAnErrorIsNotSkipped(t *testing.T) {
	reverting := &revertingCall{fakeChain: newFakeChain()}
	other := newFakeChain()
	_, err := testSender(t, NewFailover(time.Second, reverting, other)).Send(context.Background(), testRequest())
	if !errors.Is(err, ErrWouldRevert) || len(other.calls) != 0 {
		t.Fatalf("err = %v, other endpoint calls = %d; a revert answer must not fail over", err, len(other.calls))
	}
}

type revertingCall struct{ *fakeChain }

func (r *revertingCall) CallContract(context.Context, ethereum.CallMsg, *big.Int) ([]byte, error) {
	return nil, errors.New("execution reverted: WrongState(1)")
}

func TestAStalledTransactionIsReplacedWithAHigherFeeUntilIncluded(t *testing.T) {
	chain := newFakeChain()
	s := testSender(t, chain)
	chain.floor[s.Primary.Address()] = big.NewInt(20_000_000_000) // twice the suggested price
	out, err := s.Send(context.Background(), testRequest())
	if err != nil || out.Receipt == nil {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	if len(out.Attempts) < 3 {
		t.Fatalf("expected fee bumps, got %d attempts", len(out.Attempts))
	}
	for _, a := range out.Attempts {
		if a.Nonce != out.Attempts[0].Nonce {
			t.Fatalf("a bump changed the nonce: %+v", out.Attempts)
		}
	}
}

func TestADroppedTransactionIsSentAgain(t *testing.T) {
	chain := newFakeChain()
	chain.dropSends = 1
	out, err := testSender(t, chain).Send(context.Background(), testRequest())
	if err != nil || out.Receipt == nil || len(out.Attempts) < 2 {
		t.Fatalf("Send = %+v, %v", out, err)
	}
}

func TestTheFeeCapStopsBumpingAndTheDeadlineEndsTheWait(t *testing.T) {
	chain := newFakeChain()
	s := testSender(t, chain)
	s.MaxGasPrice = big.NewInt(12_000_000_000)
	chain.floor[s.Primary.Address()] = big.NewInt(50_000_000_000)
	req := testRequest()
	req.Deadline = chain.head + 20
	req.Margin = 5
	out, err := s.Send(context.Background(), req)
	if !errors.Is(err, ErrDeadlinePassed) {
		t.Fatalf("err = %v; want ErrDeadlinePassed", err)
	}
	for _, a := range out.Attempts {
		if a.GasPrice.Cmp(s.MaxGasPrice) > 0 {
			t.Fatalf("an attempt paid above the cap: %s", a.GasPrice)
		}
	}
}

func TestAFailedTransactionIsReportedAsReverted(t *testing.T) {
	chain := newFakeChain()
	chain.revert = func(*types.Transaction) bool { return true }
	out, err := testSender(t, chain).Send(context.Background(), testRequest())
	if !errors.Is(err, ErrReverted) || out.Receipt != nil {
		t.Fatalf("Send = %+v, %v; want ErrReverted", out, err)
	}
}

func TestAnotherTransactionTakingTheNonceIsDetectedAndTheCallSentAgain(t *testing.T) {
	chain := newFakeChain()
	s := testSender(t, chain)
	stolen := false
	chain.floor[s.Primary.Address()] = big.NewInt(1 << 62) // hold our first attempt in the pool
	chain.beforeMine = func(head uint64) {
		if stolen {
			return
		}
		// Another process used the account: its transaction takes the nonce our attempt waits on.
		from := s.Primary.Address()
		if _, ok := chain.pool[from][chain.nonces[from]]; ok {
			delete(chain.pool[from], chain.nonces[from])
			chain.nonces[from]++
			stolen = true
			delete(chain.floor, from)
		}
	}
	out, err := s.Send(context.Background(), testRequest())
	if err != nil || out.Receipt == nil {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	last := out.Attempts[len(out.Attempts)-1]
	if last.Nonce == out.Attempts[0].Nonce {
		t.Fatalf("the call was not sent again on a fresh nonce: %+v", out.Attempts)
	}
}

func TestAnAccountStuckBehindAForeignTransactionRefusesOrUsesABackup(t *testing.T) {
	chain := newFakeChain()
	s := testSender(t, chain)
	chain.addForeign(s.Primary.Address())
	if _, err := s.Send(context.Background(), testRequest()); !errors.Is(err, ErrAccountBusy) {
		t.Fatalf("err = %v; want ErrAccountBusy for a call only the primary may make", err)
	}

	backup := testAccount(t, "backup")
	s = testSender(t, chain, backup)
	req := testRequest()
	req.AnySender = true
	out, err := s.Send(context.Background(), req)
	if err != nil || out.Receipt == nil || out.Attempts[0].From != backup.Address() {
		t.Fatalf("Send = %+v, %v; want inclusion from the backup", out, err)
	}
}

func TestNothingIsBroadcastTooCloseToTheDeadline(t *testing.T) {
	chain := newFakeChain()
	req := testRequest()
	req.Secret = true
	req.Deadline = chain.head + 50
	req.Margin = 100
	out, err := testSender(t, chain).Send(context.Background(), req)
	if !errors.Is(err, ErrTooLate) || out.Exposed || len(chain.sends) != 0 {
		t.Fatalf("Send = %+v, %v, %d sends; nothing may be sent", out, err, len(chain.sends))
	}
}

func TestASecretIsNeverSimulatedThroughAnEndpoint(t *testing.T) {
	chain := newFakeChain()
	req := testRequest()
	req.Secret = true
	if _, err := testSender(t, chain).Send(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	for _, call := range chain.calls {
		if bytes.Equal(call.Data, req.Data) {
			t.Fatal("the secret calldata was sent to an endpoint for simulation")
		}
	}
}

// TestAKeyExposedBeforeInclusionIsDrivenToInclusion is F-2: once a reveal is broadcast the key is public, so the
// sender must get some reveal included before the deadline, even when the primary account stops being mined.
func TestAKeyExposedBeforeInclusionIsDrivenToInclusion(t *testing.T) {
	chain := newFakeChain()
	backup := testAccount(t, "backup")
	s := testSender(t, chain, backup)
	s.MaxGasPrice = big.NewInt(15_000_000_000)
	chain.floor[s.Primary.Address()] = big.NewInt(1 << 62) // the primary's reveal never clears
	req := testRequest()
	req.Secret = true
	req.AnySender = true
	req.Deadline = chain.head + 40
	req.Margin = 20
	var recorded []Attempt
	req.OnAttempt = func(a Attempt) { recorded = append(recorded, a) }
	out, err := s.Send(context.Background(), req)
	if err != nil || out.Receipt == nil || !out.Exposed {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	if out.Receipt.BlockNumber.Uint64() > req.Deadline {
		t.Fatalf("included at block %s, after the deadline %d", out.Receipt.BlockNumber, req.Deadline)
	}
	var fromBackup bool
	for _, a := range out.Attempts {
		fromBackup = fromBackup || a.From == backup.Address()
	}
	if !fromBackup || len(recorded) != len(out.Attempts) {
		t.Fatalf("the backup was not used, or attempts were not all recorded: %+v", out.Attempts)
	}
}

func TestAnInterruptedSendResumesWithoutANewTransaction(t *testing.T) {
	chain := newFakeChain()
	s := testSender(t, chain)
	chain.floor[s.Primary.Address()] = big.NewInt(1 << 62)
	ctx, cancel := context.WithCancel(context.Background())
	req := testRequest()
	req.OnAttempt = func(Attempt) { cancel() }
	out, err := s.Send(ctx, req)
	if !errors.Is(err, context.Canceled) || len(out.Attempts) != 1 {
		t.Fatalf("Send = %+v, %v; want one recorded attempt and cancellation", out, err)
	}
	delete(chain.floor, s.Primary.Address())
	sendsBefore := len(chain.sends)
	resumed, err := s.Resume(context.Background(), testRequest(), out.Attempts)
	if err != nil || resumed.Receipt == nil || resumed.Receipt.TxHash != out.Attempts[0].Hash {
		t.Fatalf("Resume = %+v, %v; want the original attempt included", resumed, err)
	}
	for _, tx := range chain.sends[sendsBefore:] {
		if tx.Hash() != out.Attempts[0].Hash {
			t.Fatalf("resume sent a new transaction %s", tx.Hash())
		}
	}
}

func TestRequestErrorsAreToldApartFromTransportFailures(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{errTransport, false},
		{context.DeadlineExceeded, false},
		{ethereum.NotFound, true},
		{errors.New("execution reverted: LeafMatches()"), true},
		{errors.New("nonce too low"), true},
		{errors.New("replacement transaction underpriced"), true},
		{fmt.Errorf("wrapped: %w", errors.New("insufficient funds for gas * price + value")), true},
		// Failures inside the EVM other than a revert, as each node words them.
		{errors.New("evm error: StackUnderflow"), true},
		{errors.New("evm error: InvalidCode(Opcode(254))"), true},
		{errors.New("EVM error InvalidJump"), true},
		{errors.New("Out of gas: gas required exceeds allowance: 100000"), true},
		{errors.New("gas required exceeds allowance (100000)"), true},
		{errors.New("invalid opcode: INVALID"), true},
		{errors.New("stack underflow (0 <=> 2)"), true},
		{errors.New("invalid jump destination"), true},
		{errors.New("dial tcp 127.0.0.1:8545: connect: connection refused"), false},
	} {
		if got := IsRequestError(tc.err); got != tc.want {
			t.Errorf("IsRequestError(%v) = %v; want %v", tc.err, got, tc.want)
		}
	}
}
