package command

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/Beam-Network/beam-cli-public/internal/config"
	"github.com/Beam-Network/beam-cli-public/internal/escrow"
	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/salestate"
	"github.com/Beam-Network/beam-cli-public/internal/salewallet"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

func (a *App) saleWallet(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	if len(args) == 0 {
		args = []string{"show"}
	}
	switch args[0] {
	case "create":
		return a.saleWalletCreate(args[1:], paths, renderer)
	case "show":
		return a.saleWalletShow(ctx, args[1:], cfg, paths, renderer)
	case "backup":
		return saleWalletBackup(args[1:], paths, renderer)
	case "limits":
		return a.saleWalletLimits(args[1:], paths, renderer)
	case "send":
		return a.saleWalletSend(ctx, args[1:], cfg, paths, renderer)
	case "remove":
		return a.saleWalletRemove(ctx, args[1:], cfg, paths, renderer)
	default:
		return usage(fmt.Sprintf("unknown sale wallet command %q", args[0]))
	}
}

// saleWalletCreate makes the wallet, or imports a key read from standard input. The key is never printed.
func (a *App) saleWalletCreate(args []string, paths config.Paths, renderer output.Renderer) error {
	boolFlags := map[string]bool{"--import-stdin": true}
	if err := validateOptions(args, nil, boolFlags); err != nil {
		return err
	}
	if positionals, err := positionalsForOptions(args, nil, boolFlags); err != nil || len(positionals) > 0 {
		return usage("sale wallet create takes no arguments")
	}
	var imported *ecdsa.PrivateKey
	if has(args, "--import-stdin") {
		key, err := a.readSaleKey(renderer)
		if err != nil {
			return err
		}
		imported = key
	}
	wallet, err := saleWalletStore(paths).Create(imported)
	if errors.Is(err, salewallet.ErrWalletExists) {
		return typedCLIError(ExitConflict, "sale_wallet_exists", "A sale wallet already exists.", "Show it with `beam sale wallet show`.", nil)
	}
	if err != nil {
		return cliError(ExitOperationFailed, "Could not create the sale wallet.", "", err)
	}
	result := map[string]any{"address": wallet.Address().Hex(), "key_store": wallet.Store, "limits": limitsView(wallet.Limits)}
	return renderer.Result(result, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "Created the sale wallet %s (keys in the %s).\nFund it with TAO for prices and gas before buying or selling.\n",
			wallet.Address().Hex(), keyStoreName(wallet.Store))
		return err
	})
}

func (a *App) readSaleKey(renderer output.Renderer) (*ecdsa.PrivateKey, error) {
	var data []byte
	var err error
	if a.stdinTerminal() {
		if _, err := fmt.Fprint(renderer.Err, "Private key (hex): "); err != nil {
			return nil, err
		}
		data, err = term.ReadPassword(int(os.Stdin.Fd()))
		_, _ = fmt.Fprintln(renderer.Err)
	} else {
		data, err = io.ReadAll(io.LimitReader(a.stdin, 1024))
	}
	if err != nil {
		return nil, cliError(ExitInterrupted, "Could not read the private key.", "Run the command again.", err)
	}
	key, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(string(data)), "0x"))
	if err != nil {
		return nil, typedCLIError(ExitUsage, "sale_wallet_key_invalid", "The private key must be 32 bytes of hex.", "Pipe the key alone into the command.", nil)
	}
	return key, nil
}

func keyStoreName(store string) string {
	if store == "keyring" {
		return "operating-system keyring"
	}
	return "owner-only wallet file"
}

func limitsView(l salewallet.Limits) map[string]any {
	return map[string]any{
		"per_sale_tao":            formatTAO(l.PerSale),
		"daily_tao":               formatTAO(l.Daily),
		"approve_above_tao":       formatTAO(l.ApproveAbove),
		"unfunded_delivery_usd":   fmt.Sprintf("%.2f", float64(l.UnfundedDeliveryCents)/100),
		"unfunded_delivery_cents": l.UnfundedDeliveryCents,
	}
}

func (a *App) saleWalletShow(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	if len(args) > 0 {
		return usage("sale wallet show takes no arguments")
	}
	wallet, err := loadSaleWallet(paths)
	if err != nil {
		return err
	}
	ledger := salewallet.Ledger{Dir: paths.Dir}
	spent, err := ledger.Spent()
	if err != nil {
		return cliError(ExitOperationFailed, "Could not read the sale ledger.", "", err)
	}
	unfunded, err := ledger.Unfunded()
	if err != nil {
		return cliError(ExitOperationFailed, "Could not read the sale ledger.", "", err)
	}
	result := map[string]any{
		"address": wallet.Address().Hex(), "key_store": wallet.Store, "limits": limitsView(wallet.Limits),
		"funded_24h_tao": formatTAO(spent), "unfunded_deliveries_24h_usd": fmt.Sprintf("%.2f", float64(unfunded)/100),
	}
	if wallet.Backup != nil {
		result["backup_address"] = wallet.BackupAddress().Hex()
	}
	balances := map[string]string{}
	network, netErr := a.dialSaleNetwork(ctx, cfg, renderer)
	if netErr == nil {
		for name, address := range map[string]common.Address{"balance_tao": wallet.Address(), "backup_balance_tao": wallet.BackupAddress()} {
			if address == (common.Address{}) {
				continue
			}
			if balance, err := network.Backend.BalanceAt(ctx, address, nil); err == nil {
				balances[name] = formatTAO(balance)
				result[name] = balances[name]
			}
		}
		if err := network.requireReleasedEscrow(ctx); err != nil {
			renderer.Progress("Escrow credit is not shown: " + asCLIError(err).Message)
		} else if credit, err := network.Contract.Credit(ctx, wallet.Address()); err == nil && credit.Sign() > 0 {
			result["escrow_credit_tao"] = formatTAO(credit)
		}
	}
	return renderer.Result(result, func(w io.Writer) error {
		var b strings.Builder
		fmt.Fprintf(&b, "Address         %s\n", wallet.Address().Hex())
		if v, ok := balances["balance_tao"]; ok {
			fmt.Fprintf(&b, "Balance         %s TAO\n", v)
		}
		if wallet.Backup != nil {
			fmt.Fprintf(&b, "Backup          %s", wallet.BackupAddress().Hex())
			if v, ok := balances["backup_balance_tao"]; ok {
				fmt.Fprintf(&b, " (%s TAO)", v)
			}
			b.WriteString("\n")
		}
		if v, ok := result["escrow_credit_tao"]; ok {
			fmt.Fprintf(&b, "Escrow credit   %s TAO; collect it with `beam sale withdraw`\n", v)
		}
		fmt.Fprintf(&b, "Keys            %s\n", keyStoreName(wallet.Store))
		fmt.Fprintf(&b, "Per sale        up to %s TAO\n", formatTAO(wallet.Limits.PerSale))
		fmt.Fprintf(&b, "Daily           %s of %s TAO funded in the last 24 hours\n", formatTAO(spent), formatTAO(wallet.Limits.Daily))
		fmt.Fprintf(&b, "Approval        a person confirms any sale above %s TAO\n", formatTAO(wallet.Limits.ApproveAbove))
		fmt.Fprintf(&b, "Delivery budget $%.2f of $%.2f spent on unfunded deliveries in the last 24 hours\n",
			float64(unfunded)/100, float64(wallet.Limits.UnfundedDeliveryCents)/100)
		if netErr != nil {
			b.WriteString("Balances are not shown: " + asCLIError(netErr).Message + "\n")
		}
		_, err := io.WriteString(w, b.String())
		return err
	})
}

func saleWalletBackup(args []string, paths config.Paths, renderer output.Renderer) error {
	if len(args) > 0 {
		return usage("sale wallet backup takes no arguments")
	}
	wallet, err := saleWalletStore(paths).AddBackup()
	if errors.Is(err, salewallet.ErrNoWallet) {
		return typedCLIError(ExitNotFound, "sale_wallet_missing", "No sale wallet exists yet.", "Create one with `beam sale wallet create`.", nil)
	}
	if err != nil {
		return cliError(ExitOperationFailed, "Could not add a backup account.", "", err)
	}
	return renderer.Result(map[string]any{"backup_address": wallet.BackupAddress().Hex()}, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "Added the backup account %s.\nGive it a little TAO for gas: it sends reveals, refunds and challenges when the main account is stuck.\n", wallet.BackupAddress().Hex())
		return err
	})
}

// saleWalletLimits shows or changes the spending limits. Lowering a limit needs nothing more; raising one needs a
// person's confirmation, so an agent can never widen its own allowance.
func (a *App) saleWalletLimits(args []string, paths config.Paths, renderer output.Renderer) error {
	valueFlags := map[string]bool{"--per-sale": true, "--daily": true, "--approve-above": true, "--delivery-budget": true}
	if err := validateOptions(args, valueFlags, nil); err != nil {
		return err
	}
	if positionals, err := positionalsForOptions(args, valueFlags, nil); err != nil || len(positionals) > 0 {
		return usage("sale wallet limits takes only options")
	}
	store := saleWalletStore(paths)
	wallet, err := loadSaleWallet(paths)
	if err != nil {
		return err
	}
	current := wallet.Limits
	next := salewallet.Limits{
		PerSale: new(big.Int).Set(current.PerSale), Daily: new(big.Int).Set(current.Daily),
		ApproveAbove: new(big.Int).Set(current.ApproveAbove), UnfundedDeliveryCents: current.UnfundedDeliveryCents,
	}
	changed := false
	for name, target := range map[string]*big.Int{"--per-sale": next.PerSale, "--daily": next.Daily, "--approve-above": next.ApproveAbove} {
		value, ok, err := flag(args, name)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		wei, err := parseTAO(value)
		if err != nil {
			return usage(name + ": " + err.Error())
		}
		target.Set(wei)
		changed = true
	}
	if value, ok, err := flag(args, "--delivery-budget"); err != nil {
		return err
	} else if ok {
		wei, err := parseTAO(value) // the same decimal rules, read as dollars
		if err != nil || new(big.Int).Rem(wei, big.NewInt(1e16)).Sign() != 0 {
			return usage("--delivery-budget must be a dollar amount with at most two decimal places")
		}
		next.UnfundedDeliveryCents = new(big.Int).Quo(wei, big.NewInt(1e16)).Int64()
		changed = true
	}
	if changed {
		raised := next.PerSale.Cmp(current.PerSale) > 0 || next.Daily.Cmp(current.Daily) > 0 ||
			next.ApproveAbove.Cmp(current.ApproveAbove) > 0 || next.UnfundedDeliveryCents > current.UnfundedDeliveryCents
		if raised {
			summary := fmt.Sprintf("Raise the sale wallet's limits to %s TAO per sale, %s TAO a day, approval above %s TAO and a $%.2f daily delivery budget?",
				formatTAO(next.PerSale), formatTAO(next.Daily), formatTAO(next.ApproveAbove), float64(next.UnfundedDeliveryCents)/100)
			if err := a.confirmByPerson(renderer, summary, "Raise the limits?"); err != nil {
				return err
			}
		}
		if wallet, err = store.SetLimits(next); err != nil {
			return cliError(ExitOperationFailed, "Could not save the limits.", "", err)
		}
	}
	return renderer.Result(map[string]any{"limits": limitsView(wallet.Limits), "changed": changed}, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "Per sale        %s TAO\nDaily           %s TAO\nApproval above  %s TAO\nDelivery budget $%.2f a day for buyers who have not funded\n",
			formatTAO(wallet.Limits.PerSale), formatTAO(wallet.Limits.Daily), formatTAO(wallet.Limits.ApproveAbove), float64(wallet.Limits.UnfundedDeliveryCents)/100)
		return err
	})
}

// saleWalletSend moves TAO out of the sale wallet to an ordinary account, after a person confirms it.
func (a *App) saleWalletSend(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	if err := validateOptions(args, map[string]bool{"--to": true, "--amount": true}, map[string]bool{"--all": true}); err != nil {
		return err
	}
	to, err := requiredFlag(args, "--to")
	if err != nil {
		return err
	}
	if !common.IsHexAddress(to) {
		return usage("--to must be a 0x-prefixed address")
	}
	amountText, hasAmount, err := flag(args, "--amount")
	if err != nil {
		return err
	}
	if hasAmount == has(args, "--all") {
		return usage("pass exactly one of --amount <TAO> or --all")
	}
	s, err := a.openWalletSession(ctx, cfg, paths, renderer)
	if err != nil {
		return err
	}
	price, err := s.net.Backend.SuggestGasPrice(ctx)
	if err != nil {
		return saleTxError("read the gas price", err)
	}
	// The fee may be raised while the transfer waits, so twice the current price is set aside and is the cap.
	maxPrice := new(big.Int).Mul(price, big.NewInt(2))
	reserve := new(big.Int).Mul(maxPrice, big.NewInt(gasTransfer))
	balance, err := s.net.Backend.BalanceAt(ctx, s.wallet.Address(), nil)
	if err != nil {
		return saleTxError("read the wallet balance", err)
	}
	var amount *big.Int
	if hasAmount {
		if amount, err = parseTAO(amountText); err != nil || amount.Sign() == 0 {
			return usage("--amount must be a positive TAO amount")
		}
	} else {
		amount = new(big.Int).Sub(balance, reserve)
	}
	if amount.Sign() <= 0 || new(big.Int).Add(amount, reserve).Cmp(balance) > 0 {
		return typedCLIError(ExitOperationFailed, "sale_wallet_insufficient", fmt.Sprintf("The wallet holds %s TAO, not enough to send that and pay for gas.", formatTAO(balance)), "", nil)
	}
	target := common.HexToAddress(to)
	if err := a.confirmByPerson(renderer, fmt.Sprintf("Send %s TAO from the sale wallet to %s?", formatTAO(amount), target.Hex()), "Send it?"); err != nil {
		return err
	}
	sender := s.sender(s.wallet.Key)
	sender.MaxGasPrice = maxPrice
	out, err := sender.Send(ctx, escrow.Request{Label: "send", To: target, Value: amount, GasLimit: gasTransfer})
	if err != nil {
		return saleTxError("send the TAO", err)
	}
	return renderer.Result(map[string]any{"to": target.Hex(), "amount_tao": formatTAO(amount), "transaction": out.Receipt.TxHash.Hex()}, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "Sent %s TAO to %s in %s.\n", formatTAO(amount), target.Hex(), out.Receipt.TxHash.Hex())
		return err
	})
}

// saleWalletRemove deletes the wallet's keys for good. It refuses while the wallet holds TAO or takes part in a sale
// that has not ended, and a person must confirm it.
func (a *App) saleWalletRemove(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	if len(args) > 0 {
		return usage("sale wallet remove takes no arguments")
	}
	wallet, err := loadSaleWallet(paths)
	if err != nil {
		return err
	}
	sales, err := salestate.Store{Dir: paths.Dir}.List()
	if err != nil {
		return cliError(ExitOperationFailed, "Could not read the sale records.", "", err)
	}
	for _, sale := range sales {
		if !sale.Ended() {
			return typedCLIError(ExitConflict, "sale_in_progress", "The sale "+shortSaleID(sale.SaleID)+" has not ended.",
				"Finish it with `beam sale resume "+shortSaleID(sale.SaleID)+"` first.", nil)
		}
	}
	checked := "The balance could not be checked."
	if network, err := a.dialSaleNetwork(ctx, cfg, renderer); err == nil {
		checked = "The wallet is empty."
		for _, address := range []common.Address{wallet.Address(), wallet.BackupAddress()} {
			if address == (common.Address{}) {
				continue
			}
			balance, err := network.Backend.BalanceAt(ctx, address, nil)
			if err != nil {
				checked = "The balance could not be checked."
				break
			}
			if balance.Sign() > 0 {
				return typedCLIError(ExitConflict, "sale_wallet_not_empty", fmt.Sprintf("%s still holds %s TAO.", address.Hex(), formatTAO(balance)),
					"Move it out with `beam sale wallet send` first.", nil)
			}
		}
	}
	if err := a.confirmByPerson(renderer, "Delete the sale wallet "+wallet.Address().Hex()+"? Its keys cannot be recovered. "+checked, "Delete it?"); err != nil {
		return err
	}
	if err := saleWalletStore(paths).Remove(); err != nil {
		return cliError(ExitOperationFailed, "Could not remove the sale wallet.", "", err)
	}
	return renderer.Result(map[string]any{"removed": wallet.Address().Hex()}, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "Removed the sale wallet %s.\n", wallet.Address().Hex())
		return err
	})
}
