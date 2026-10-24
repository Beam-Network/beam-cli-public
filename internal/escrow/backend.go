package escrow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// Backend is the part of an Ethereum JSON-RPC client the escrow uses. *ethclient.Client implements it.
type Backend interface {
	ChainID(ctx context.Context) (*big.Int, error)
	BlockNumber(ctx context.Context) (uint64, error)
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
	BalanceAt(ctx context.Context, account common.Address, block *big.Int) (*big.Int, error)
	CodeAt(ctx context.Context, account common.Address, block *big.Int) ([]byte, error)
	NonceAt(ctx context.Context, account common.Address, block *big.Int) (uint64, error)
	PendingNonceAt(ctx context.Context, account common.Address) (uint64, error)
	SuggestGasPrice(ctx context.Context) (*big.Int, error)
	CallContract(ctx context.Context, msg ethereum.CallMsg, block *big.Int) ([]byte, error)
	SendTransaction(ctx context.Context, tx *types.Transaction) error
	TransactionReceipt(ctx context.Context, hash common.Hash) (*types.Receipt, error)
	FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error)
}

// Failover spreads calls over several RPC endpoints. Reads go to the first endpoint that answers; an endpoint is
// skipped only when it fails to answer, never when it answers with an error about the request itself. Transactions
// are broadcast to every endpoint, so one unreachable endpoint cannot hide a transaction from the chain.
//
// An endpoint that refuses a call for too many requests gets no call at all for as long as it asks, so its limit can
// clear; a client that kept calling would stay refused. When every endpoint is resting, a call waits until the first
// of them may be asked again, up to restRounds times.
type Failover struct {
	backends []Backend
	timeout  time.Duration
	// Now returns the current time and Sleep waits for d or until ctx ends; tests replace both.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
	// OnRest, when set, is told each time a call waits for rate-limited endpoints.
	OnRest func(wait time.Duration)

	mu      sync.Mutex
	resting map[int]time.Time
}

// restRounds is how many times one call waits for rate-limited endpoints before it reports them unreachable.
const restRounds = 5

// NewFailover wraps backends in priority order. Each call to one endpoint gets at most timeout.
func NewFailover(timeout time.Duration, backends ...Backend) *Failover {
	return &Failover{backends: backends, timeout: timeout, resting: map[int]time.Time{}}
}

func (f *Failover) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *Failover) sleep(ctx context.Context, d time.Duration) error {
	if f.Sleep != nil {
		return f.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// awaitRest waits, when every endpoint is resting, until the first of them may be asked again.
func (f *Failover) awaitRest(ctx context.Context) error {
	shortest := time.Duration(0)
	for i := range f.backends {
		wait := f.restingFor(i)
		if wait <= 0 {
			return nil
		}
		if shortest == 0 || wait < shortest {
			shortest = wait
		}
	}
	if f.OnRest != nil {
		f.OnRest(shortest)
	}
	return f.sleep(ctx, shortest)
}

// restingFor is how much longer endpoint i asked to be left alone.
func (f *Failover) restingFor(i int) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resting[i].Sub(f.now())
}

// rest records that endpoint i refused a call for too many requests, when err says so.
func (f *Failover) rest(i int, err error) bool {
	wait, ok := rateLimited(err)
	if ok {
		f.mu.Lock()
		f.resting[i] = f.now().Add(wait)
		f.mu.Unlock()
	}
	return ok
}

// rateLimited reports whether err is an endpoint's HTTP 429, and how long it asked to be left alone: the
// retry_after_seconds its body gives, within 10 minutes, or one minute.
func rateLimited(err error) (time.Duration, bool) {
	var httpErr rpc.HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusTooManyRequests {
		return 0, false
	}
	var body struct {
		RetryAfter float64 `json:"retry_after_seconds"`
	}
	if json.Unmarshal(httpErr.Body, &body) == nil && body.RetryAfter > 0 && body.RetryAfter <= 600 {
		return time.Duration(body.RetryAfter * float64(time.Second)), true
	}
	return time.Minute, true
}

// errResting is a call not sent to endpoint i because it is resting.
func errResting(i int, wait time.Duration) error {
	return fmt.Errorf("endpoint %d limits this client's requests for %s more", i+1, wait.Round(time.Second))
}

// Dial connects to each RPC URL and checks that all of them serve chainID.
func Dial(ctx context.Context, chainID *big.Int, timeout time.Duration, urls ...string) (*Failover, error) {
	if len(urls) == 0 {
		return nil, errors.New("no RPC endpoint configured")
	}
	var backends []Backend
	for _, url := range urls {
		client, err := ethclient.DialContext(ctx, url)
		if err != nil {
			return nil, fmt.Errorf("RPC endpoint %s: %w", url, err)
		}
		backends = append(backends, client)
	}
	f := NewFailover(timeout, backends...)
	for i, b := range backends {
		callCtx, cancel := context.WithTimeout(ctx, timeout)
		got, err := b.ChainID(callCtx)
		cancel()
		if err == nil && got.Cmp(chainID) != 0 {
			return nil, fmt.Errorf("RPC endpoint %s serves chain %s, not %s", urls[i], got, chainID)
		}
	}
	return f, nil
}

// ErrUnreachable reports that no endpoint answered.
var ErrUnreachable = errors.New("no RPC endpoint answered")

// IsRequestError reports whether err is an endpoint's answer about the request itself (a revert, a nonce or fee
// rule, a missing receipt), as opposed to a failure to answer.
func IsRequestError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ethereum.NotFound) {
		return true
	}
	// A revert carries JSON-RPC error code 3. Other codes, such as -32000 or -32005, are as often a node's own
	// trouble (a missing header, a rate limit) as a judgement on the request, so they are read by their message.
	var coded interface{ ErrorCode() int }
	if errors.As(err, &coded) && coded.ErrorCode() == 3 {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, s := range requestErrorTexts {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

var requestErrorTexts = []string{
	"execution reverted", "nonce too low", "nonce too high", "already known", "known transaction",
	"replacement transaction underpriced", "transaction underpriced", "insufficient funds", "intrinsic gas too low",
	"gas limit", "invalid sender", "invalid transaction",
	// A call that fails inside the EVM without reverting, which each node words its own way: Frontier, which Bittensor
	// EVM runs, says "evm error: …" or "out of gas"; anvil "EVM error …"; geth names the failure itself.
	"evm error", "out of gas", "gas required exceeds allowance", "invalid opcode", "stack underflow",
	"stack limit reached", "invalid jump destination", "max call depth exceeded", "write protection",
	"return data out of bounds",
}

// IsKnownTransaction reports an endpoint saying it already holds this transaction.
func IsKnownTransaction(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already known") || strings.Contains(msg, "known transaction")
}

// IsNonceTooLow reports that the nonce was already used: a transaction with that nonce was mined.
func IsNonceTooLow(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "nonce too low")
}

func first[T any](f *Failover, ctx context.Context, call func(context.Context, Backend) (T, error)) (T, error) {
	var zero T
	var errs []error
	for round := 0; round < restRounds; round++ {
		if err := f.awaitRest(ctx); err != nil {
			return zero, err
		}
		v, err, done := firstRound(f, ctx, call, &errs)
		if done {
			return v, err
		}
	}
	return zero, fmt.Errorf("%w: %w", ErrUnreachable, errors.Join(errs...))
}

// firstRound tries each endpoint once. done is false when none answered.
func firstRound[T any](f *Failover, ctx context.Context, call func(context.Context, Backend) (T, error), errs *[]error) (T, error, bool) {
	var zero T
	for i, b := range f.backends {
		if wait := f.restingFor(i); wait > 0 {
			*errs = append(*errs, errResting(i, wait))
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, f.timeout)
		v, err := call(callCtx, b)
		cancel()
		if f.rest(i, err) {
			*errs = append(*errs, err)
			continue
		}
		if err == nil || IsRequestError(err) {
			return v, err, true
		}
		if ctx.Err() != nil {
			return zero, ctx.Err(), true
		}
		*errs = append(*errs, err)
	}
	return zero, nil, false
}

func (f *Failover) ChainID(ctx context.Context) (*big.Int, error) {
	return first(f, ctx, func(c context.Context, b Backend) (*big.Int, error) { return b.ChainID(c) })
}

func (f *Failover) HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error) {
	return first(f, ctx, func(c context.Context, b Backend) (*types.Header, error) { return b.HeaderByNumber(c, number) })
}

func (f *Failover) BlockNumber(ctx context.Context) (uint64, error) {
	return first(f, ctx, func(c context.Context, b Backend) (uint64, error) { return b.BlockNumber(c) })
}

func (f *Failover) BalanceAt(ctx context.Context, account common.Address, block *big.Int) (*big.Int, error) {
	return first(f, ctx, func(c context.Context, b Backend) (*big.Int, error) { return b.BalanceAt(c, account, block) })
}

func (f *Failover) CodeAt(ctx context.Context, account common.Address, block *big.Int) ([]byte, error) {
	return first(f, ctx, func(c context.Context, b Backend) ([]byte, error) { return b.CodeAt(c, account, block) })
}

func (f *Failover) NonceAt(ctx context.Context, account common.Address, block *big.Int) (uint64, error) {
	return first(f, ctx, func(c context.Context, b Backend) (uint64, error) { return b.NonceAt(c, account, block) })
}

func (f *Failover) PendingNonceAt(ctx context.Context, account common.Address) (uint64, error) {
	return first(f, ctx, func(c context.Context, b Backend) (uint64, error) { return b.PendingNonceAt(c, account) })
}

func (f *Failover) SuggestGasPrice(ctx context.Context) (*big.Int, error) {
	return first(f, ctx, func(c context.Context, b Backend) (*big.Int, error) { return b.SuggestGasPrice(c) })
}

func (f *Failover) CallContract(ctx context.Context, msg ethereum.CallMsg, block *big.Int) ([]byte, error) {
	return first(f, ctx, func(c context.Context, b Backend) ([]byte, error) { return b.CallContract(c, msg, block) })
}

func (f *Failover) TransactionReceipt(ctx context.Context, hash common.Hash) (*types.Receipt, error) {
	return first(f, ctx, func(c context.Context, b Backend) (*types.Receipt, error) { return b.TransactionReceipt(c, hash) })
}

func (f *Failover) FilterLogs(ctx context.Context, q ethereum.FilterQuery) ([]types.Log, error) {
	return first(f, ctx, func(c context.Context, b Backend) ([]types.Log, error) { return b.FilterLogs(c, q) })
}

// SendTransaction broadcasts tx to every endpoint. It succeeds if any endpoint accepted the transaction or already
// held it; otherwise it returns an endpoint's request error if there was one, else ErrUnreachable.
func (f *Failover) SendTransaction(ctx context.Context, tx *types.Transaction) error {
	var errs []error
	for round := 0; round < restRounds; round++ {
		if err := f.awaitRest(ctx); err != nil {
			return err
		}
		accepted, requestErr := false, error(nil)
		for i, b := range f.backends {
			if wait := f.restingFor(i); wait > 0 {
				errs = append(errs, errResting(i, wait))
				continue
			}
			callCtx, cancel := context.WithTimeout(ctx, f.timeout)
			err := b.SendTransaction(callCtx, tx)
			cancel()
			switch {
			case f.rest(i, err):
				errs = append(errs, err)
			case err == nil || IsKnownTransaction(err):
				accepted = true
			case IsRequestError(err):
				if requestErr == nil {
					requestErr = err
				}
			default:
				errs = append(errs, err)
			}
		}
		if accepted {
			return nil
		}
		if requestErr != nil {
			return requestErr
		}
	}
	return fmt.Errorf("%w: %w", ErrUnreachable, errors.Join(errs...))
}
