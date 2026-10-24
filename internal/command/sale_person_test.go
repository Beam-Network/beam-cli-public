//go:build !windows

package command

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Beam-Network/beam-cli-public/internal/config"
	"github.com/Beam-Network/beam-cli-public/internal/datasale"
	"github.com/Beam-Network/beam-cli-public/internal/salewallet"
)

// personPath is one way a command can be run: whether its errors and its input are on a terminal, extra global
// options, and the answer it is given.
type personPath struct {
	name                          string
	global                        []string
	stderrTerminal, stdinTerminal bool
	answer                        string
}

// Every way to run a person-only action except a person answering yes at an interactive terminal.
var refusedPaths = []personPath{
	{name: "piped input", answer: "y"},
	{name: "--json at a terminal", global: []string{"--json"}, stderrTerminal: true, stdinTerminal: true, answer: "y"},
	{name: "--quiet at a terminal", global: []string{"--quiet"}, stderrTerminal: true, stdinTerminal: true, answer: "y"},
	{name: "--no-interactive at a terminal", global: []string{"--no-interactive"}, stderrTerminal: true, stdinTerminal: true, answer: "y"},
	{name: "errors on a terminal, input piped", stderrTerminal: true, answer: "y"},
	{name: "input from a terminal, errors piped", stdinTerminal: true, answer: "y"},
	{name: "a person at a terminal answers no", stderrTerminal: true, stdinTerminal: true, answer: "n"},
}

var personAtTerminal = personPath{name: "a person at a terminal answers yes", stderrTerminal: true, stdinTerminal: true, answer: "y"}

func runAs(path personPath, args ...string) (string, string, int) {
	app := newWebhookApp()
	app.isTerminal = func(io.Writer) bool { return path.stderrTerminal }
	app.stdinTerminal = func() bool { return path.stdinTerminal }
	app.stdin = strings.NewReader(path.answer + "\n")
	app.readLine = func() (string, error) { return path.answer, nil }
	return runWebhookCLI(app, append(append([]string{}, path.global...), args...)...)
}

// personOnlyWallet configures a sale escrow on a local chain and creates a funded wallet in its own configuration
// directory, as the environment every command of a test reads.
func personOnlyWallet(t *testing.T, ctx context.Context, chain *saleChain, funded bool) (config.Paths, common.Address) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BEAM_CONFIG_DIR", dir)
	t.Setenv("BEAM_SALE_CHAIN_ID", "945")
	t.Setenv("BEAM_SALE_RPC_URLS", chain.url)
	t.Setenv("BEAM_SALE_ESCROW", chain.escrow.Hex())
	stdout, stderr, code := runAs(personPath{}, "--json", "sale", "wallet", "create")
	if code != ExitOK {
		t.Fatalf("wallet create: exit %d, %s", code, stderr)
	}
	var created struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal([]byte(stdout), &created); err != nil {
		t.Fatal(err)
	}
	address := common.HexToAddress(created.Address)
	if funded {
		chain.fund(t, ctx, address)
	}
	return config.Paths{Dir: dir}, address
}

// keyMaterial is the wallet's private key, secret, and a sale key derived from it, as lowercase hex.
func keyMaterial(t *testing.T, paths config.Paths) []string {
	t.Helper()
	wallet, err := saleWalletStore(paths).Load()
	if err != nil {
		t.Fatal(err)
	}
	return []string{
		hex.EncodeToString(crypto.FromECDSA(wallet.Key)),
		hex.EncodeToString(wallet.Secret[:]),
		hex.EncodeToString(datasale.SellerKey(wallet.Secret, common.HexToHash("0x01")).Bytes()),
	}
}

func assertNoKeyMaterial(t *testing.T, secrets []string, outputs ...string) {
	t.Helper()
	for _, output := range outputs {
		lowered := strings.ToLower(output)
		for _, secret := range secrets {
			if strings.Contains(lowered, secret) {
				t.Fatal("key material appeared in a command's output")
			}
		}
	}
}

func TestPersonOnlyActionsRefuseEveryPathButAPersonAtATerminal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	chain := startSaleChain(t, ctx)
	recipient := common.HexToAddress("0x00000000000000000000000000000000000Be4a1")

	actions := []struct {
		name   string
		funded bool
		args   []string
		// done reports whether the action took effect.
		done func(t *testing.T, paths config.Paths) bool
	}{
		{
			name: "raise a limit", args: []string{"sale", "wallet", "limits", "--per-sale", "5"},
			done: func(t *testing.T, paths config.Paths) bool {
				wallet, err := saleWalletStore(paths).Load()
				if err != nil {
					t.Fatal(err)
				}
				return wallet.Limits.PerSale.Cmp(new(big.Int).Mul(big.NewInt(5), weiPerTAO)) == 0
			},
		},
		{
			name: "send TAO out", funded: true, args: []string{"sale", "wallet", "send", "--to", recipient.Hex(), "--all"},
			done: func(t *testing.T, _ config.Paths) bool { return chain.balance(t, ctx, recipient).Sign() > 0 },
		},
		{
			name: "remove the wallet", args: []string{"sale", "wallet", "remove"},
			done: func(t *testing.T, paths config.Paths) bool {
				exists, err := saleWalletStore(paths).Exists()
				if err != nil {
					t.Fatal(err)
				}
				return !exists
			},
		},
	}
	for _, action := range actions {
		t.Run(action.name, func(t *testing.T) {
			paths, _ := personOnlyWallet(t, ctx, chain, action.funded)
			secrets := keyMaterial(t, paths)
			for _, path := range refusedPaths {
				stdout, stderr, code := runAs(path, action.args...)
				assertNoKeyMaterial(t, secrets, stdout, stderr)
				if code == ExitOK || action.done(t, paths) {
					t.Fatalf("%s: exit %d and the action took effect; want it refused\n%s", path.name, code, stderr)
				}
			}
			// No flag stands in for the person.
			yes := append(append([]string{}, action.args...), "--yes")
			if _, stderr, code := runAs(personAtTerminal, yes...); code != ExitUsage || action.done(t, paths) {
				t.Fatalf("--yes: exit %d; want a usage error and nothing done\n%s", code, stderr)
			}
			stdout, stderr, code := runAs(personAtTerminal, action.args...)
			assertNoKeyMaterial(t, secrets, stdout, stderr)
			if code != ExitOK || !action.done(t, paths) {
				t.Fatalf("a person at a terminal: exit %d, done %v; want the action taken\n%s", code, action.done(t, paths), stderr)
			}
		})
	}
}

// A spend above the approval threshold, the fourth person-only action, is refused on every path but a person at a
// terminal in TestASpendAboveTheApprovalThresholdNeedsAPersonAndFundsNothing and here through the ledger it uses.
func TestASpendAboveTheThresholdIsRecordedOnlyOnceApproved(t *testing.T) {
	ledger := salewallet.Ledger{Dir: t.TempDir()}
	limits := salewallet.DefaultLimits()
	sale := common.HexToHash("0x5a1e")
	price := new(big.Int).Mul(big.NewInt(3), new(big.Int).Div(weiPerTAO, big.NewInt(10)))
	needs, err := ledger.Spend(limits, sale, price, false)
	if err != nil || !needs {
		t.Fatalf("Spend without approval = %v, %v; want approval needed", needs, err)
	}
	if spent, _ := ledger.Spent(); spent.Sign() != 0 {
		t.Fatalf("an unapproved spend was recorded: %s", spent)
	}
}

func TestNoSaleCommandPrintsKeyMaterial(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	chain := startSaleChain(t, ctx)
	paths, _ := personOnlyWallet(t, ctx, chain, true)
	secrets := keyMaterial(t, paths)
	commands := [][]string{
		{"sale", "wallet", "show"}, {"sale", "wallet", "backup"}, {"sale", "wallet", "limits"},
		{"sale", "wallet", "limits", "--per-sale", "0.5"}, {"sale", "status"}, {"sale", "withdraw"},
		{"sale", "wallet", "create"}, {"help", "sale"},
	}
	for _, mode := range [][]string{nil, {"--json"}, {"--verbose"}} {
		for _, command := range commands {
			stdout, stderr, _ := runAs(personPath{global: mode}, command...)
			assertNoKeyMaterial(t, secrets, stdout, stderr)
		}
	}
	// A malformed key on import is not echoed back, even with --verbose.
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	malformed := strings.Repeat("ab", 31) + "zz"
	app := newWebhookApp()
	app.stdin = strings.NewReader(malformed + "\n")
	_, stderr, code := runWebhookCLI(app, "--verbose", "sale", "wallet", "create", "--import-stdin")
	if code == ExitOK || strings.Contains(stderr, malformed) || strings.Contains(stderr, malformed[:20]) {
		t.Fatalf("import of a malformed key: exit %d, stderr %q", code, stderr)
	}
}

// Moving TAO out works whatever sale_escrow holds, so a wrong escrow address never strands the wallet's balance.
func TestAPersonCanEmptyTheWalletWhateverTheEscrowAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	chain := startSaleChain(t, ctx)
	_, address := personOnlyWallet(t, ctx, chain, true)
	t.Setenv("BEAM_SALE_ESCROW", address.Hex())
	stdout, stderr, code := runAs(personPath{}, "sale", "wallet", "show")
	if code != ExitOK || !strings.Contains(stdout, "10 TAO") || !strings.Contains(stderr, "Escrow credit is not shown") {
		t.Fatalf("wallet show: exit %d\n%s\n%s", code, stdout, stderr)
	}
	recipient := common.HexToAddress("0x00000000000000000000000000000000000Be4a2")
	if _, stderr, code := runAs(personAtTerminal, "sale", "wallet", "send", "--to", recipient.Hex(), "--all"); code != ExitOK || chain.balance(t, ctx, recipient).Sign() == 0 {
		t.Fatalf("wallet send: exit %d\n%s", code, stderr)
	}
}
