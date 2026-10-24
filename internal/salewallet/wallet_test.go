package salewallet

import (
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/testutil"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

type failingKeyring struct{}

func (failingKeyring) Get(string, string) (string, error) { return "", errors.New("no keyring") }
func (failingKeyring) Set(string, string, string) error   { return errors.New("no keyring") }
func (failingKeyring) Delete(string, string) error        { return errors.New("no keyring") }

func TestAWalletIsCreatedLoadedAndKeptInTheKeyring(t *testing.T) {
	keyring := &testutil.MemoryKeyring{}
	store := Store{Dir: t.TempDir(), Namespace: "beam", Keyring: keyring}
	created, err := store.Create(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(nil); !errors.Is(err, ErrWalletExists) {
		t.Fatalf("second Create = %v; want ErrWalletExists", err)
	}
	loaded, err := store.Load()
	if err != nil || loaded.Address() != created.Address() || loaded.Secret != created.Secret || loaded.Store != "keyring" {
		t.Fatalf("Load = %+v, %v", loaded, err)
	}
	data, _ := os.ReadFile(filepath.Join(store.Dir, walletFile))
	if strings.Contains(string(data), created.Secret.Hex()[2:]) || strings.Contains(string(data), common.Bytes2Hex(crypto.FromECDSA(created.Key))) {
		t.Fatal("the wallet file holds a key or the secret although the keyring is available")
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(filepath.Join(store.Dir, walletFile)); info.Mode().Perm() != 0o600 {
			t.Fatalf("wallet file mode = %v", info.Mode().Perm())
		}
	}
}

func TestWithoutAKeyringTheKeysGoToTheOwnerOnlyFile(t *testing.T) {
	store := Store{Dir: t.TempDir(), Namespace: "beam", Keyring: failingKeyring{}}
	created, err := store.Create(nil)
	if err != nil || created.Store != "file" {
		t.Fatalf("Create = %+v, %v", created, err)
	}
	loaded, err := (Store{Dir: store.Dir, Namespace: "beam"}).Load()
	if err != nil || loaded.Address() != created.Address() {
		t.Fatalf("Load = %+v, %v", loaded, err)
	}
}

func TestEachReleaseChannelHasItsOwnKeyringItems(t *testing.T) {
	keyring := &testutil.MemoryKeyring{}
	prod, err := (Store{Dir: t.TempDir(), Namespace: "beam", Keyring: keyring}).Create(nil)
	if err != nil {
		t.Fatal(err)
	}
	dev, err := (Store{Dir: t.TempDir(), Namespace: "beam-dev", Keyring: keyring}).Create(nil)
	if err != nil {
		t.Fatal(err)
	}
	if prod.Address() == dev.Address() {
		t.Fatal("two channels share one wallet")
	}
}

func TestImportBackupLimitsAndRemove(t *testing.T) {
	keyring := &testutil.MemoryKeyring{}
	store := Store{Dir: t.TempDir(), Namespace: "beam", Keyring: keyring}
	key, _ := crypto.GenerateKey()
	imported, err := store.Create(key)
	if err != nil || imported.Address() != crypto.PubkeyToAddress(key.PublicKey) {
		t.Fatalf("Create(key) = %+v, %v", imported, err)
	}
	withBackup, err := store.AddBackup()
	if err != nil || withBackup.Backup == nil {
		t.Fatalf("AddBackup = %+v, %v", withBackup, err)
	}
	if loaded, err := store.Load(); err != nil || loaded.BackupAddress() != withBackup.BackupAddress() {
		t.Fatalf("Load after AddBackup = %+v, %v", loaded, err)
	}
	limits := DefaultLimits()
	limits.Daily = big.NewInt(5)
	if _, err := store.SetLimits(limits); err != nil {
		t.Fatal(err)
	}
	limits.PerSale = big.NewInt(-1)
	if _, err := store.SetLimits(limits); err == nil {
		t.Fatal("a negative limit was accepted")
	}
	if loaded, _ := store.Load(); loaded.Limits.Daily.Cmp(big.NewInt(5)) != 0 {
		t.Fatalf("limits not saved: %+v", loaded.Limits)
	}
	if err := store.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); !errors.Is(err, ErrNoWallet) {
		t.Fatalf("Load after Remove = %v", err)
	}
	if _, err := keyring.Get(keyringService, store.keyringUser("key")); err == nil {
		t.Fatal("Remove left the key in the keyring")
	}
}

func TestSpendingLimitsHoldOverADay(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	ledger := Ledger{Dir: t.TempDir(), Now: func() time.Time { return now }}
	limits := Limits{PerSale: tao(1, 1), Daily: tao(15, 10), ApproveAbove: tao(5, 10), UnfundedDeliveryCents: 500}
	sale := func(n byte) common.Hash { return common.BytesToHash([]byte{n}) }

	if _, err := ledger.Spend(limits, sale(1), tao(2, 1), true); !errors.Is(err, ErrPerSaleLimit) {
		t.Fatalf("over the per-sale limit: %v", err)
	}
	if needs, err := ledger.Spend(limits, sale(1), tao(8, 10), false); err != nil || !needs {
		t.Fatalf("above the approval threshold: needs = %v, %v", needs, err)
	}
	if _, err := ledger.Spend(limits, sale(1), tao(8, 10), true); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Spend(limits, sale(1), tao(8, 10), true); err != nil {
		t.Fatalf("recording the same sale twice must not count twice: %v", err)
	}
	if _, err := ledger.Spend(limits, sale(2), tao(8, 10), true); !errors.Is(err, ErrDailyLimit) {
		t.Fatalf("over the daily limit: %v", err)
	}
	now = now.Add(25 * time.Hour)
	if _, err := ledger.Spend(limits, sale(2), tao(8, 10), true); err != nil {
		t.Fatalf("a day later the limit has room again: %v", err)
	}
}

func TestTheUnfundedDeliveryBudgetCountsOnlyUnfundedDeliveries(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	ledger := Ledger{Dir: t.TempDir(), Now: func() time.Time { return now }}
	limits := DefaultLimits() // $5.00
	for i := byte(1); i <= 8; i++ {
		if err := ledger.Deliver(limits, common.BytesToHash([]byte{i}), 61); err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}
	if err := ledger.Deliver(limits, common.BytesToHash([]byte{9}), 61); !errors.Is(err, ErrDeliveryBudget) {
		t.Fatalf("ninth delivery: %v; want ErrDeliveryBudget", err)
	}
	if err := ledger.Funded(common.BytesToHash([]byte{1})); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Deliver(limits, common.BytesToHash([]byte{9}), 61); err != nil {
		t.Fatalf("a funded sale returns its charge to the budget: %v", err)
	}
	if spent, _ := ledger.Unfunded(); spent != 8*61 {
		t.Fatalf("Unfunded = %d", spent)
	}
}

// refusingBackup is a keyring that stores everything but a backup key. Before refusing one it runs block, so a test
// can stop the wallet file being written in the middle of a save.
type refusingBackup struct {
	testutil.MemoryKeyring
	block func()
}

func (k *refusingBackup) Set(service, user, value string) error {
	if strings.HasSuffix(user, "backup key") {
		if k.block != nil {
			k.block()
		}
		return errors.New("refused")
	}
	return k.MemoryKeyring.Set(service, user, value)
}

func TestAKeyringThatRefusesAnItemNeverLosesAKey(t *testing.T) {
	keyring := &refusingBackup{}
	store := Store{Dir: t.TempDir(), Namespace: "beam", Keyring: keyring}
	created, err := store.Create(nil)
	if err != nil || created.Store != storeKeyring {
		t.Fatalf("Create = %s, %v; want the keyring", created.Store, err)
	}

	// The file cannot be written, because a directory stands where it goes: the keys stay where they were.
	path := filepath.Join(store.Dir, walletFile)
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	keyring.block = func() {
		_ = os.Remove(path)
		_ = os.MkdirAll(filepath.Join(path, "blocked"), 0o700)
	}
	if _, err := store.AddBackup(); err == nil {
		t.Fatal("AddBackup succeeded without writing the wallet file")
	}
	keyring.block = nil
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, saved, 0o600); err != nil {
		t.Fatal(err)
	}
	if loaded, err := store.Load(); err != nil || loaded.Address() != created.Address() || loaded.Secret != created.Secret {
		t.Fatalf("after a failed save, Load = %v; want the wallet as created", err)
	}

	// The file can be written: every key moves to it.
	withBackup, err := store.AddBackup()
	if err != nil || withBackup.Store != storeFile {
		t.Fatalf("AddBackup = %s, %v; want the file", withBackup.Store, err)
	}
	loaded, err := store.Load()
	if err != nil || loaded.Address() != created.Address() || loaded.Secret != created.Secret || loaded.BackupAddress() != withBackup.BackupAddress() {
		t.Fatalf("Load = %v; want every key from the file", err)
	}
	if _, err := keyring.Get(keyringService, store.keyringUser("key")); err == nil {
		t.Fatal("the key stayed in the keyring after moving to the file")
	}
}
