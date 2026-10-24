package command

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/Beam-Network/beam-cli-public/internal/config"
	"github.com/Beam-Network/beam-cli-public/internal/escrow"
	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/salestate"
)

// saleReport is what a sale command reports about one sale.
type saleReport struct {
	SaleID      string `json:"sale_id"`
	Role        string `json:"role"`
	Name        string `json:"name,omitempty"`
	PriceTAO    string `json:"price_tao"`
	SizeBytes   uint64 `json:"size_bytes"`
	Counterpart string `json:"counterpart"`
	RoomID      string `json:"room_id"`
	// EscrowState is the escrow's state, or empty when the chain could not be read.
	EscrowState string `json:"escrow_state,omitempty"`
	Phase       string `json:"phase"`
	Outcome     string `json:"outcome,omitempty"`
	Block       uint64 `json:"block,omitempty"`
	Next        string `json:"next,omitempty"`
	// NextBlock is the block the next step is due at or must happen by.
	NextBlock  uint64 `json:"next_block,omitempty"`
	Output     string `json:"output,omitempty"`
	Mismatches int    `json:"mismatching_leaves_listed,omitempty"`
}

// saleReportOf describes a sale and its next step. record is nil when the chain could not be read.
func saleReportOf(sale salestate.Sale, record *escrow.Record, head uint64) saleReport {
	offer := sale.Header.Offer
	r := saleReport{
		SaleID: sale.SaleID.Hex(), Role: sale.Role, Name: sale.Header.Name, PriceTAO: formatTAO(offer.Price), SizeBytes: offer.SizeBytes,
		RoomID: sale.RoomID, Phase: sale.Phase, Outcome: sale.Outcome, Output: sale.Output, Mismatches: len(sale.Mismatches),
	}
	r.Counterpart = offer.Buyer.Hex()
	if sale.Role == salestate.Buyer {
		r.Counterpart = offer.Seller.Hex()
	}
	resume := "run `beam sale resume " + shortSaleID(sale.SaleID) + "`"
	if record == nil {
		if sale.Ended() {
			r.Next = "ended: " + sale.Outcome
		}
		return r
	}
	r.EscrowState, r.Block = record.State.String(), head
	switch {
	case sale.Ended():
		r.Next = "ended: " + sale.Outcome
	case record.State == escrow.StateNone && head > offer.FundDeadline:
		r.Next = "the offer expired unfunded; " + resume + " to close it"
	case record.State == escrow.StateNone && sale.Role == salestate.Seller && sale.Phase == salestate.PhaseDeliveryFailed:
		r.Next, r.NextBlock = "the delivery failed; the buyer can fund only if it received the package", offer.FundDeadline
	case record.State == escrow.StateNone && sale.Role == salestate.Seller:
		r.Next, r.NextBlock = "waiting for the buyer to fund; "+resume+" --wait to reveal the key as soon as it does", offer.FundDeadline
	case record.State == escrow.StateNone:
		r.Next, r.NextBlock = "fund the sale: "+resume, offer.FundDeadline
	case record.State == escrow.StateFunded && sale.Role == salestate.Seller:
		r.Next, r.NextBlock = "reveal the key now: "+resume, record.Deadline
	case record.State == escrow.StateFunded && head > record.Deadline:
		r.Next = "the seller did not reveal the key; reclaim the payment: " + resume
	case record.State == escrow.StateFunded:
		r.Next, r.NextBlock = "waiting for the seller to reveal the key; "+resume+" --wait to decrypt as soon as it does", record.Deadline
	case record.State == escrow.StateKeyReleased && sale.Role == salestate.Seller && head > record.Deadline:
		r.Next = "settle the sale: " + resume
	case record.State == escrow.StateKeyReleased && sale.Role == salestate.Seller:
		r.Next, r.NextBlock = "settle after the challenge window: "+resume, record.Deadline+1
	case record.State == escrow.StateKeyReleased && sale.Phase == salestate.PhaseVerified:
		r.Next = "nothing: every leaf matches, and the seller settles after the challenge window"
	case record.State == escrow.StateKeyReleased && sale.Phase == salestate.PhaseMismatch && head > record.Deadline:
		r.Next = "the challenge window has closed"
	case record.State == escrow.StateKeyReleased:
		r.Next, r.NextBlock = "decrypt and check the dataset, and challenge a bad leaf: "+resume, record.Deadline
	default:
		r.Next = "the escrow is " + record.State.String() + "; " + resume + " to record it"
	}
	return r
}

func renderSaleReport(renderer output.Renderer, report saleReport) error {
	return renderer.Result(report, func(w io.Writer) error {
		var b strings.Builder
		fmt.Fprintf(&b, "Sale      %s\n", report.SaleID)
		fmt.Fprintf(&b, "Role      %s, with %s\n", report.Role, report.Counterpart)
		if report.Name != "" {
			fmt.Fprintf(&b, "Dataset   %s, %d bytes\n", saleDatasetName(report.Name), report.SizeBytes)
		}
		fmt.Fprintf(&b, "Price     %s TAO\n", report.PriceTAO)
		if report.EscrowState != "" {
			fmt.Fprintf(&b, "Escrow    %s at block %d\n", report.EscrowState, report.Block)
		}
		if report.Output != "" {
			fmt.Fprintf(&b, "Output    %s\n", report.Output)
		}
		if report.Next != "" {
			next := report.Next
			if report.NextBlock > 0 && report.Block > 0 {
				next += fmt.Sprintf(" (block %d, %s)", report.NextBlock, blocksAway(report.Block, report.NextBlock))
			}
			fmt.Fprintf(&b, "Next      %s\n", next)
		}
		_, err := io.WriteString(w, b.String())
		return err
	})
}

// saleStatus lists the sales on this machine, or shows one, with each one's escrow state and next step.
func (a *App) saleStatus(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	if len(args) > 1 {
		return usage("sale status takes at most one SALE_ID")
	}
	store := salestate.Store{Dir: paths.Dir}
	var sales []salestate.Sale
	if len(args) == 1 {
		sale, err := store.Find(args[0])
		if err != nil {
			return typedCLIError(ExitNotFound, "sale_unknown", "No sale "+args[0]+" on this machine.", "List them with `beam sale status`.", err)
		}
		sales = append(sales, sale)
	} else {
		list, err := store.List()
		if err != nil {
			return cliError(ExitOperationFailed, "Could not read the sale records.", "", err)
		}
		sales = list
	}
	// The chain is read once for the current block and once per sale that has not ended; an ended sale's record says
	// all there is.
	network, netErr := a.dialSaleNetwork(ctx, cfg, renderer)
	var head uint64
	if netErr == nil {
		if netErr = network.requireReleasedEscrow(ctx); netErr == nil {
			if head, netErr = network.Backend.BlockNumber(ctx); netErr != nil {
				netErr = saleTxError("read the chain", netErr)
			}
		}
		if netErr != nil {
			renderer.Progress("Escrow states are not shown: " + asCLIError(netErr).Message)
		}
	}
	reports := make([]saleReport, 0, len(sales))
	for _, sale := range sales {
		var record *escrow.Record
		if netErr == nil && !sale.Ended() && sale.ChainID != nil && sale.ChainID.Cmp(network.Domain.ChainID) == 0 &&
			sale.Escrow == network.Domain.Escrow {
			if r, err := network.Contract.Record(ctx, sale.SaleID); err == nil {
				record = &r
			}
		}
		reports = append(reports, saleReportOf(sale, record, head))
	}
	if len(args) == 1 {
		return renderSaleReport(renderer, reports[0])
	}
	return renderer.Result(map[string]any{"sales": reports}, func(w io.Writer) error {
		if len(reports) == 0 {
			_, err := fmt.Fprintln(w, "No sales on this machine.")
			return err
		}
		rows := make([][]string, 0, len(reports))
		for _, r := range reports {
			rows = append(rows, []string{strings.TrimPrefix(r.SaleID, "0x")[:12], r.Role, r.PriceTAO, firstNonEmpty(r.EscrowState, r.Outcome, "unknown"), r.Next})
		}
		return renderer.Table(w, output.Table{Headers: []string{"SALE", "ROLE", "TAO", "ESCROW", "NEXT"}, Rows: rows})
	})
}

// saleResume carries a sale forward from wherever it stands. With --wait it keeps going until nothing is left for
// this party to do.
func (a *App) saleResume(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	boolFlags := map[string]bool{"--wait": true}
	if err := validateOptions(args, nil, boolFlags); err != nil {
		return err
	}
	positionals, err := positionalsForOptions(args, nil, boolFlags)
	if err != nil {
		return err
	}
	if len(positionals) != 1 {
		return usage("sale resume requires one SALE_ID")
	}
	wait := has(args, "--wait")
	s, err := a.openSaleSession(ctx, cfg, paths, renderer)
	if err != nil {
		return err
	}
	sale, err := s.findSale(positionals[0])
	if err != nil {
		return err
	}
	var report saleReport
	if sale.Role == salestate.Seller {
		if sale.Phase == salestate.PhaseDelivering {
			room, err := s.state.LoadRoom(sale.RoomID)
			if err != nil {
				return cliError(ExitOperationFailed, "Could not read the sale Room record.", "", err)
			}
			if err := s.deliverPackage(ctx, &sale, room); err != nil {
				return err
			}
		}
		report, err = s.driveSeller(ctx, &sale, wait, wait)
	} else {
		report, err = s.driveBuyer(ctx, &sale, wait)
	}
	if err != nil {
		return err
	}
	return renderSaleReport(renderer, report)
}

// saleWithdraw collects what the escrow credited the wallet when a direct payment to it failed.
func (a *App) saleWithdraw(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	if len(args) > 0 {
		return usage("sale withdraw takes no arguments")
	}
	s, err := a.openSaleSession(ctx, cfg, paths, renderer)
	if err != nil {
		return err
	}
	credit, err := s.net.Contract.Credit(ctx, s.wallet.Address())
	if err != nil {
		return saleTxError("read the escrow credit", err)
	}
	if credit.Sign() == 0 {
		return renderer.Result(map[string]any{"withdrawn_tao": "0"}, func(w io.Writer) error {
			_, err := fmt.Fprintln(w, "The escrow holds nothing for this wallet.")
			return err
		})
	}
	keys, need, err := s.gasKeys(ctx, gasWithdraw, nil)
	if err != nil {
		return err
	}
	if len(keys) == 0 || keys[0] != s.wallet.Key {
		return typedCLIError(ExitOperationFailed, "sale_wallet_no_gas", fmt.Sprintf("The sale wallet needs %s TAO to withdraw.", formatTAO(need)), "Fund "+s.wallet.Address().Hex()+".", nil)
	}
	data, err := escrow.WithdrawData()
	if err != nil {
		return err
	}
	out, err := s.sender(s.wallet.Key).Send(ctx, escrow.Request{Label: "withdraw", To: s.net.Domain.Escrow, Data: data, GasLimit: gasWithdraw})
	if err != nil {
		return saleTxError("withdraw", err)
	}
	return renderer.Result(map[string]any{"withdrawn_tao": formatTAO(credit), "transaction": out.Receipt.TxHash.Hex()}, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "Withdrew %s TAO from the escrow in %s.\n", formatTAO(credit), out.Receipt.TxHash.Hex())
		return err
	})
}
