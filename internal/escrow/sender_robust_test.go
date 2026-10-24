package escrow

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
)

// rpcError is a JSON-RPC error object as an endpoint returns it, with a code and a message.
type rpcError struct {
	code    int
	message string
}

func (e rpcError) Error() string  { return e.message }
func (e rpcError) ErrorCode() int { return e.code }
func (e rpcError) ErrorData() any { return nil }

func TestOnlyRevertsAndRequestFaultsCountAsAnswersAboutTheRequest(t *testing.T) {
	for _, test := range []struct {
		err     error
		request bool
	}{
		{rpcError{3, "execution reverted"}, true},
		{rpcError{-32000, "insufficient funds for gas * price + value"}, true},
		{rpcError{-32000, "nonce too low"}, true},
		{rpcError{-32603, "evm error: StackUnderflow"}, true},
		{rpcError{-32603, "out of gas"}, true},
		{rpcError{-32003, "EVM error InvalidFEOpcode"}, true},
		{rpcError{-32000, "invalid opcode: INVALID"}, true},
		{rpcError{-32000, "header not found"}, false},
		{rpcError{-32005, "limit exceeded"}, false},
		{rpcError{-32603, "internal error"}, false},
	} {
		if got := IsRequestError(test.err); got != test.request {
			t.Errorf("IsRequestError(%d %q) = %v; want %v", test.err.(rpcError).code, test.err.Error(), got, test.request)
		}
	}
}

// lossyReads fails reads with a node error for a while once a transaction has been sent.
type lossyReads struct {
	*fakeChain
	failing int
}

func (l *lossyReads) BlockNumber(ctx context.Context) (uint64, error) {
	if len(l.fakeChain.sends) > 0 && l.failing > 0 {
		l.failing--
		return 0, rpcError{-32000, "header not found"}
	}
	return l.fakeChain.BlockNumber(ctx)
}

func TestAWatchedTransactionOutlastsNodeErrors(t *testing.T) {
	chain := &lossyReads{fakeChain: newFakeChain(), failing: 5}
	out, err := testSender(t, chain).Send(context.Background(), testRequest())
	if err != nil || out.Receipt == nil || len(out.Attempts) != 1 {
		t.Fatalf("Send = %+v, %v; want the attempt watched through the node errors and included", out, err)
	}
}

func TestAFeeIsNeverRaisedAboveWhatTheAccountCanPay(t *testing.T) {
	chain := newFakeChain()
	sender := testSender(t, chain)
	primary := sender.Primary.Address()
	request := testRequest()
	// The account can pay for the gas at the starting price and no more, and the chain will not include that
	// price for a while.
	chain.balance = new(big.Int).Mul(chain.suggested, new(big.Int).SetUint64(request.GasLimit))
	chain.floor[primary] = new(big.Int).Mul(chain.suggested, big.NewInt(2))
	start := chain.head
	chain.beforeMine = func(head uint64) {
		if head > start+20 {
			chain.floor[primary] = nil
		}
	}
	out, err := sender.Send(context.Background(), request)
	if err != nil || out.Receipt == nil {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	for _, attempt := range out.Attempts {
		if new(big.Int).Mul(attempt.GasPrice, new(big.Int).SetUint64(request.GasLimit)).Cmp(chain.balance) > 0 {
			t.Fatalf("an attempt at %s per gas costs more than the account holds", attempt.GasPrice)
		}
	}
	if len(out.Attempts) != 1 {
		t.Fatalf("%d attempts; want the first kept and sent again, with no replacement it could not pay for", len(out.Attempts))
	}
}

func TestAReplacementTheChainRefusesKeepsTheAttemptAlreadyOut(t *testing.T) {
	chain := &refusingReplacement{fakeChain: newFakeChain()}
	sender := testSender(t, chain)
	primary := sender.Primary.Address()
	chain.floor[primary] = new(big.Int).Mul(chain.suggested, big.NewInt(100))
	start := chain.head
	chain.beforeMine = func(head uint64) {
		if head > start+12 {
			chain.floor[primary] = nil
		}
	}
	out, err := sender.Send(context.Background(), testRequest())
	if err != nil || out.Receipt == nil || out.Receipt.TxHash != out.Attempts[0].Hash {
		t.Fatalf("Send = %+v, %v; want the first attempt included", out, err)
	}
}

// refusingReplacement refuses a replacement, another transaction at a nonce already sent, as a node does when the
// account cannot pay for its higher fee.
type refusingReplacement struct {
	*fakeChain
}

func (r *refusingReplacement) SendTransaction(ctx context.Context, tx *types.Transaction) error {
	r.fakeChain.mu.Lock()
	from := r.fakeChain.sender(tx)
	replacement := false
	for _, sent := range r.fakeChain.sends {
		if r.fakeChain.sender(sent) == from && sent.Nonce() == tx.Nonce() && sent.Hash() != tx.Hash() {
			replacement = true
		}
	}
	r.fakeChain.mu.Unlock()
	if replacement {
		return rpcError{-32000, "insufficient funds for gas * price + value"}
	}
	return r.fakeChain.SendTransaction(ctx, tx)
}
