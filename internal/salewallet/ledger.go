package salewallet

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/auth"
	"github.com/ethereum/go-ethereum/common"
)

const (
	ledgerFile = "sale-ledger.json"
	lockFile   = "sale-ledger.lock"
	window     = 24 * time.Hour
	// staleLock is how old a lock file must be before another process may take it over.
	staleLock = time.Minute
)

var (
	// ErrPerSaleLimit means a sale's price is above the wallet's per-sale limit.
	ErrPerSaleLimit = errors.New("the price is above the wallet's per-sale limit")
	// ErrDailyLimit means funding the sale would exceed the wallet's daily limit.
	ErrDailyLimit = errors.New("funding this sale would exceed the wallet's daily limit")
	// ErrDeliveryBudget means another unfunded delivery would exceed the seller's daily budget for them.
	ErrDeliveryBudget = errors.New("the daily budget for deliveries to buyers who have not funded is spent")
)

// Entry is one ledger line: funding a sale (Kind "fund"), or a delivery to a buyer that has not funded yet (Kind
// "delivery", until Funded is set).
type Entry struct {
	Time     time.Time   `json:"time"`
	SaleID   common.Hash `json:"sale_id"`
	Kind     string      `json:"kind"`
	Wei      *big.Int    `json:"wei,omitempty"`
	USDCents int64       `json:"usd_cents,omitempty"`
	Funded   bool        `json:"funded,omitempty"`
}

// Ledger records spends so limits hold across processes and restarts.
type Ledger struct {
	Dir string
	Now func() time.Time
}

func (l Ledger) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// withLock runs fn on the entries under an exclusive lock and saves what fn returns.
func (l Ledger) withLock(fn func(entries []Entry) ([]Entry, error)) error {
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		return err
	}
	lock := filepath.Join(l.Dir, lockFile)
	deadline := time.Now().Add(10 * time.Second)
	for {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		if info, statErr := os.Stat(lock); statErr == nil && time.Since(info.ModTime()) > staleLock {
			_ = os.Remove(lock)
			continue
		}
		if time.Now().After(deadline) {
			return errors.New("the sale ledger is locked by another process")
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer os.Remove(lock)
	entries, err := l.read()
	if err != nil {
		return err
	}
	updated, err := fn(entries)
	if err != nil || updated == nil {
		return err
	}
	data, err := json.MarshalIndent(updated, "", "  ")
	if err != nil {
		return err
	}
	return auth.WriteRestrictedFile(filepath.Join(l.Dir, ledgerFile), append(data, '\n'))
}

func (l Ledger) read() ([]Entry, error) {
	data, err := os.ReadFile(filepath.Join(l.Dir, ledgerFile))
	if errors.Is(err, os.ErrNotExist) {
		return []Entry{}, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ledgerFile, err)
	}
	return entries, nil
}

// recent keeps the entries of the last 24 hours, the window every limit counts over.
func (l Ledger) recent(entries []Entry) []Entry {
	cutoff := l.now().Add(-window)
	var kept []Entry
	for _, e := range entries {
		if e.Time.After(cutoff) {
			kept = append(kept, e)
		}
	}
	return kept
}

// Spend checks a buyer's funding of price against the limits and records it. It returns whether a person must
// confirm the spend first; when they must, nothing is recorded and the caller calls Spend again with approved set.
func (l Ledger) Spend(limits Limits, saleID common.Hash, price *big.Int, approved bool) (needsApproval bool, err error) {
	if price.Cmp(limits.PerSale) > 0 {
		return false, fmt.Errorf("%w (%s wei above %s)", ErrPerSaleLimit, price, limits.PerSale)
	}
	if price.Cmp(limits.ApproveAbove) > 0 && !approved {
		return true, nil
	}
	err = l.withLock(func(entries []Entry) ([]Entry, error) {
		entries = l.recent(entries)
		spent := new(big.Int)
		for _, e := range entries {
			if e.Kind == "fund" {
				if e.SaleID == saleID {
					return nil, nil // already recorded
				}
				spent.Add(spent, e.Wei)
			}
		}
		if new(big.Int).Add(spent, price).Cmp(limits.Daily) > 0 {
			return nil, fmt.Errorf("%w (%s wei spent in the last 24 hours)", ErrDailyLimit, spent)
		}
		return append(entries, Entry{Time: l.now(), SaleID: saleID, Kind: "fund", Wei: new(big.Int).Set(price)}), nil
	})
	return false, err
}

// Deliver checks a seller's delivery to a buyer that has not funded against the daily budget and records it.
func (l Ledger) Deliver(limits Limits, saleID common.Hash, usdCents int64) error {
	return l.withLock(func(entries []Entry) ([]Entry, error) {
		entries = l.recent(entries)
		var spent int64
		for _, e := range entries {
			if e.Kind == "delivery" && !e.Funded {
				if e.SaleID == saleID {
					return nil, nil
				}
				spent += e.USDCents
			}
		}
		if spent+usdCents > limits.UnfundedDeliveryCents {
			return nil, fmt.Errorf("%w ($%.2f of $%.2f used)", ErrDeliveryBudget, float64(spent)/100, float64(limits.UnfundedDeliveryCents)/100)
		}
		return append(entries, Entry{Time: l.now(), SaleID: saleID, Kind: "delivery", USDCents: usdCents}), nil
	})
}

// Funded marks a delivery's sale as funded, which returns its charge to the delivery budget.
func (l Ledger) Funded(saleID common.Hash) error {
	return l.withLock(func(entries []Entry) ([]Entry, error) {
		entries = l.recent(entries)
		for i := range entries {
			if entries[i].Kind == "delivery" && entries[i].SaleID == saleID {
				entries[i].Funded = true
			}
		}
		return entries, nil
	})
}

// Spent returns what the wallet funded over the last 24 hours, the amount the daily limit counts.
func (l Ledger) Spent() (*big.Int, error) {
	spent := new(big.Int)
	err := l.withLock(func(entries []Entry) ([]Entry, error) {
		for _, e := range l.recent(entries) {
			if e.Kind == "fund" {
				spent.Add(spent, e.Wei)
			}
		}
		return nil, nil
	})
	return spent, err
}

// Unfunded returns the buyer-delivery charges still counted against today's budget, in US cents.
func (l Ledger) Unfunded() (int64, error) {
	var spent int64
	err := l.withLock(func(entries []Entry) ([]Entry, error) {
		for _, e := range l.recent(entries) {
			if e.Kind == "delivery" && !e.Funded {
				spent += e.USDCents
			}
		}
		return nil, nil
	})
	return spent, err
}
