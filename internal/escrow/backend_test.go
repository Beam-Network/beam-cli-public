package escrow

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/rpc"
)

// limitedChain answers BlockNumber with an HTTP 429 while limited, and counts the calls that reach it.
type limitedChain struct {
	*fakeChain
	limited bool
	reached int
}

func (l *limitedChain) BlockNumber(ctx context.Context) (uint64, error) {
	l.reached++
	if l.limited {
		return 0, rpc.HTTPError{StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests",
			Body: []byte(`{"error":"Too many requests from this source.","retry_after_seconds":60}`)}
	}
	return l.fakeChain.BlockNumber(ctx)
}

func TestACallWaitsOutARateLimitInsteadOfFailing(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	limited := &limitedChain{fakeChain: newFakeChain(), limited: true}
	f := NewFailover(time.Second, limited)
	f.Now = func() time.Time { return now }
	var waits []time.Duration
	f.OnRest = func(d time.Duration) { waits = append(waits, d) }
	f.Sleep = func(_ context.Context, d time.Duration) error {
		now = now.Add(d)
		limited.limited = false
		return nil
	}
	if _, err := f.BlockNumber(context.Background()); err != nil {
		t.Fatalf("BlockNumber = %v; want it to wait out the limit and answer", err)
	}
	if limited.reached != 2 || len(waits) != 1 || waits[0] != time.Minute {
		t.Fatalf("calls %d, waits %v; want 2 calls around one wait of the 60 s the endpoint asked for", limited.reached, waits)
	}
}

func TestARateLimitedEndpointGetsNoCallWhileItRests(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	limited := &limitedChain{fakeChain: newFakeChain(), limited: true}
	f := NewFailover(time.Second, limited)
	f.Now = func() time.Time { return now }
	slept := time.Duration(0)
	f.Sleep = func(_ context.Context, d time.Duration) error {
		slept += d
		now = now.Add(d)
		return nil
	}
	if _, err := f.BlockNumber(context.Background()); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("BlockNumber = %v; want ErrUnreachable once the endpoint keeps refusing", err)
	}
	if limited.reached != restRounds || slept != time.Duration(restRounds-1)*time.Minute {
		t.Fatalf("calls %d after %s asleep; want one call per round, each after the minute the endpoint asked for", limited.reached, slept)
	}
}

func TestAWaitForARateLimitEndsWithTheContext(t *testing.T) {
	limited := &limitedChain{fakeChain: newFakeChain(), limited: true}
	f := NewFailover(time.Second, limited)
	ctx, cancel := context.WithCancel(context.Background())
	f.Sleep = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}
	if _, err := f.BlockNumber(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("BlockNumber = %v; want context.Canceled", err)
	}
}

func TestReadsGoToAnotherEndpointWhileOneIsRateLimited(t *testing.T) {
	ctx := context.Background()
	limited := &limitedChain{fakeChain: newFakeChain(), limited: true}
	other := newFakeChain()
	f := NewFailover(time.Second, limited, other)
	for i := 0; i < 3; i++ {
		if _, err := f.BlockNumber(ctx); err != nil {
			t.Fatalf("BlockNumber = %v; the other endpoint should answer", err)
		}
	}
	if limited.reached != 1 {
		t.Fatalf("the rate-limited endpoint was called %d times; want 1", limited.reached)
	}
}

func TestARateLimitWithoutADurationRestsTheEndpointAMinute(t *testing.T) {
	wait, ok := rateLimited(rpc.HTTPError{StatusCode: http.StatusTooManyRequests, Body: []byte("slow down")})
	if !ok || wait != time.Minute {
		t.Fatalf("rateLimited = %s, %v; want 1m, true", wait, ok)
	}
	if _, ok := rateLimited(rpc.HTTPError{StatusCode: http.StatusBadGateway}); ok {
		t.Fatal("a 502 was taken for a rate limit")
	}
}
