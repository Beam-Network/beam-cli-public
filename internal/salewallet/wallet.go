// Package salewallet keeps the data-sale spending wallet: the account sales are paid from and to, the secret every
// per-sale key derives from, an optional backup account for revealing keys, the spending limits, and the ledger that
// enforces them. Keys live in the operating-system keyring when one is available, and otherwise in a file only the
// owner can read.
package salewallet

import (
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	"github.com/Beam-Network/beam-cli-public/internal/auth"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

const (
	walletFile     = "sale-wallet.json"
	walletVersion  = 1
	keyringService = "Beam CLI"
	storeKeyring   = "keyring"
	storeFile      = "file"
)

var (
	// ErrNoWallet means no sale wallet exists yet.
	ErrNoWallet = errors.New("no sale wallet")
	// ErrWalletExists means a sale wallet already exists; it is never overwritten.
	ErrWalletExists = errors.New("a sale wallet already exists")
)

// Limits bound what the wallet spends. PerSale caps one sale's price, Daily caps funding over any 24 hours, and a
// spend above ApproveAbove needs a person to confirm it at a terminal. UnfundedDeliveryCents caps, per 24 hours, the
// Beam charges a seller spends delivering to buyers who have not funded.
type Limits struct {
	PerSale               *big.Int `json:"per_sale_wei"`
	Daily                 *big.Int `json:"daily_wei"`
	ApproveAbove          *big.Int `json:"approve_above_wei"`
	UnfundedDeliveryCents int64    `json:"unfunded_delivery_daily_usd_cents"`
}

// DefaultLimits are the limits a new wallet starts with.
func DefaultLimits() Limits {
	return Limits{
		PerSale:               tao(1, 1),
		Daily:                 tao(3, 1),
		ApproveAbove:          tao(2, 10),
		UnfundedDeliveryCents: 500,
	}
}

// tao returns num/den TAO in wei.
func tao(num, den int64) *big.Int {
	v := new(big.Int).Mul(big.NewInt(num), big.NewInt(1e18))
	return v.Div(v, big.NewInt(den))
}

// Wallet is a loaded sale wallet.
type Wallet struct {
	Key    *ecdsa.PrivateKey
	Secret common.Hash
	Backup *ecdsa.PrivateKey
	Limits Limits
	// Store is where the keys are kept: "keyring" or "file".
	Store string
}

// Address is the wallet's account.
func (w Wallet) Address() common.Address { return crypto.PubkeyToAddress(w.Key.PublicKey) }

// BackupAddress is the backup account, or the zero address when there is none.
func (w Wallet) BackupAddress() common.Address {
	if w.Backup == nil {
		return common.Address{}
	}
	return crypto.PubkeyToAddress(w.Backup.PublicKey)
}

type record struct {
	Version       int    `json:"version"`
	Address       string `json:"address"`
	BackupAddress string `json:"backup_address,omitempty"`
	SecretStore   string `json:"secret_store"`
	Key           string `json:"key,omitempty"`
	Secret        string `json:"secret,omitempty"`
	BackupKey     string `json:"backup_key,omitempty"`
	Limits        Limits `json:"limits"`
}

// Store keeps one sale wallet per state namespace in Dir.
type Store struct {
	Dir       string
	Namespace string
	Keyring   auth.Keyring
}

func (s Store) path() string { return filepath.Join(s.Dir, walletFile) }

func (s Store) keyringUser(item string) string {
	return s.Namespace + " sale wallet " + item
}

// Exists reports whether a wallet has been created.
func (s Store) Exists() (bool, error) {
	_, err := os.Stat(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// Create makes a new wallet with a fresh key and secret, or imports key when it is not nil.
func (s Store) Create(key *ecdsa.PrivateKey) (Wallet, error) {
	if exists, err := s.Exists(); err != nil || exists {
		if err != nil {
			return Wallet{}, err
		}
		return Wallet{}, ErrWalletExists
	}
	if key == nil {
		generated, err := crypto.GenerateKey()
		if err != nil {
			return Wallet{}, err
		}
		key = generated
	}
	var secret common.Hash
	if _, err := rand.Read(secret[:]); err != nil {
		return Wallet{}, err
	}
	w := Wallet{Key: key, Secret: secret, Limits: DefaultLimits()}
	return w, s.save(&w)
}

// AddBackup gives the wallet a backup account, used only to reveal keys when the main account is stuck.
func (s Store) AddBackup() (Wallet, error) {
	w, err := s.Load()
	if err != nil {
		return Wallet{}, err
	}
	if w.Backup != nil {
		return Wallet{}, errors.New("the sale wallet already has a backup account")
	}
	backup, err := crypto.GenerateKey()
	if err != nil {
		return Wallet{}, err
	}
	w.Backup = backup
	return w, s.save(&w)
}

// SetLimits replaces the wallet's limits.
func (s Store) SetLimits(limits Limits) (Wallet, error) {
	w, err := s.Load()
	if err != nil {
		return Wallet{}, err
	}
	if err := limits.validate(); err != nil {
		return Wallet{}, err
	}
	w.Limits = limits
	return w, s.save(&w)
}

func (l Limits) validate() error {
	for name, v := range map[string]*big.Int{"per-sale": l.PerSale, "daily": l.Daily, "approval": l.ApproveAbove} {
		if v == nil || v.Sign() < 0 {
			return fmt.Errorf("the %s limit must be zero or more", name)
		}
	}
	if l.UnfundedDeliveryCents < 0 {
		return errors.New("the unfunded delivery budget must be zero or more")
	}
	return nil
}

// Load reads the wallet.
func (s Store) Load() (Wallet, error) {
	data, err := os.ReadFile(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return Wallet{}, ErrNoWallet
	}
	if err != nil {
		return Wallet{}, err
	}
	var r record
	if err := json.Unmarshal(data, &r); err != nil {
		return Wallet{}, fmt.Errorf("parse %s: %w", walletFile, err)
	}
	if r.Version != walletVersion {
		return Wallet{}, fmt.Errorf("%s has version %d; this build reads version %d", walletFile, r.Version, walletVersion)
	}
	keyHex, secretHex, backupHex := r.Key, r.Secret, r.BackupKey
	if r.SecretStore == storeKeyring {
		if s.Keyring == nil {
			return Wallet{}, errors.New("the sale wallet's keys are in the keyring, which is unavailable")
		}
		if keyHex, err = s.Keyring.Get(keyringService, s.keyringUser("key")); err != nil {
			return Wallet{}, fmt.Errorf("read the sale wallet key from the keyring: %w", err)
		}
		if secretHex, err = s.Keyring.Get(keyringService, s.keyringUser("secret")); err != nil {
			return Wallet{}, fmt.Errorf("read the sale wallet secret from the keyring: %w", err)
		}
		if r.BackupAddress != "" {
			if backupHex, err = s.Keyring.Get(keyringService, s.keyringUser("backup key")); err != nil {
				return Wallet{}, fmt.Errorf("read the sale wallet backup key from the keyring: %w", err)
			}
		}
	}
	w := Wallet{Limits: r.Limits, Store: r.SecretStore}
	if w.Key, err = crypto.HexToECDSA(strings.TrimPrefix(keyHex, "0x")); err != nil {
		return Wallet{}, errors.New("the sale wallet key is malformed")
	}
	if !strings.EqualFold(w.Address().Hex(), r.Address) {
		return Wallet{}, errors.New("the sale wallet key does not match its recorded address")
	}
	secret, err := hex.DecodeString(strings.TrimPrefix(secretHex, "0x"))
	if err != nil || len(secret) != 32 {
		return Wallet{}, errors.New("the sale wallet secret is malformed")
	}
	w.Secret = common.BytesToHash(secret)
	if backupHex != "" {
		if w.Backup, err = crypto.HexToECDSA(strings.TrimPrefix(backupHex, "0x")); err != nil {
			return Wallet{}, errors.New("the sale wallet backup key is malformed")
		}
	}
	return w, nil
}

// Remove deletes the wallet and its keyring items. The keys are gone for good.
func (s Store) Remove() error {
	if s.Keyring != nil {
		for _, item := range []string{"key", "secret", "backup key"} {
			_ = s.Keyring.Delete(keyringService, s.keyringUser(item))
		}
	}
	err := os.Remove(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return ErrNoWallet
	}
	return err
}

// save writes the wallet: keys to the keyring when every item can be stored there, otherwise to the owner-only file.
// Keyring items are deleted only after the file holding the keys is written, so a failure never loses a key.
func (s Store) save(w *Wallet) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	r := record{Version: walletVersion, Address: w.Address().Hex(), Limits: w.Limits}
	items := map[string]string{"key": hex.EncodeToString(crypto.FromECDSA(w.Key)), "secret": hex.EncodeToString(w.Secret[:])}
	if w.Backup != nil {
		r.BackupAddress = w.BackupAddress().Hex()
		items["backup key"] = hex.EncodeToString(crypto.FromECDSA(w.Backup))
	}
	inKeyring := s.Keyring != nil && s.keyringSet(items)
	if inKeyring {
		r.SecretStore = storeKeyring
	} else {
		r.SecretStore = storeFile
		r.Key, r.Secret, r.BackupKey = items["key"], items["secret"], items["backup key"]
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := auth.WriteRestrictedFile(s.path(), append(data, '\n')); err != nil {
		return err
	}
	w.Store = r.SecretStore
	if !inKeyring && s.Keyring != nil {
		for _, item := range []string{"key", "secret", "backup key"} {
			_ = s.Keyring.Delete(keyringService, s.keyringUser(item))
		}
	}
	return nil
}

// keyringSet stores every item in the keyring and reports whether all of them were stored.
func (s Store) keyringSet(items map[string]string) bool {
	for item, value := range items {
		if err := s.Keyring.Set(keyringService, s.keyringUser(item), value); err != nil {
			return false
		}
	}
	return true
}
