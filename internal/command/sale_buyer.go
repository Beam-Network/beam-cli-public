package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/Beam-Network/beam-cli-public/internal/auth"
	"github.com/Beam-Network/beam-cli-public/internal/config"
	"github.com/Beam-Network/beam-cli-public/internal/datasale"
	"github.com/Beam-Network/beam-cli-public/internal/escrow"
	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/roomcli/protocol/state"
	"github.com/Beam-Network/beam-cli-public/internal/saleroom"
	"github.com/Beam-Network/beam-cli-public/internal/salestate"
	"github.com/Beam-Network/beam-cli-public/internal/salewallet"
	"github.com/Beam-Network/beam-cli-public/internal/tunnel"
)

// saleJoin is the buyer joining the seller's sale Room and binding its member there to its sale wallet.
func (a *App) saleJoin(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	valueFlags := map[string]bool{"--invitation-file": true, "--invitation-token": true, "--out": true}
	if err := validateOptions(args, valueFlags, nil); err != nil {
		return err
	}
	positionals, err := positionalsForOptions(args, valueFlags, nil)
	if err != nil {
		return err
	}
	if len(positionals) != 1 {
		return usage("sale join requires one ROOM_ID")
	}
	roomID := positionals[0]
	file, hasFile, err := flag(args, "--invitation-file")
	if err != nil {
		return err
	}
	token, hasToken, err := flag(args, "--invitation-token")
	if err != nil {
		return err
	}
	if hasFile == hasToken {
		return usage("pass exactly one of --invitation-file or --invitation-token")
	}
	out, hasOut, err := flag(args, "--out")
	if err != nil {
		return err
	}
	if hasOut {
		if _, err := os.Lstat(out); err == nil {
			return usage("--out " + out + " already exists")
		}
	}
	wallet, err := loadSaleWallet(paths)
	if err != nil {
		return err
	}
	network, err := a.dialSaleNetwork(ctx, cfg, renderer)
	if err != nil {
		return err
	}
	// The binding names the escrow, so it is checked before the Room is joined.
	if err := network.requireReleasedEscrow(ctx); err != nil {
		return err
	}
	join := []string{"join", roomID, "--idempotency-key", "sale-join-" + roomID}
	if hasFile {
		join = append(join, "--invitation-file", file)
	} else {
		join = append(join, "--invitation-token", token)
	}
	data, err := a.saleRoomJSON(ctx, cfg, paths, join...)
	if err != nil {
		return err
	}
	record, err := decodeSaleRoom[state.BTRRoomRecord](data, "Room")
	if err != nil {
		return err
	}
	memberID := record.Membership.MemberID
	binding, err := saleroom.SignBinding(wallet.Key, network.Domain, roomID, memberID, time.Now())
	if err != nil {
		return cliError(ExitOperationFailed, "Could not sign the buyer binding.", "", err)
	}
	if err := (salestate.Store{Dir: paths.Dir}).SaveRoom(salestate.Room{RoomID: roomID, Role: salestate.Buyer, MemberID: memberID, CreatedAt: time.Now().UTC()}); err != nil {
		return cliError(ExitOperationFailed, "Could not record the sale Room.", "", err)
	}
	encoded, err := json.MarshalIndent(binding, "", "  ")
	if err != nil {
		return err
	}
	result := map[string]any{"room_id": roomID, "member_id": memberID, "buyer": wallet.Address().Hex(), "binding": binding}
	if hasOut {
		if err := auth.WriteRestrictedFile(out, append(encoded, '\n')); err != nil {
			return cliError(ExitOperationFailed, "Could not write the binding file.", "", err)
		}
		result["binding_file"] = out
	}
	return renderer.Result(result, func(w io.Writer) error {
		where := "Pass --out to write the binding to a file, or send this binding:\n" + string(encoded) + "\n"
		if hasOut {
			where = "The binding is in " + out + ".\n"
		}
		_, err := fmt.Fprintf(w, "Joined the sale Room %s as member %s, buying with %s.\n%sGive the binding to the seller: it is what the seller's offer names this wallet from.\n",
			roomID, memberID, wallet.Address().Hex(), where)
		return err
	})
}

func requireSaleCapabilities(ctx context.Context, client *tunnel.LocalClient, capabilities ...string) error {
	status, err := client.Status(ctx)
	if err != nil {
		return mapTunnelError(err)
	}
	for _, capability := range capabilities {
		if !slices.Contains(status.Capabilities, capability) {
			return typedCLIError(ExitVersionIncompatible, "sale_agent_unsupported", "The local beam-agentd does not support "+capability+", which data sales need.",
				"Run `beam update`.", nil)
		}
	}
	return nil
}

// agent connects to the local agent, which the buyer reads its delivered package through.
func (s *saleSession) agent(ctx context.Context) (*tunnel.LocalClient, error) {
	client, err := s.app.readyLocalClient(ctx, s.cfg)
	if err != nil {
		return nil, err
	}
	if err := requireSaleCapabilities(ctx, client, "btr.inbox.list", "btr.inbox.read"); err != nil {
		return nil, err
	}
	return client, nil
}

// saleBuy is the buyer's side of a sale: it waits for the seller's package, checks it, funds the escrow, waits for
// the key, decrypts and checks every leaf, and challenges a leaf that does not match.
func (a *App) saleBuy(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	valueFlags := map[string]bool{"--out": true, "--max-price": true, "--sale": true, "--wait": true}
	if err := validateOptions(args, valueFlags, nil); err != nil {
		return err
	}
	positionals, err := positionalsForOptions(args, valueFlags, nil)
	if err != nil {
		return err
	}
	if len(positionals) != 1 {
		return usage("sale buy requires one ROOM_ID")
	}
	roomID := positionals[0]
	out, err := requiredFlag(args, "--out")
	if err != nil {
		return err
	}
	maxPriceText, err := requiredFlag(args, "--max-price")
	if err != nil {
		return err
	}
	maxPrice, err := parseTAO(maxPriceText)
	if err != nil || maxPrice.Sign() == 0 {
		return usage("--max-price must be a positive TAO amount")
	}
	wait := 30 * time.Minute
	if value, ok, err := flag(args, "--wait"); err != nil {
		return err
	} else if ok {
		if wait, err = time.ParseDuration(value); err != nil || wait < 0 {
			return usage("--wait must be a duration such as 30m")
		}
	}
	want, _, err := flag(args, "--sale")
	if err != nil {
		return err
	}
	if out, err = filepath.Abs(out); err != nil {
		return usage("--out: " + err.Error())
	}
	for _, path := range []string{out, out + ".part", out + ".unverified"} {
		if _, err := os.Lstat(path); err == nil {
			return usage(path + " already exists")
		}
	}
	if info, err := os.Stat(filepath.Dir(out)); err != nil || !info.IsDir() {
		return usage("the directory of --out does not exist")
	}

	s, err := a.openSaleSession(ctx, cfg, paths, renderer)
	if err != nil {
		return err
	}
	room, err := s.state.LoadRoom(roomID)
	if err != nil || room.Role != salestate.Buyer {
		return typedCLIError(ExitNotFound, "sale_room_unknown", "Room "+roomID+" is not a sale Room this machine joined.", "Join it with `beam sale join`.", err)
	}
	client, err := s.agent(ctx)
	if err != nil {
		return err
	}
	object, err := s.waitForPackage(ctx, client, roomID, want, wait)
	if err != nil {
		return err
	}
	src := saleroom.InboxSource{Ctx: ctx, Local: client, Object: object}
	header, err := saleroom.ReadHeader(src)
	if err != nil {
		return typedCLIError(ExitOperationFailed, "sale_package_rejected", "The delivered file is not a valid sale package.", "Do not fund it.", err)
	}
	saleID, err := datasale.SaleID(s.net.Domain, header.Offer)
	if err != nil {
		return typedCLIError(ExitOperationFailed, "sale_package_rejected", "The delivered offer is malformed.", "Do not fund it.", err)
	}
	if object.Filename != saleroom.PackageName(saleID) {
		return typedCLIError(ExitOperationFailed, "sale_package_rejected", "The package's name does not match the offer inside it.", "Do not fund it.", nil)
	}
	if _, err := s.state.Load(saleID); err == nil {
		return typedCLIError(ExitConflict, "sale_already_recorded", "Sale "+shortSaleID(saleID)+" is already recorded on this machine.",
			"Continue it with `beam sale resume "+shortSaleID(saleID)+"`.", nil)
	} else if !errors.Is(err, salestate.ErrNotFound) {
		return cliError(ExitOperationFailed, "Could not read the sale records.", "", err)
	}
	// With no record, anything in the sale's directory is left from an interrupted check. Only a directory this run
	// creates is removed when the package is refused.
	saleDir := s.state.SaleDir(saleID)
	_, statErr := os.Lstat(saleDir)
	created := errors.Is(statErr, os.ErrNotExist)
	discard := func() {
		if created {
			_ = os.RemoveAll(saleDir)
		}
	}
	treeDir := filepath.Join(saleDir, "trees")
	_ = os.RemoveAll(treeDir)
	renderer.Progress(fmt.Sprintf("Checking the package %s (%d bytes) against the seller's signed commitments.", object.Filename, object.SizeBytes))
	checked, err := saleroom.CheckPackage(ctx, src, s.net.Domain, s.wallet.Address(), treeDir)
	if err != nil {
		discard()
		if errors.Is(err, saleroom.ErrPackageCheck) || errors.Is(err, saleroom.ErrPackage) {
			return typedCLIError(ExitOperationFailed, "sale_package_rejected", "The package failed the checks, so it is not funded.", "", err)
		}
		return cliError(ExitOperationFailed, "Could not check the package.", "Run the command again.", err)
	}
	head, err := s.net.Backend.BlockNumber(ctx)
	if err != nil {
		return saleTxError("read the chain", err)
	}
	if err := s.checkTerms(ctx, checked.Header.Offer, maxPrice, head); err != nil {
		discard()
		return err
	}
	sale := &salestate.Sale{
		SaleID: saleID, Role: salestate.Buyer, ChainID: s.net.Domain.ChainID, Escrow: s.net.Domain.Escrow,
		RoomID: roomID, MemberID: room.MemberID, Header: checked.Header, StartBlock: head, Phase: salestate.PhaseChecked,
		TransferID: object.TransferID, TreeDir: treeDir, Output: out,
	}
	if err := s.state.Save(sale); err != nil {
		return cliError(ExitOperationFailed, "Could not record the sale.", "", err)
	}
	offer := checked.Header.Offer
	renderer.Progress(fmt.Sprintf("The package matches its commitments: %s, %d bytes, from %s, for %s TAO.", saleDatasetName(checked.Header.Name), offer.SizeBytes, offer.Seller.Hex(), formatTAO(offer.Price)))
	report, err := s.driveBuyer(ctx, sale, true)
	if err != nil {
		return err
	}
	return renderSaleReport(renderer, report)
}

// checkTerms applies the buyer's rules to an offer before funding it.
func (s *saleSession) checkTerms(ctx context.Context, offer datasale.Offer, maxPrice *big.Int, head uint64) error {
	limits, err := s.net.Contract.Limits(ctx)
	if err != nil {
		return saleTxError("read the escrow's limits", err)
	}
	reject := func(message string) error {
		return typedCLIError(ExitOperationFailed, "sale_terms_rejected", message, "The sale is not funded.", nil)
	}
	switch {
	case offer.Price.Cmp(maxPrice) > 0:
		return reject(fmt.Sprintf("The offer asks %s TAO, more than --max-price %s TAO.", formatTAO(offer.Price), formatTAO(maxPrice)))
	case offer.Price.Cmp(limits.MaxPrice) > 0:
		return reject(fmt.Sprintf("The offer asks %s TAO; the escrow accepts at most %s TAO.", formatTAO(offer.Price), formatTAO(limits.MaxPrice)))
	case offer.SizeBytes > maxSaleBytes:
		return reject("The dataset is larger than 100 GB.")
	case offer.RevealPeriod < limits.MinRevealPeriod || offer.RevealPeriod > maxSalePeriod:
		return reject(fmt.Sprintf("The reveal period of %d blocks is outside the escrow's bounds.", offer.RevealPeriod))
	case offer.ChallengePeriod < limits.MinChallengePeriod || offer.ChallengePeriod > maxSalePeriod:
		return reject(fmt.Sprintf("The challenge period of %d blocks is outside the escrow's bounds.", offer.ChallengePeriod))
	case offer.Seller == s.wallet.Address():
		return reject("The offer is from this wallet.")
	case head+fundMargin > offer.FundDeadline:
		return reject(fmt.Sprintf("The offer could be funded only until block %d.", offer.FundDeadline))
	}
	return nil
}

// waitForPackage returns the newest sale package delivered in the Room that this machine has not taken up yet.
func (s *saleSession) waitForPackage(ctx context.Context, local saleroom.Local, roomID, want string, wait time.Duration) (saleroom.InboxObject, error) {
	deadline := time.Now().Add(wait)
	want = strings.ToLower(strings.TrimPrefix(want, "0x"))
	said := false
	for {
		objects, err := saleroom.ListInbox(ctx, local, roomID)
		if err != nil {
			return saleroom.InboxObject{}, mapTunnelError(err)
		}
		for _, object := range saleroom.Packages(objects) {
			id := strings.TrimSuffix(strings.TrimPrefix(object.Filename, "beam-sale-"), ".package")
			if !strings.HasPrefix(id, want) {
				continue
			}
			if _, err := s.state.Load(common.HexToHash(id)); err == nil {
				continue
			}
			return object, nil
		}
		if time.Now().After(deadline) {
			return saleroom.InboxObject{}, typedCLIError(ExitNotFound, "sale_no_package", "No new sale package has arrived in Room "+roomID+".",
				"Ask the seller to run `beam sale offer`, then run this again; a sale this machine already took up continues with `beam sale resume`.", nil)
		}
		if !said {
			s.renderer.Progress("Waiting for the seller's package in Room " + roomID + ".")
			said = true
		}
		if err := s.app.saleSleep(ctx); err != nil {
			return saleroom.InboxObject{}, err
		}
	}
}

// driveBuyer carries the buyer's side forward from the escrow's state: fund, reclaim the payment if no key comes,
// decrypt and check once it does, and challenge a mismatching leaf. It waits for the key only when asked to.
func (s *saleSession) driveBuyer(ctx context.Context, sale *salestate.Sale, waitReveal bool) (saleReport, error) {
	offer := sale.Header.Offer
	said := ""
	say := func(message string) {
		if message != said {
			s.renderer.Progress(message)
			said = message
		}
	}
	for {
		record, head, err := s.chainState(ctx, sale.SaleID)
		if err != nil {
			return saleReport{}, err
		}
		switch record.State {
		case escrow.StateNone:
			if head > offer.FundDeadline {
				return s.endSale(ctx, sale, salestate.OutcomeExpired, record, head)
			}
			if err := s.act(ctx, say, func() error { return s.fund(ctx, sale) }); err != nil {
				return saleReport{}, err
			}
			continue
		case escrow.StateFunded:
			if head > record.Deadline {
				say("The seller did not reveal the key in time. Reclaiming the payment.")
				if err := s.act(ctx, say, func() error { return s.refundUnrevealed(ctx, sale) }); err != nil {
					return saleReport{}, err
				}
				continue
			}
			if !waitReveal {
				return saleReportOf(*sale, &record, head), nil
			}
			say(fmt.Sprintf("Funded. Waiting for the seller to reveal the key, until block %d (%s).", record.Deadline, blocksAway(head, record.Deadline)))
		case escrow.StateKeyReleased:
			switch sale.Phase {
			case salestate.PhaseVerified:
				return saleReportOf(*sale, &record, head), nil
			case salestate.PhaseMismatch:
				if err := s.act(ctx, say, func() error { return s.challenge(ctx, sale, record, head) }); err != nil {
					return saleReport{}, err
				}
			default:
				if err := s.act(ctx, say, func() error { return s.decrypt(ctx, sale, record) }); err != nil {
					return saleReport{}, err
				}
			}
			continue
		case escrow.StateSettled:
			return s.endSale(ctx, sale, salestate.OutcomeSettled, record, head)
		case escrow.StateRefunded:
			return s.endSale(ctx, sale, salestate.OutcomeRefunded, record, head)
		case escrow.StateRevoked:
			return s.endSale(ctx, sale, salestate.OutcomeRevoked, record, head)
		}
		if err := s.app.saleSleep(ctx); err != nil {
			return saleReport{}, err
		}
	}
}

// fund pays the price into the escrow. A first attempt is checked against the wallet's limits, and a price above the
// approval threshold needs a person's confirmation.
func (s *saleSession) fund(ctx context.Context, sale *salestate.Sale) error {
	offer := sale.Header.Offer
	if len(sale.Transactions["fund"]) == 0 {
		keys, need, err := s.gasKeys(ctx, gasFund, offer.Price)
		if err != nil {
			return err
		}
		if len(keys) == 0 || keys[0] != s.wallet.Key {
			return typedCLIError(ExitOperationFailed, "sale_wallet_insufficient", fmt.Sprintf("The sale wallet needs %s TAO for the price and gas.", formatTAO(need)),
				"Fund "+s.wallet.Address().Hex()+", then run `beam sale resume "+shortSaleID(sale.SaleID)+"`.", nil)
		}
		needsApproval, err := s.ledger.Spend(s.wallet.Limits, sale.SaleID, offer.Price, false)
		if err != nil {
			return saleSpendError(err)
		}
		if needsApproval {
			summary := fmt.Sprintf("Fund the sale %s: %s, %d bytes, from %s, for %s TAO?", shortSaleID(sale.SaleID),
				saleDatasetName(sale.Header.Name), offer.SizeBytes, offer.Seller.Hex(), formatTAO(offer.Price))
			if err := s.app.confirmByPerson(s.renderer, summary, "Fund it?"); err != nil {
				return err
			}
			if _, err := s.ledger.Spend(s.wallet.Limits, sale.SaleID, offer.Price, true); err != nil {
				return saleSpendError(err)
			}
		}
		sale.Phase = salestate.PhaseFunding
		if err := s.state.Save(sale); err != nil {
			return cliError(ExitOperationFailed, "Could not record the sale.", "", err)
		}
	}
	data, err := escrow.FundData(offer, saleCommitments(sale), sale.Header.Signature)
	if err != nil {
		return err
	}
	s.renderer.Progress(fmt.Sprintf("Funding the sale with %s TAO.", formatTAO(offer.Price)))
	_, err = s.transact(ctx, sale, s.sender(s.wallet.Key), "fund", escrow.Request{
		Data: data, Value: offer.Price, GasLimit: gasFund, Deadline: offer.FundDeadline, Margin: fundMargin,
	})
	if err != nil {
		return saleTxError("fund the sale", err)
	}
	return nil
}

func saleSpendError(err error) error {
	if errors.Is(err, salewallet.ErrPerSaleLimit) || errors.Is(err, salewallet.ErrDailyLimit) {
		return saleLimitError(err)
	}
	return cliError(ExitOperationFailed, "Could not update the sale ledger.", "", err)
}

func (s *saleSession) refundUnrevealed(ctx context.Context, sale *salestate.Sale) error {
	keys, need, err := s.gasKeys(ctx, gasRefund, nil)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return typedCLIError(ExitOperationFailed, "sale_wallet_no_gas", fmt.Sprintf("No account of the sale wallet holds the %s TAO a refund needs.", formatTAO(need)), "Fund "+s.wallet.Address().Hex()+".", nil)
	}
	data, err := escrow.RefundUnrevealedData(sale.Header.Offer)
	if err != nil {
		return err
	}
	if _, err := s.transact(ctx, sale, s.sender(keys[0], keys[1:]...), "refund", escrow.Request{Data: data, GasLimit: gasRefund, AnySender: true}); err != nil {
		return saleTxError("reclaim the payment", err)
	}
	return nil
}

// revealedKey reads the key from the escrow's KeyRevealed event, in the one block the reveal was included in.
func (s *saleSession) revealedKey(ctx context.Context, sale *salestate.Sale, record escrow.Record) (common.Hash, error) {
	block := new(big.Int).SetUint64(record.Deadline - sale.Header.Offer.ChallengePeriod)
	logs, err := s.net.Backend.FilterLogs(ctx, ethereum.FilterQuery{
		FromBlock: block, ToBlock: block, Addresses: []common.Address{s.net.Domain.Escrow}, Topics: [][]common.Hash{nil, {sale.SaleID}},
	})
	if err != nil {
		return common.Hash{}, saleTxError("read the revealed key", err)
	}
	pointers := make([]*types.Log, len(logs))
	for i := range logs {
		pointers[i] = &logs[i]
	}
	events, err := escrow.DecodeEvents(s.net.Domain.Escrow, pointers)
	if err != nil {
		return common.Hash{}, saleTxError("read the revealed key", err)
	}
	for _, event := range events {
		if event.Name == "KeyRevealed" && datasale.KeyCommitment(sale.SaleID, event.Key) == sale.Header.KeyCommitment {
			return event.Key, nil
		}
	}
	return common.Hash{}, typedCLIError(ExitOperationFailed, "sale_key_missing", "The escrow says the key is revealed, but no endpoint returned it.", "Run `beam sale resume` again.", nil)
}

// inbox is the delivered package as the buyer's agent holds it.
func (s *saleSession) inbox(ctx context.Context, sale *salestate.Sale) (saleroom.InboxSource, error) {
	client, err := s.agent(ctx)
	if err != nil {
		return saleroom.InboxSource{}, err
	}
	return saleroom.InboxSource{Ctx: ctx, Local: client, Object: saleroom.InboxObject{
		RoomID: sale.RoomID, TransferID: sale.TransferID, SizeBytes: saleroom.PackageSize(sale.Header.Leaves()),
	}}, nil
}

// dropDelivered removes the delivered package from the agent's inbox, once the buyer no longer needs it to prove a
// leaf. An agent that cannot delete inbox objects keeps it until its own inbox limits remove it.
func (s *saleSession) dropDelivered(ctx context.Context, sale *salestate.Sale) {
	if sale.TransferID == "" {
		return
	}
	client, err := s.app.readyLocalClient(ctx, s.cfg)
	if err == nil {
		err = requireSaleCapabilities(ctx, client, "btr.inbox.delete")
	}
	if err == nil {
		err = saleroom.DeleteInbox(ctx, client, sale.RoomID, sale.TransferID)
	}
	if err != nil {
		s.renderer.Progress("The delivered package stays in the agent's inbox: " + asCLIError(err).Message)
		return
	}
	sale.TransferID = ""
}

// decrypt writes the dataset with the revealed key and checks every leaf against the hash the seller committed to.
// A dataset that checks out goes to its output path; one that does not is kept beside it, marked unverified, and the
// sale is challenged.
func (s *saleSession) decrypt(ctx context.Context, sale *salestate.Sale, record escrow.Record) error {
	key, err := s.revealedKey(ctx, sale, record)
	if err != nil {
		return err
	}
	src, err := s.inbox(ctx, sale)
	if err != nil {
		return err
	}
	ciphertext, err := saleroom.Ciphertext(src, sale.Header)
	if err != nil {
		return saleInboxError(sale, err)
	}
	defer ciphertext.Close()
	hashes, err := saleroom.HashList(src, sale.Header)
	if err != nil {
		return saleInboxError(sale, err)
	}
	defer hashes.Close()
	part := sale.Output + ".part"
	f, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return cliError(ExitOperationFailed, "Could not write the dataset.", "", err)
	}
	s.renderer.Progress("The key is revealed. Decrypting and checking every leaf.")
	result, err := datasale.Decrypt(ctx, ciphertext, sale.Header.Offer.SizeBytes, key, hashes, f)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return cliError(ExitOperationFailed, "Could not decrypt the dataset.", "Run `beam sale resume "+shortSaleID(sale.SaleID)+"` to try again.", err)
	}
	if result.MismatchCount == 0 {
		if _, err := os.Lstat(sale.Output); err == nil {
			return typedCLIError(ExitConflict, "sale_output_exists", sale.Output+" appeared while decrypting; the dataset is in "+part+".", "", nil)
		}
		if err := os.Rename(part, sale.Output); err != nil {
			return cliError(ExitOperationFailed, "Could not move the dataset into place.", "It is in "+part+".", err)
		}
		_ = os.RemoveAll(sale.TreeDir)
		sale.TreeDir = ""
		sale.Phase = salestate.PhaseVerified
		s.renderer.Progress("Every leaf matches the seller's commitment. The dataset is in " + sale.Output + ".")
		// No challenge can be needed now, so the encrypted copy is of no further use.
		s.dropDelivered(ctx, sale)
	} else {
		// The challenge's evidence is kept now, so the challenge does not depend on the agent still holding the package.
		index := result.Mismatches[0]
		leaf, err := saleroom.CipherLeaf(src, sale.Header, index)
		if err != nil {
			return saleInboxError(sale, err)
		}
		plainHash, err := saleroom.PlainHash(src, index)
		if err != nil {
			return saleInboxError(sale, err)
		}
		if err := os.Rename(part, sale.Output+".unverified"); err != nil {
			return cliError(ExitOperationFailed, "Could not keep the decrypted file.", "", err)
		}
		sale.Mismatches = result.Mismatches[:min(len(result.Mismatches), 16)]
		sale.ChallengeLeaf, sale.ChallengePlainHash = leaf, plainHash
		sale.Phase = salestate.PhaseMismatch
		s.renderer.Progress(fmt.Sprintf("%d of %d leaves do not match the seller's commitment. Challenging the sale to reclaim the payment.",
			result.MismatchCount, sale.Header.Leaves()))
	}
	if err := s.state.Save(sale); err != nil {
		return cliError(ExitOperationFailed, "Could not record the sale.", "", err)
	}
	return nil
}

// challenge proves to the escrow that one delivered leaf decrypts to something other than what the seller committed
// to, which refunds the buyer. The proof is checked locally first, so a challenge is only sent when it succeeds.
func (s *saleSession) challenge(ctx context.Context, sale *salestate.Sale, record escrow.Record, head uint64) error {
	margin := s.app.saleSafetyMargin()
	if len(sale.Transactions["challenge"]) == 0 && head+margin > record.Deadline {
		return typedCLIError(ExitOperationFailed, "sale_too_late", fmt.Sprintf("It is too late to challenge: the challenge window ends at block %d.", record.Deadline), "", nil)
	}
	key, err := s.revealedKey(ctx, sale, record)
	if err != nil {
		return err
	}
	offer := sale.Header.Offer
	index, leaf, plainHash := sale.Mismatches[0], sale.ChallengeLeaf, sale.ChallengePlainHash
	plainTree, err := datasale.OpenTree(filepath.Join(sale.TreeDir, "plain"))
	if err != nil {
		return cliError(ExitOperationFailed, "Could not open the proof trees.", "", err)
	}
	cipherTree, err := datasale.OpenTree(filepath.Join(sale.TreeDir, "cipher"))
	if err != nil {
		return cliError(ExitOperationFailed, "Could not open the proof trees.", "", err)
	}
	plainProof, err := plainTree.Proof(index)
	if err != nil {
		return cliError(ExitOperationFailed, "Could not build the proof.", "", err)
	}
	cipherProof, err := cipherTree.Proof(index)
	if err != nil {
		return cliError(ExitOperationFailed, "Could not build the proof.", "", err)
	}
	decrypted := make([]byte, len(leaf))
	datasale.XORKeystream(key, index, decrypted, leaf)
	if datasale.ProcessProof(cipherProof, datasale.CipherLeafHash(sale.SaleID, index, leaf)) != sale.Header.CipherRoot ||
		datasale.ProcessProof(plainProof, datasale.PlainLeafHash(index, plainHash)) != offer.PlainRoot ||
		crypto.Keccak256Hash(decrypted) == plainHash {
		return typedCLIError(ExitOperationFailed, "sale_challenge_invalid", fmt.Sprintf("The proof for leaf %d does not hold locally, so no challenge is sent.", index), "", nil)
	}
	keys, need, err := s.gasKeys(ctx, gasChallenge, nil)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return typedCLIError(ExitOperationFailed, "sale_wallet_no_gas", fmt.Sprintf("No account of the sale wallet holds the %s TAO a challenge needs.", formatTAO(need)),
			fmt.Sprintf("Fund %s; the challenge window ends at block %d.", s.wallet.Address().Hex(), record.Deadline), nil)
	}
	data, err := escrow.ChallengeData(offer, saleCommitments(sale), key, escrow.LeafProof{
		Index: index, CipherLeaf: leaf, PlainHash: plainHash, CipherProof: cipherProof, PlainProof: plainProof,
	})
	if err != nil {
		return err
	}
	if _, err := s.transact(ctx, sale, s.sender(keys[0], keys[1:]...), "challenge", escrow.Request{
		Data: data, GasLimit: gasChallenge, Deadline: record.Deadline, Margin: margin, AnySender: true,
	}); err != nil {
		return saleTxError("challenge the sale", err)
	}
	s.renderer.Progress(fmt.Sprintf("Challenged leaf %d.", index))
	return nil
}

// saleInboxError explains a failed read of the delivered package from the agent's inbox.
func saleInboxError(sale *salestate.Sale, err error) error {
	var tunnelErr *tunnel.Error
	if errors.As(err, &tunnelErr) && tunnelErr.Kind == tunnel.ErrNotFound {
		return typedCLIError(ExitNotFound, "sale_package_gone", "The agent no longer holds the package delivered for sale "+shortSaleID(sale.SaleID)+".",
			"An agent run with --room-inbox-max-age or --room-inbox-max-gib removes delivered objects; keep them until every sale ends.", err)
	}
	return mapTunnelError(err)
}
