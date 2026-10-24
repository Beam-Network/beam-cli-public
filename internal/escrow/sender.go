package escrow

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

var (
	// ErrTooLate means the request's deadline is too close to broadcast. Nothing was sent, so a secret stayed secret.
	ErrTooLate = errors.New("too close to the deadline to broadcast")
	// ErrWouldRevert means simulating the call showed it would revert. Nothing was sent.
	ErrWouldRevert = errors.New("the call would revert")
	// ErrReverted means the transaction was included and reverted.
	ErrReverted = errors.New("the transaction was included and reverted")
	// ErrDeadlinePassed means the deadline passed before any attempt was included.
	ErrDeadlinePassed = errors.New("the deadline passed before the transaction was included")
	// ErrAccountBusy means the sending account has pending transactions this sender did not send, so a new one would
	// wait behind them.
	ErrAccountBusy = errors.New("the account has pending transactions that were not sent by this client")
)

// Account is a key that pays for and signs transactions.
type Account struct {
	Key *ecdsa.PrivateKey
}

// Address is the account's address.
func (a Account) Address() common.Address { return crypto.PubkeyToAddress(a.Key.PublicKey) }

// Request is one contract call to get included.
type Request struct {
	Label    string
	To       common.Address
	Data     []byte
	Value    *big.Int
	GasLimit uint64
	// Deadline is the last block in which the call can still succeed; 0 means none. Margin is how many blocks before
	// the deadline the first broadcast must happen. Past that point the sender refuses to start, but keeps working on
	// a transaction it already sent.
	Deadline uint64
	Margin   uint64
	// Secret marks calldata that reveals a secret, such as a sale key. It is never simulated through an RPC
	// endpoint, so the secret reaches the network only in the broadcast itself.
	Secret bool
	// AnySender allows a backup account to send the call, for functions anyone may call. The sender switches to a
	// backup when the primary account is stuck behind a transaction it did not send, or when the deadline is near
	// and nothing has been included.
	AnySender bool
	// OnAttempt is called after each broadcast, before waiting, so the caller can record the attempt and resume
	// after an interruption.
	OnAttempt func(Attempt)
}

// Attempt is one signed transaction that was broadcast.
type Attempt struct {
	Hash     common.Hash    `json:"hash"`
	From     common.Address `json:"from"`
	Nonce    uint64         `json:"nonce"`
	GasPrice *big.Int       `json:"gasPrice"`
	Block    uint64         `json:"block"`
	Raw      []byte         `json:"raw"`
}

// Outcome is what happened to a request. Exposed is true once any broadcast was attempted: for a secret request it
// means the secret may be public, whether or not a transaction was included.
type Outcome struct {
	Receipt  *types.Receipt
	Attempts []Attempt
	Exposed  bool
}

// Sender gets requests included, from a primary account and optional backups.
type Sender struct {
	Backend Backend
	ChainID *big.Int
	Primary Account
	Backups []Account
	// Poll is the wait between checks; StallBlocks is how many blocks an attempt may stay pending before the fee is
	// raised by BumpPercent (at least 10, the minimum a replacement needs). MaxGasPrice caps what one gas may cost.
	Poll        time.Duration
	StallBlocks uint64
	BumpPercent int64
	MaxGasPrice *big.Int
	// Sleep waits for d or until ctx ends; tests replace it.
	Sleep func(ctx context.Context, d time.Duration) error
}

func (s *Sender) sleep(ctx context.Context) error {
	if s.Sleep != nil {
		return s.Sleep(ctx, s.Poll)
	}
	t := time.NewTimer(s.Poll)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (s *Sender) bumpPercent() int64 {
	if s.BumpPercent < 10 {
		return 25
	}
	return s.BumpPercent
}

func (s *Sender) stallBlocks() uint64 {
	if s.StallBlocks == 0 {
		return 2
	}
	return s.StallBlocks
}

// Send gets req included and returns its receipt. On any error the outcome still lists every attempt broadcast.
func (s *Sender) Send(ctx context.Context, req Request) (Outcome, error) {
	head, err := s.Backend.BlockNumber(ctx)
	if err != nil {
		return Outcome{}, err
	}
	if req.Deadline > 0 && head+req.Margin > req.Deadline {
		return Outcome{}, fmt.Errorf("%w: block %d, deadline %d, margin %d", ErrTooLate, head, req.Deadline, req.Margin)
	}
	if !req.Secret {
		msg := ethereum.CallMsg{From: s.Primary.Address(), To: &req.To, Data: req.Data, Value: req.Value, Gas: req.GasLimit}
		if _, err := s.Backend.CallContract(ctx, msg, nil); err != nil {
			if IsRequestError(err) {
				return Outcome{}, fmt.Errorf("%w: %v", ErrWouldRevert, err)
			}
			return Outcome{}, err
		}
	}
	account, err := s.pickAccount(ctx, req)
	if err != nil {
		return Outcome{}, err
	}
	out := &Outcome{}
	price, err := s.startPrice(ctx)
	if err != nil {
		return *out, err
	}
	if err := s.broadcastNew(ctx, req, out, account, price); err != nil {
		return *out, err
	}
	return s.wait(ctx, req, out)
}

// Resume keeps working on attempts an interrupted Send recorded, without sending a new transaction unless one is
// needed to get the request included.
func (s *Sender) Resume(ctx context.Context, req Request, attempts []Attempt) (Outcome, error) {
	out := &Outcome{Attempts: append([]Attempt(nil), attempts...), Exposed: len(attempts) > 0}
	for _, a := range attempts {
		var tx types.Transaction
		if err := tx.UnmarshalBinary(a.Raw); err == nil {
			_ = s.Backend.SendTransaction(ctx, &tx)
		}
	}
	return s.wait(ctx, req, out)
}

// pickAccount returns the primary account, or a backup when the primary has pending transactions it did not send
// and the request allows any sender.
func (s *Sender) pickAccount(ctx context.Context, req Request) (Account, error) {
	busy, err := s.hasForeignPending(ctx, s.Primary)
	if err != nil {
		return Account{}, err
	}
	if !busy {
		return s.Primary, nil
	}
	if req.AnySender {
		for _, b := range s.Backups {
			backupBusy, err := s.hasForeignPending(ctx, b)
			if err == nil && !backupBusy {
				return b, nil
			}
		}
	}
	return Account{}, fmt.Errorf("%w (%s)", ErrAccountBusy, s.Primary.Address())
}

func (s *Sender) hasForeignPending(ctx context.Context, a Account) (bool, error) {
	latest, err := s.Backend.NonceAt(ctx, a.Address(), nil)
	if err != nil {
		return false, err
	}
	pending, err := s.Backend.PendingNonceAt(ctx, a.Address())
	if err != nil {
		return false, err
	}
	return pending > latest, nil
}

func (s *Sender) startPrice(ctx context.Context) (*big.Int, error) {
	price, err := s.Backend.SuggestGasPrice(ctx)
	if err != nil {
		return nil, err
	}
	return s.capped(price), nil
}

func (s *Sender) capped(price *big.Int) *big.Int {
	if s.MaxGasPrice != nil && price.Cmp(s.MaxGasPrice) > 0 {
		return new(big.Int).Set(s.MaxGasPrice)
	}
	return price
}

func (s *Sender) bumped(price *big.Int) *big.Int {
	next := new(big.Int).Mul(price, big.NewInt(100+s.bumpPercent()))
	next.Div(next, big.NewInt(100))
	if next.Cmp(price) <= 0 {
		next.Add(price, big.NewInt(1))
	}
	return s.capped(next)
}

// broadcastNew sends the request from account at the account's next nonce, retrying with a fresh nonce if one was
// taken in the meantime.
func (s *Sender) broadcastNew(ctx context.Context, req Request, out *Outcome, account Account, price *big.Int) error {
	for try := 0; try < 3; try++ {
		nonce, err := s.Backend.PendingNonceAt(ctx, account.Address())
		if err != nil {
			return err
		}
		err = s.broadcast(ctx, req, out, account, nonce, price)
		if err == nil || !IsNonceTooLow(err) {
			return err
		}
	}
	return errors.New("the account's nonce kept moving while sending")
}

func (s *Sender) broadcast(ctx context.Context, req Request, out *Outcome, account Account, nonce uint64, price *big.Int) error {
	value := req.Value
	if value == nil {
		value = new(big.Int)
	}
	tx, err := types.SignNewTx(account.Key, types.NewEIP155Signer(s.ChainID), &types.LegacyTx{
		Nonce: nonce, GasPrice: price, Gas: req.GasLimit, To: &req.To, Value: value, Data: req.Data,
	})
	if err != nil {
		return err
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		return err
	}
	head, err := s.Backend.BlockNumber(ctx)
	if err != nil {
		return err
	}
	out.Exposed = true
	if err := s.Backend.SendTransaction(ctx, tx); err != nil && !IsKnownTransaction(err) {
		if !errors.Is(err, ErrUnreachable) {
			return err
		}
		// No endpoint answered. The transaction may still have reached one, so it is recorded and watched like any
		// other attempt, and re-broadcast if it stalls.
	}
	attempt := Attempt{Hash: tx.Hash(), From: account.Address(), Nonce: nonce, GasPrice: price, Block: head, Raw: raw}
	out.Attempts = append(out.Attempts, attempt)
	if req.OnAttempt != nil {
		req.OnAttempt(attempt)
	}
	return nil
}

// wait watches every attempt until one is included, raising the fee of stalled attempts, re-sending on a fresh nonce
// when another transaction took the attempt's nonce, and switching to a backup account when allowed and needed. Once
// an attempt is out, nothing but the context, an inclusion, every attempt reverting or the deadline passing ends the
// watch: a failed read or a replacement that could not be sent is tried again on the next round.
func (s *Sender) wait(ctx context.Context, req Request, out *Outcome) (Outcome, error) {
	usedBackup := len(out.Attempts) > 0 && out.Attempts[len(out.Attempts)-1].From != s.Primary.Address()
	for {
		if err := s.sleep(ctx); err != nil {
			return *out, err
		}
		head, err := s.Backend.BlockNumber(ctx)
		if err != nil {
			continue
		}
		reverted, err := s.checkReceipts(ctx, out)
		if err != nil {
			continue
		}
		if out.Receipt != nil {
			return *out, nil
		}
		pendingAccounts := s.pendingAccounts(out, reverted)
		if len(pendingAccounts) == 0 && reverted != nil {
			return *out, fmt.Errorf("%w: %s", ErrReverted, reverted.TxHash)
		}
		if req.Deadline > 0 && head > req.Deadline {
			return *out, fmt.Errorf("%w (block %d, deadline %d)", ErrDeadlinePassed, head, req.Deadline)
		}
		for _, latest := range pendingAccounts {
			_ = s.advance(ctx, req, out, latest, head)
		}
		if req.AnySender && !usedBackup && len(s.Backups) > 0 && s.needsBackup(ctx, req, out, head) {
			for _, b := range s.Backups {
				busy, err := s.hasForeignPending(ctx, b)
				if err != nil || busy {
					continue
				}
				price := s.affordable(ctx, b, req, s.bumped(out.Attempts[len(out.Attempts)-1].GasPrice))
				if err := s.broadcastNew(ctx, req, out, b, price); err == nil {
					usedBackup = true
					break
				}
			}
		}
	}
}

// checkReceipts sets out.Receipt when an attempt succeeded and returns a reverted receipt if any attempt reverted.
func (s *Sender) checkReceipts(ctx context.Context, out *Outcome) (*types.Receipt, error) {
	var reverted *types.Receipt
	for _, a := range out.Attempts {
		receipt, err := s.Backend.TransactionReceipt(ctx, a.Hash)
		if errors.Is(err, ethereum.NotFound) || errors.Is(err, ErrUnreachable) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if receipt.Status == types.ReceiptStatusSuccessful {
			out.Receipt = receipt
			return nil, nil
		}
		reverted = receipt
	}
	return reverted, nil
}

// pendingAccounts returns, for each account with an attempt that may still be included, that account's latest
// attempt. A reverted attempt used its account's nonce, so that account's other attempts at the same nonce are dead.
func (s *Sender) pendingAccounts(out *Outcome, reverted *types.Receipt) []Attempt {
	latest := map[common.Address]Attempt{}
	var order []common.Address
	for _, a := range out.Attempts {
		if _, seen := latest[a.From]; !seen {
			order = append(order, a.From)
		}
		latest[a.From] = a
	}
	if reverted != nil {
		for _, a := range out.Attempts {
			if a.Hash == reverted.TxHash && latest[a.From].Nonce == a.Nonce {
				delete(latest, a.From)
			}
		}
	}
	var result []Attempt
	for _, addr := range order {
		if a, ok := latest[addr]; ok {
			result = append(result, a)
		}
	}
	return result
}

// advance acts on an account's latest attempt: nothing while it is fresh, a fee bump once it stalls, or a fresh
// nonce if another transaction took its nonce.
func (s *Sender) advance(ctx context.Context, req Request, out *Outcome, latest Attempt, head uint64) error {
	account := s.accountFor(latest.From)
	if account == nil {
		return nil
	}
	confirmed, err := s.Backend.NonceAt(ctx, latest.From, nil)
	if err != nil {
		if errors.Is(err, ErrUnreachable) {
			return nil
		}
		return err
	}
	if confirmed > latest.Nonce {
		// The nonce was used. A receipt for one of our attempts would have ended the wait, so give the chain one more
		// stall period to show it, then send again on a fresh nonce.
		if head < latest.Block+s.stallBlocks() {
			return nil
		}
		return s.broadcastNew(ctx, req, out, *account, s.affordable(ctx, *account, req, s.bumped(latest.GasPrice)))
	}
	if head < latest.Block+s.stallBlocks() {
		return nil
	}
	price := s.affordable(ctx, *account, req, s.bumped(latest.GasPrice))
	if price.Cmp(latest.GasPrice) > 0 {
		err = s.broadcast(ctx, req, out, *account, latest.Nonce, price)
		if err == nil || IsNonceTooLow(err) {
			return nil
		}
	}
	// At the fee cap, at what the account can pay, or with a replacement refused: the attempt already out stays the
	// one to include, and is sent again in case it was dropped.
	var tx types.Transaction
	if decodeErr := tx.UnmarshalBinary(latest.Raw); decodeErr == nil {
		_ = s.Backend.SendTransaction(ctx, &tx)
	}
	return err
}

// affordable lowers price to what account can pay for the request's gas and value, when its balance can be read.
func (s *Sender) affordable(ctx context.Context, account Account, req Request, price *big.Int) *big.Int {
	balance, err := s.Backend.BalanceAt(ctx, account.Address(), nil)
	if err != nil || req.GasLimit == 0 {
		return price
	}
	spendable := new(big.Int).Set(balance)
	if req.Value != nil {
		spendable.Sub(spendable, req.Value)
	}
	most := new(big.Int).Quo(spendable, new(big.Int).SetUint64(req.GasLimit))
	if most.Cmp(price) < 0 {
		return most
	}
	return price
}

func (s *Sender) accountFor(addr common.Address) *Account {
	if s.Primary.Address() == addr {
		return &s.Primary
	}
	for i := range s.Backups {
		if s.Backups[i].Address() == addr {
			return &s.Backups[i]
		}
	}
	return nil
}

// needsBackup reports whether a backup account should send the request too: the primary is stuck behind a nonce it
// did not use, or the deadline is within half the margin and nothing has been included.
func (s *Sender) needsBackup(ctx context.Context, req Request, out *Outcome, head uint64) bool {
	last := out.Attempts[len(out.Attempts)-1]
	if req.Deadline > 0 && head+req.Margin/2 >= req.Deadline {
		return true
	}
	confirmed, err := s.Backend.NonceAt(ctx, last.From, nil)
	if err != nil {
		return false
	}
	return confirmed < last.Nonce && head >= last.Block+s.stallBlocks()
}
