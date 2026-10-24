package auth

import (
	"os"
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/testutil"
)

// TestMain replaces the operating-system keyring with one that fails the test
// that reaches it. Tests give a Store an in-memory keyring or none.
func TestMain(m *testing.M) {
	UseSystemKeyring(testutil.RefusingKeyring{})
	os.Exit(m.Run())
}
