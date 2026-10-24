package command

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/auth"
	"github.com/Beam-Network/beam-cli-public/internal/config"
	"github.com/Beam-Network/beam-cli-public/internal/datasale"
	"github.com/Beam-Network/beam-cli-public/internal/escrow"
	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/salestate"
	"github.com/Beam-Network/beam-cli-public/internal/salewallet"
	"github.com/Beam-Network/beam-cli-public/internal/version"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"
)

// saleSeams are the settings tests shorten. poll is how often a sale checks the chain and its Room delivery, and margin
// how many blocks before a deadline a reveal or challenge must go out; zero means the defaults. finalized returns the
// chain's finalized block; nil reads it from the endpoint. Tests use the latest block, since a local chain finalizes far
// behind it.
type saleSeams struct {
	poll      time.Duration
	margin    uint64
	finalized func(context.Context, escrow.Backend) (uint64, error)
}

// The client's rules for every sale, from the v1 proposal. The contract enforces its own floors and cap; these are
// what the client adds.
const (
	// maxSaleBytes is the largest dataset the client sells or buys: 100 GB.
	maxSaleBytes = 100_000_000_000
	// minSaleUSDCents is the smallest price a seller may ask, in US cents at the current TAO/USD quote.
	minSaleUSDCents = 1000
	// The default periods, in blocks: 6 hours to reveal, 24 hours to challenge, and 6 hours to fund once the
	// delivery window has closed.
	defaultRevealPeriod    = 1800
	defaultChallengePeriod = 7200
	defaultFundWindow      = 1800
	// maxSalePeriod is the contract's longest reveal or challenge period.
	maxSalePeriod = 216_000
	// defaultSaleMargin is how many blocks before a deadline a reveal or challenge must be broadcast.
	defaultSaleMargin = 100
	// fundMargin is how many blocks before the funding deadline the buyer stops trying to fund.
	fundMargin = 5
	// saleBlockTime is the chain's block time, used only to estimate when a block arrives.
	saleBlockTime = 12 * time.Second
)

// Gas limits, set well above what each call measured on testnet with the frozen contract: fund 86,978, reveal
// 36,641, settle 40,090, revoke 48,753, and a challenge on a 100 GB file 175,692.
const (
	gasFund      = 200_000
	gasReveal    = 120_000
	gasSettle    = 120_000
	gasRefund    = 120_000
	gasRevoke    = 120_000
	gasChallenge = 400_000
	gasWithdraw  = 100_000
	gasTransfer  = 21_000
)

// maxSaleGasPrice caps what one unit of gas may cost when the sender raises the fee of a stalled transaction:
// 1,000 gwei, two hundred times the price measured on Bittensor EVM.
var maxSaleGasPrice = big.NewInt(1_000_000_000_000)

var weiPerTAO = big.NewInt(1e18)

// newSaleKeyring is the keyring the sale wallet keeps its keys in. Tests replace it with an in-memory keyring, so no
// test reads or writes the developer's keychain.
var newSaleKeyring = func(string) auth.Keyring { return auth.SystemKeyring() }

func saleWalletStore(paths config.Paths) salewallet.Store {
	return salewallet.Store{Dir: paths.Dir, Namespace: version.StateNamespace(), Keyring: newSaleKeyring(paths.Dir)}
}

func (a *App) saleCommand(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		return renderNamedHelp("sale", renderer)
	}
	switch args[0] {
	case "wallet":
		return a.saleWallet(ctx, args[1:], cfg, paths, renderer)
	case "room":
		return a.saleRoomCreate(ctx, args[1:], cfg, paths, renderer)
	case "join":
		return a.saleJoin(ctx, args[1:], cfg, paths, renderer)
	case "offer":
		return a.saleOffer(ctx, args[1:], cfg, paths, renderer)
	case "buy":
		return a.saleBuy(ctx, args[1:], cfg, paths, renderer)
	case "status":
		return a.saleStatus(ctx, args[1:], cfg, paths, renderer)
	case "resume":
		return a.saleResume(ctx, args[1:], cfg, paths, renderer)
	case "revoke":
		return a.saleRevoke(ctx, args[1:], cfg, paths, renderer)
	case "withdraw":
		return a.saleWithdraw(ctx, args[1:], cfg, paths, renderer)
	default:
		return usage(fmt.Sprintf("unknown sale command %q", args[0]))
	}
}

// saleNetwork is the configured escrow deployment and the RPC endpoints that reach it.
type saleNetwork struct {
	Domain   datasale.Domain
	Backend  escrow.Backend
	Contract escrow.Contract
}

func (a *App) dialSaleNetwork(ctx context.Context, cfg config.Config, renderer output.Renderer) (*saleNetwork, error) {
	if cfg.SaleChainID == "" || cfg.SaleRPCURLs == "" || cfg.SaleEscrow == "" {
		return nil, typedCLIError(ExitConfig, "sale_network_unconfigured", "No data-sale escrow is configured for this context.",
			"Set sale_chain_id, sale_rpc_urls and sale_escrow with `beam config set`.", nil)
	}
	chainID, ok := new(big.Int).SetString(cfg.SaleChainID, 10)
	if !ok || chainID.Sign() <= 0 {
		return nil, typedCLIError(ExitConfig, "sale_network_invalid", "sale_chain_id must be a positive integer.", "Fix it with `beam config set sale_chain_id <id>`.", nil)
	}
	if !common.IsHexAddress(cfg.SaleEscrow) {
		return nil, typedCLIError(ExitConfig, "sale_network_invalid", "sale_escrow must be a 0x-prefixed contract address.", "Fix it with `beam config set sale_escrow <address>`.", nil)
	}
	var urls []string
	for _, url := range strings.Split(cfg.SaleRPCURLs, ",") {
		if url = strings.TrimSpace(url); url != "" {
			urls = append(urls, url)
		}
	}
	backend, err := escrow.Dial(ctx, chainID, 15*time.Second, urls...)
	if err != nil {
		return nil, typedCLIError(ExitConfig, "sale_network_invalid", "The configured RPC endpoints cannot serve the sale escrow.", "Check sale_rpc_urls and sale_chain_id.", err)
	}
	backend.OnRest = func(wait time.Duration) {
		renderer.Progress(fmt.Sprintf("The RPC endpoint asked this client to slow down; waiting %s.", wait.Round(time.Second)))
	}
	address := common.HexToAddress(cfg.SaleEscrow)
	return &saleNetwork{
		Domain:   datasale.Domain{ChainID: chainID, Escrow: address},
		Backend:  backend,
		Contract: escrow.Contract{Address: address, Backend: backend},
	}, nil
}

// requireReleasedEscrow refuses an escrow address whose code is not the released build, so no command signs for,
// delivers to, funds or settles through a contract with other rules.
func (n *saleNetwork) requireReleasedEscrow(ctx context.Context) error {
	err := escrow.VerifyRelease(ctx, n.Backend, n.Domain.Escrow)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, escrow.ErrUnknownEscrow):
		return typedCLIError(ExitConfig, "sale_escrow_unverified", "The code at "+n.Domain.Escrow.Hex()+" is not the DataSaleEscrow v1 build.",
			"Use the escrow this build defaults to with `beam config unset sale_escrow`.", err)
	default:
		return saleTxError("read the escrow's code", err)
	}
}

func loadSaleWallet(paths config.Paths) (salewallet.Wallet, error) {
	wallet, err := saleWalletStore(paths).Load()
	if errors.Is(err, salewallet.ErrNoWallet) {
		return salewallet.Wallet{}, typedCLIError(ExitNotFound, "sale_wallet_missing", "No sale wallet exists yet.", "Create one with `beam sale wallet create`.", nil)
	}
	if err != nil {
		return salewallet.Wallet{}, typedCLIError(ExitConfig, "sale_wallet_unreadable", "Could not read the sale wallet.", "Check the operating-system keyring.", err)
	}
	return wallet, nil
}

// saleSession is one command's view of the sales on this machine: the network, the wallet, the records and the
// ledger.
type saleSession struct {
	app      *App
	cfg      config.Config
	paths    config.Paths
	renderer output.Renderer
	net      *saleNetwork
	wallet   salewallet.Wallet
	state    salestate.Store
	ledger   salewallet.Ledger
}

// openSaleSession opens a session for a command that uses the escrow, which must be the released build.
func (a *App) openSaleSession(ctx context.Context, cfg config.Config, paths config.Paths, renderer output.Renderer) (*saleSession, error) {
	s, err := a.openWalletSession(ctx, cfg, paths, renderer)
	if err != nil {
		return nil, err
	}
	if err := s.net.requireReleasedEscrow(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// openWalletSession opens a session for a command that only moves the wallet's own TAO, which works whatever the
// configured escrow address holds.
func (a *App) openWalletSession(ctx context.Context, cfg config.Config, paths config.Paths, renderer output.Renderer) (*saleSession, error) {
	wallet, err := loadSaleWallet(paths)
	if err != nil {
		return nil, err
	}
	network, err := a.dialSaleNetwork(ctx, cfg, renderer)
	if err != nil {
		return nil, err
	}
	return &saleSession{
		app: a, cfg: cfg, paths: paths, renderer: renderer, net: network, wallet: wallet,
		state:  salestate.Store{Dir: paths.Dir},
		ledger: salewallet.Ledger{Dir: paths.Dir},
	}, nil
}

// salePollInterval is how often a sale checks the chain: once a block, which keeps two clients sharing an address
// well under public endpoints' rate limits.
func (a *App) salePollInterval() time.Duration {
	if a.sales.poll > 0 {
		return a.sales.poll
	}
	return saleBlockTime
}

func (a *App) saleSafetyMargin() uint64 {
	if a.sales.margin > 0 {
		return a.sales.margin
	}
	return defaultSaleMargin
}

func (a *App) saleSleep(ctx context.Context) error {
	t := time.NewTimer(a.salePollInterval())
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// chainState reads the sale's escrow record and the current block, waiting out endpoints that do not answer.
func (s *saleSession) chainState(ctx context.Context, saleID common.Hash) (escrow.Record, uint64, error) {
	warned := false
	for {
		head, err := s.net.Backend.BlockNumber(ctx)
		if err == nil {
			// The record is read as of that block, so the two agree whichever endpoint answers each.
			var record escrow.Record
			if record, err = s.net.Contract.RecordAt(ctx, saleID, new(big.Int).SetUint64(head)); err == nil {
				return record, head, nil
			}
		}
		if !errors.Is(err, escrow.ErrUnreachable) {
			return escrow.Record{}, 0, saleTxError("read the sale", err)
		}
		if !warned {
			s.renderer.Progress("No RPC endpoint answered; retrying.")
			warned = true
		}
		if err := s.app.saleSleep(ctx); err != nil {
			return escrow.Record{}, 0, err
		}
	}
}

// act runs one step of a driver. An error that only means no RPC endpoint answered, or that another of this wallet's
// transactions is still pending, is waited out, because the step may have a deadline and the cause may clear before
// it; the step then runs again, with every check redone. Any other error ends the drive.
func (s *saleSession) act(ctx context.Context, say func(string), step func() error) error {
	err := step()
	switch {
	case err == nil:
		return nil
	case errors.Is(err, escrow.ErrUnreachable):
		say("No RPC endpoint answered; retrying.")
	case errors.Is(err, escrow.ErrAccountBusy):
		say("Another transaction of the sale wallet is pending; waiting for it.")
	default:
		return err
	}
	return s.app.saleSleep(ctx)
}

// finalRecord reads a sale's record as of the chain's finalized block.
func (s *saleSession) finalRecord(ctx context.Context, saleID common.Hash) (escrow.Record, error) {
	final := s.app.sales.finalized
	if final == nil {
		final = finalizedBlock
	}
	block, err := final(ctx, s.net.Backend)
	if err != nil {
		return escrow.Record{}, err
	}
	return s.net.Contract.RecordAt(ctx, saleID, new(big.Int).SetUint64(block))
}

// finalizedBlock is the chain's finalized block, which no reorganization can remove.
func finalizedBlock(ctx context.Context, backend escrow.Backend) (uint64, error) {
	header, err := backend.HeaderByNumber(ctx, big.NewInt(int64(rpc.FinalizedBlockNumber)))
	if err != nil {
		return 0, err
	}
	return header.Number.Uint64(), nil
}

// sender sends from primary, falling back to the backups for calls anyone may make.
func (s *saleSession) sender(primary *ecdsa.PrivateKey, backups ...*ecdsa.PrivateKey) *escrow.Sender {
	sender := &escrow.Sender{
		Backend: s.net.Backend, ChainID: s.net.Domain.ChainID, Primary: escrow.Account{Key: primary},
		Poll: s.app.salePollInterval() / 2, MaxGasPrice: maxSaleGasPrice,
	}
	for _, b := range backups {
		sender.Backups = append(sender.Backups, escrow.Account{Key: b})
	}
	return sender
}

// gasKeys returns the wallet's accounts that can pay gasLimit at the current price twice over, the main account
// first. A call anyone may make can be sent from any of them, so an account without gas is left out rather than
// tried.
func (s *saleSession) gasKeys(ctx context.Context, gasLimit uint64, value *big.Int) ([]*ecdsa.PrivateKey, *big.Int, error) {
	price, err := s.net.Backend.SuggestGasPrice(ctx)
	if err != nil {
		return nil, nil, saleTxError("read the gas price", err)
	}
	need := new(big.Int).Mul(price, new(big.Int).SetUint64(2*gasLimit))
	if value != nil {
		need.Add(need, value)
	}
	var keys []*ecdsa.PrivateKey
	for _, key := range []*ecdsa.PrivateKey{s.wallet.Key, s.wallet.Backup} {
		if key == nil {
			continue
		}
		balance, err := s.net.Backend.BalanceAt(ctx, crypto.PubkeyToAddress(key.PublicKey), nil)
		if err != nil {
			return nil, nil, saleTxError("read the wallet balance", err)
		}
		if balance.Cmp(need) >= 0 {
			keys = append(keys, key)
		}
	}
	return keys, need, nil
}

// transact gets one escrow call included for the sale. It resumes the transactions an interrupted run broadcast for
// the same action before it sends a new one, and records each attempt as it is broadcast.
func (s *saleSession) transact(ctx context.Context, sale *salestate.Sale, sender *escrow.Sender, action string, req escrow.Request) (escrow.Outcome, error) {
	req.Label = action
	req.To = s.net.Domain.Escrow
	req.OnAttempt = func(attempt escrow.Attempt) {
		if err := s.state.AddAttempt(sale, action, attempt); err != nil {
			s.renderer.Progress("Could not record the " + action + " transaction " + attempt.Hash.Hex() + ": " + err.Error())
		}
	}
	if prior := sale.Transactions[action]; len(prior) > 0 {
		out, err := sender.Resume(ctx, req, prior)
		if !errors.Is(err, escrow.ErrReverted) {
			return out, err
		}
		// Every earlier attempt was included and reverted; they are kept apart and a new transaction is sent.
		sale.Transactions[action+".reverted"] = append(sale.Transactions[action+".reverted"], prior...)
		delete(sale.Transactions, action)
		if err := s.state.Save(sale); err != nil {
			return out, err
		}
	}
	return sender.Send(ctx, req)
}

// saleTxError explains a failed escrow call. It never repeats the call's data, which can hold a sale key.
func saleTxError(action string, err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return err
	case errors.Is(err, escrow.ErrTooLate):
		return typedCLIError(ExitOperationFailed, "sale_too_late", "It is too close to the deadline to "+action+".", "", err)
	case errors.Is(err, escrow.ErrWouldRevert):
		return typedCLIError(ExitOperationFailed, "sale_would_revert", "The escrow would refuse to "+action+".", "Run `beam sale status` to see the sale's state.", err)
	case errors.Is(err, escrow.ErrReverted):
		return typedCLIError(ExitOperationFailed, "sale_reverted", "The escrow refused to "+action+".", "Run `beam sale status` to see the sale's state.", err)
	case errors.Is(err, escrow.ErrDeadlinePassed):
		return typedCLIError(ExitOperationFailed, "sale_deadline_passed", "The deadline passed before the transaction to "+action+" was included.", "Run `beam sale status` to see the sale's state.", err)
	case errors.Is(err, escrow.ErrAccountBusy):
		return typedCLIError(ExitOperationFailed, "sale_account_busy", "The sale wallet has pending transactions this client did not send.",
			"Wait for them to be included. A backup account (`beam sale wallet backup`) lets reveals and refunds go out meanwhile.", err)
	case errors.Is(err, escrow.ErrUnreachable):
		return typedCLIError(ExitOperationFailed, "sale_rpc_unreachable", "No RPC endpoint answered.", "Check sale_rpc_urls, or add another endpoint to it.", err)
	default:
		return typedCLIError(ExitOperationFailed, "sale_transaction_failed", "Could not "+action+".", "Run `beam sale resume` to retry.", err)
	}
}

// confirmByPerson asks the person at the terminal to confirm. It never accepts a flag instead: a spend above the
// approval threshold, a raised limit and moving funds out must be confirmed by a person, never by an agent.
func (a *App) confirmByPerson(renderer output.Renderer, summary, question string) error {
	renderer.Progress(summary)
	if !renderer.Interactive || !a.stdinTerminal() {
		return typedCLIError(ExitOperationFailed, "sale_approval_required", "This needs a person's confirmation at a terminal.",
			"Run the command in an interactive terminal.", nil)
	}
	accepted, err := a.promptYesNo(renderer, question, false)
	if err != nil {
		return err
	}
	if !accepted {
		return cliError(ExitInterrupted, "Not confirmed.", "", nil)
	}
	return nil
}

var decimalPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

// saleDatasetName shows a seller-chosen dataset name safely: quoted, with every non-graphic character escaped and cut
// to 64 characters, so it cannot move the cursor, reorder the text around it, or push the price out of a prompt.
func saleDatasetName(name string) string {
	if name == "" {
		return "an unnamed dataset"
	}
	if runes := []rune(name); len(runes) > 64 {
		name = string(runes[:64]) + "…"
	}
	return strconv.QuoteToGraphic(name)
}

// parseTAO reads a decimal TAO amount into wei.
func parseTAO(value string) (*big.Int, error) {
	value = strings.TrimSpace(value)
	r, ok := new(big.Rat).SetString(value)
	if !decimalPattern.MatchString(value) || !ok {
		return nil, fmt.Errorf("%q is not a TAO amount", value)
	}
	r.Mul(r, new(big.Rat).SetInt(weiPerTAO))
	if !r.IsInt() {
		return nil, fmt.Errorf("%q has more than 18 decimal places", value)
	}
	return new(big.Int).Set(r.Num()), nil
}

// formatTAO renders wei as a decimal TAO amount.
func formatTAO(wei *big.Int) string {
	if wei == nil {
		return "0"
	}
	s := new(big.Rat).SetFrac(wei, weiPerTAO).FloatString(18)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// taoUSDQuoteURL serves the TAO/USD price the $10 floor is checked against. The request carries no data about the
// sale or the wallet.
var taoUSDQuoteURL = "https://api.coingecko.com/api/v3/simple/price?ids=bittensor&vs_currencies=usd"

// taoUSD returns the TAO/USD price: the one given with --tao-usd, or a quote fetched from taoUSDQuoteURL.
func taoUSD(ctx context.Context, given string) (*big.Rat, error) {
	if given != "" {
		r, ok := new(big.Rat).SetString(strings.TrimSpace(given))
		if !decimalPattern.MatchString(strings.TrimSpace(given)) || !ok || r.Sign() <= 0 {
			return nil, usage("--tao-usd must be a positive decimal price")
		}
		return r, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, taoUSDQuoteURL, nil)
	if err != nil {
		return nil, err
	}
	fail := func(cause error) error {
		return typedCLIError(ExitOperationFailed, "sale_quote_unavailable", "Could not get a TAO/USD quote for the $10 minimum.",
			"Retry, or give the current price with --tao-usd.", cause)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fail(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fail(errors.New(resp.Status))
	}
	var quote map[string]map[string]json.Number
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&quote); err != nil {
		return nil, fail(err)
	}
	r, ok := new(big.Rat).SetString(quote["bittensor"]["usd"].String())
	if !ok || r.Sign() <= 0 {
		return nil, fail(errors.New("the quote has no TAO price"))
	}
	return r, nil
}

// usdCents converts wei to US cents at usdPerTAO, rounding down.
func usdCents(wei *big.Int, usdPerTAO *big.Rat) int64 {
	r := new(big.Rat).SetFrac(wei, weiPerTAO)
	r.Mul(r, usdPerTAO)
	r.Mul(r, big.NewRat(100, 1))
	return new(big.Int).Quo(r.Num(), r.Denom()).Int64()
}

// saleDeliveryCents estimates what delivering a package of packageBytes costs the seller's organization: the Room
// transfer at 0.2 credit plus 0.02 credit per started GB, and the Room's 1-credit start on its first delivery, at
// $0.50 a credit. With signed offers the seller spends no gas before it is paid.
func saleDeliveryCents(packageBytes int64, firstInRoom bool) int64 {
	cents := 10 + (packageBytes+999_999_999)/1_000_000_000
	if firstInRoom {
		cents += 50
	}
	return cents
}

// blocksAway renders how long until block target arrives, from block head.
func blocksAway(head, target uint64) string {
	if target <= head {
		return "now"
	}
	d := time.Duration(target-head) * saleBlockTime
	switch {
	case d < 90*time.Minute:
		return fmt.Sprintf("in about %d min", int((d+time.Minute-1)/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("in about %d h", int((d+30*time.Minute)/time.Hour))
	default:
		return fmt.Sprintf("in about %d days", int((d+12*time.Hour)/(24*time.Hour)))
	}
}

// parseBlocks reads a block count flag.
func parseBlocks(args []string, name string, fallback uint64) (uint64, error) {
	value, ok, err := flag(args, name)
	if err != nil || !ok {
		return fallback, err
	}
	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil || n == 0 {
		return 0, usage(name + " must be a positive number of blocks")
	}
	return n, nil
}

func shortSaleID(id common.Hash) string { return strings.TrimPrefix(id.Hex(), "0x")[:12] }
