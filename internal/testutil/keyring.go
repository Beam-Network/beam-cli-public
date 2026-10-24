package testutil

import (
	"fmt"
	"sync"

	"github.com/zalando/go-keyring"
)

// MemoryKeyring is an in-memory credential store with the semantics of the
// operating-system keyring: a missing item is keyring.ErrNotFound.
type MemoryKeyring struct {
	mu    sync.Mutex
	items map[[2]string]string
}

func (k *MemoryKeyring) Get(service, user string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	password, ok := k.items[[2]string{service, user}]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return password, nil
}

func (k *MemoryKeyring) Set(service, user, password string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.items == nil {
		k.items = make(map[[2]string]string)
	}
	k.items[[2]string{service, user}] = password
	return nil
}

func (k *MemoryKeyring) Delete(service, user string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.items[[2]string{service, user}]; !ok {
		return keyring.ErrNotFound
	}
	delete(k.items, [2]string{service, user})
	return nil
}

// RefusingKeyring stands in for the operating-system keyring in tests. Any use
// panics, so a test that would read, write, or delete the developer's real
// credentials fails instead.
type RefusingKeyring struct{}

func (RefusingKeyring) Get(service, user string) (string, error) {
	panic(refusal("read", service, user))
}

func (RefusingKeyring) Set(service, user, _ string) error {
	panic(refusal("write", service, user))
}

func (RefusingKeyring) Delete(service, user string) error {
	panic(refusal("delete", service, user))
}

func refusal(operation, service, user string) string {
	return fmt.Sprintf("test tried to %s the real keychain item %q/%q; give the store an in-memory keyring (auth.NewStoreWithKeyring with testutil.MemoryKeyring)", operation, service, user)
}
