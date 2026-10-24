package command

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/auth"
	"github.com/Beam-Network/beam-cli-public/internal/config"
	"github.com/Beam-Network/beam-cli-public/internal/datasale"
	"github.com/Beam-Network/beam-cli-public/internal/escrow"
	"github.com/Beam-Network/beam-cli-public/internal/output"
	"github.com/Beam-Network/beam-cli-public/internal/saleroom"
	"github.com/Beam-Network/beam-cli-public/internal/salestate"
	"github.com/Beam-Network/beam-cli-public/internal/salewallet"
	"github.com/ethereum/go-ethereum/common"
)

// saleRoomCreate sets up a Room for one buyer: the Room, its object channel and a one-use invitation.
func (a *App) saleRoomCreate(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	if len(args) == 0 || args[0] != "create" {
		return usage("sale room requires create")
	}
	args = args[1:]
	valueFlags := map[string]bool{"--api-key": true, "--max-size": true, "--invitation-ttl": true, "--out": true}
	if err := validateOptions(args, valueFlags, nil); err != nil {
		return err
	}
	if positionals, err := positionalsForOptions(args, valueFlags, nil); err != nil || len(positionals) > 0 {
		return usage("sale room create takes only options")
	}
	maxSize := uint64(maxSaleBytes)
	if value, ok, err := flag(args, "--max-size"); err != nil {
		return err
	} else if ok {
		if maxSize, err = parseSaleSize(value); err != nil {
			return usage("--max-size: " + err.Error())
		}
	}
	ttl := 24 * time.Hour
	if value, ok, err := flag(args, "--invitation-ttl"); err != nil {
		return err
	} else if ok {
		if ttl, err = time.ParseDuration(value); err != nil || ttl < time.Minute {
			return usage("--invitation-ttl must be a duration of at least 1m, such as 24h")
		}
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
	apiKey, _, err := flag(args, "--api-key")
	if err != nil {
		return err
	}
	key := "sale-room-" + randomHex(8)
	room, err := a.createSaleRoom(ctx, cfg, paths, apiKey, salePackageBytes(maxSize), key)
	if err != nil {
		return err
	}
	store := salestate.Store{Dir: paths.Dir}
	if err := store.SaveRoom(salestate.Room{RoomID: room.RoomID, Role: salestate.Seller, ChannelID: room.ChannelID, MemberID: room.SellerMemberID, CreatedAt: time.Now().UTC()}); err != nil {
		return cliError(ExitOperationFailed, "Could not record the sale Room.", "", err)
	}
	invitation, err := a.inviteSaleBuyer(ctx, cfg, paths, room, ttl, key)
	if err != nil {
		return err
	}
	result := map[string]any{
		"room_id": room.RoomID, "channel_id": room.ChannelID, "invitation_id": invitation.Invitation.InvitationID,
		"invitation_expires_at": invitation.Invitation.ExpiresAt, "max_size_bytes": maxSize,
	}
	if hasOut {
		if err := auth.WriteRestrictedFile(out, []byte(invitation.InvitationToken+"\n")); err != nil {
			return cliError(ExitOperationFailed, "Could not write the invitation file.", "", err)
		}
		result["invitation_file"] = out
	} else {
		result["invitation_token"] = invitation.InvitationToken
	}
	return renderer.Result(result, func(w io.Writer) error {
		where := "The invitation token is in --json output; pass it with --out next time to keep it in a file."
		if hasOut {
			where = "The one-use invitation is in " + out + "."
		}
		_, err := fmt.Fprintf(w, "Created the sale Room %s.\n%s Give it to the buyer, who joins with `beam sale join %s --invitation-file <file>`.\n",
			room.RoomID, where, room.RoomID)
		return err
	})
}

var sizePattern = regexp.MustCompile(`^([0-9]+)\s*([a-zA-Z]*)$`)

// parseSaleSize reads a size such as 500MB, 10GiB or 1048576.
func parseSaleSize(value string) (uint64, error) {
	match := sizePattern.FindStringSubmatch(strings.TrimSpace(value))
	if match == nil {
		return 0, fmt.Errorf("%q is not a size", value)
	}
	n, err := strconv.ParseUint(match[1], 10, 64)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("%q is not a size", value)
	}
	units := map[string]uint64{"": 1, "b": 1, "kb": 1e3, "mb": 1e6, "gb": 1e9, "tb": 1e12, "kib": 1 << 10, "mib": 1 << 20, "gib": 1 << 30, "tib": 1 << 40}
	unit, ok := units[strings.ToLower(match[2])]
	if !ok {
		return 0, fmt.Errorf("unknown unit %q", match[2])
	}
	if n > maxSaleBytes/unit {
		return 0, fmt.Errorf("the largest dataset is 100 GB")
	}
	return n * unit, nil
}

// salePackageBytes is the size of the package that carries a dataset of size bytes.
func salePackageBytes(size uint64) uint64 {
	return uint64(saleroom.PackageSize(datasale.LeafCount(size)))
}

// saleDeliveryBlocks is how many blocks the Room's delivery window for a package of packageBytes lasts.
func saleDeliveryBlocks(packageBytes uint64) uint64 {
	return uint64((saleroom.DeliveryTTL(packageBytes) + saleBlockTime - 1) / saleBlockTime)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// saleOffer is the seller's side of a sale: it checks the buyer and the Room, builds and signs the package, delivers
// it to the buyer, waits for funding and reveals the key.
func (a *App) saleOffer(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	valueFlags := map[string]bool{"--file": true, "--price": true, "--binding": true, "--reveal-period": true, "--challenge-period": true,
		"--fund-within": true, "--tao-usd": true, "--package-dir": true, "--name": true}
	if err := validateOptions(args, valueFlags, nil); err != nil {
		return err
	}
	positionals, err := positionalsForOptions(args, valueFlags, nil)
	if err != nil {
		return err
	}
	if len(positionals) != 1 {
		return usage("sale offer requires one ROOM_ID")
	}
	roomID := positionals[0]
	file, err := requiredFlag(args, "--file")
	if err != nil {
		return err
	}
	priceText, err := requiredFlag(args, "--price")
	if err != nil {
		return err
	}
	bindingPath, err := requiredFlag(args, "--binding")
	if err != nil {
		return err
	}
	price, err := parseTAO(priceText)
	if err != nil || price.Sign() == 0 {
		return usage("--price must be a positive TAO amount")
	}
	revealPeriod, err := parseBlocks(args, "--reveal-period", defaultRevealPeriod)
	if err != nil {
		return err
	}
	challengePeriod, err := parseBlocks(args, "--challenge-period", defaultChallengePeriod)
	if err != nil {
		return err
	}
	fundWithin, err := parseBlocks(args, "--fund-within", defaultFundWindow)
	if err != nil {
		return err
	}
	quoteText, _, err := flag(args, "--tao-usd")
	if err != nil {
		return err
	}

	info, err := os.Stat(file)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return usage("--file must be a non-empty regular file")
	}
	if info.Size() > maxSaleBytes {
		return typedCLIError(ExitUsage, "sale_too_large", "The largest dataset a sale carries is 100 GB.", "", nil)
	}
	size := uint64(info.Size())
	packageDir, _, err := flag(args, "--package-dir")
	if err != nil {
		return err
	}
	if packageDir == "" {
		packageDir = filepath.Dir(file)
	}
	name, _, err := flag(args, "--name")
	if err != nil {
		return err
	}
	if name == "" {
		name = filepath.Base(file)
	}

	s, err := a.openSaleSession(ctx, cfg, paths, renderer)
	if err != nil {
		return err
	}
	room, err := s.state.LoadRoom(roomID)
	if err != nil || room.Role != salestate.Seller {
		return typedCLIError(ExitNotFound, "sale_room_unknown", "Room "+roomID+" is not a sale Room this machine created.", "Create one with `beam sale room create`.", err)
	}
	limits, err := s.net.Contract.Limits(ctx)
	if err != nil {
		return saleTxError("read the escrow's limits", err)
	}
	switch {
	case price.Cmp(limits.MaxPrice) > 0:
		return typedCLIError(ExitUsage, "sale_price_too_high", fmt.Sprintf("The escrow accepts prices up to %s TAO.", formatTAO(limits.MaxPrice)), "", nil)
	case revealPeriod < limits.MinRevealPeriod || revealPeriod > maxSalePeriod:
		return usage(fmt.Sprintf("--reveal-period must be %d to %d blocks", limits.MinRevealPeriod, maxSalePeriod))
	case challengePeriod < limits.MinChallengePeriod || challengePeriod > maxSalePeriod:
		return usage(fmt.Sprintf("--challenge-period must be %d to %d blocks", limits.MinChallengePeriod, maxSalePeriod))
	}
	usd, err := taoUSD(ctx, quoteText)
	if err != nil {
		return err
	}
	if cents := usdCents(price, usd); cents < minSaleUSDCents {
		return typedCLIError(ExitUsage, "sale_price_too_low", fmt.Sprintf("%s TAO is $%.2f; the smallest sale is $10.", formatTAO(price), float64(cents)/100),
			"Raise --price: a smaller sale costs more in gas and Beam charges than it is worth.", nil)
	}

	binding, err := readBinding(bindingPath)
	if err != nil {
		return err
	}
	if err := binding.Verify(s.net.Domain, roomID); err != nil {
		return typedCLIError(ExitUsage, "sale_binding_invalid", "The buyer binding does not hold for this sale.", "Ask the buyer to run `beam sale join` again.", err)
	}
	if binding.Buyer == s.wallet.Address() || binding.MemberID == room.MemberID {
		return typedCLIError(ExitUsage, "sale_binding_invalid", "The binding names this machine as the buyer.", "", nil)
	}
	members, err := a.saleRoomMembers(ctx, cfg, paths, roomID)
	if err != nil {
		return err
	}
	if err := saleroom.CheckRoom(members, room.MemberID, binding); err != nil {
		return typedCLIError(ExitConflict, "sale_room_not_private", "The sale Room must hold only this machine and the buyer.", "Create a new sale Room for this buyer.", err)
	}
	head, err := s.net.Backend.BlockNumber(ctx)
	if err != nil {
		return saleTxError("read the chain", err)
	}
	if err := s.checkBuyerLimits(ctx, binding.Buyer, price, head); err != nil {
		return err
	}
	if keys, need, err := s.gasKeys(ctx, gasReveal+gasSettle, nil); err != nil {
		return err
	} else if len(keys) == 0 {
		return typedCLIError(ExitOperationFailed, "sale_wallet_no_gas", fmt.Sprintf("The sale wallet needs %s TAO to reveal and settle.", formatTAO(need)), "Fund "+s.wallet.Address().Hex()+".", nil)
	}
	packageBytes := salePackageBytes(size)
	deliveryCents := saleDeliveryCents(int64(packageBytes), room.Delivered == 0)
	if spent, err := s.ledger.Unfunded(); err != nil {
		return cliError(ExitOperationFailed, "Could not read the sale ledger.", "", err)
	} else if spent+deliveryCents > s.wallet.Limits.UnfundedDeliveryCents {
		return saleLimitError(fmt.Errorf("%w ($%.2f of $%.2f used)", salewallet.ErrDeliveryBudget, float64(spent)/100, float64(s.wallet.Limits.UnfundedDeliveryCents)/100))
	}
	connectAgent := func() error {
		client, err := a.readyLocalClient(ctx, cfg)
		if err != nil {
			return err
		}
		return requireSaleCapabilities(ctx, client, "btr.object.publish")
	}
	if err := connectAgent(); err != nil {
		return err
	}

	sale, err := s.buildPackage(ctx, room, binding, file, name, packageDir, price, revealPeriod, challengePeriod, fundWithin, packageBytes, head)
	if err != nil {
		return err
	}
	if err := s.ledger.Deliver(s.wallet.Limits, sale.SaleID, deliveryCents); err != nil {
		_ = os.Remove(sale.Package)
		return saleLimitError(err)
	}
	if err := s.state.Save(sale); err != nil {
		return cliError(ExitOperationFailed, "Could not record the sale.", "", err)
	}
	room.Delivered++
	_ = s.state.SaveRoom(room)
	if err := s.deliverPackage(ctx, sale, room); err != nil {
		return err
	}
	report, err := s.driveSeller(ctx, sale, true, false)
	if err != nil {
		return err
	}
	return renderSaleReport(renderer, report)
}

func readBinding(path string) (saleroom.Binding, error) {
	f, err := os.Open(path)
	if err != nil {
		return saleroom.Binding{}, usage("--binding: " + err.Error())
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return saleroom.Binding{}, usage("--binding: " + err.Error())
	}
	binding, err := saleroom.ParseBinding(data)
	if err != nil {
		return saleroom.Binding{}, typedCLIError(ExitUsage, "sale_binding_invalid", "The buyer binding is not readable.", "Ask the buyer for the file `beam sale join` wrote.", err)
	}
	return binding, nil
}

// checkBuyerLimits applies the seller's limits on whom it delivers to: one unfunded delivery per buyer address at a
// time, and only to an address that holds the price and the gas to fund.
func (s *saleSession) checkBuyerLimits(ctx context.Context, buyer common.Address, price *big.Int, head uint64) error {
	sales, err := s.state.List()
	if err != nil {
		return cliError(ExitOperationFailed, "Could not read the sale records.", "", err)
	}
	for _, other := range sales {
		if other.ChainID == nil || other.ChainID.Cmp(s.net.Domain.ChainID) != 0 || other.Escrow != s.net.Domain.Escrow {
			continue // a sale on another escrow does not hold this one's funds
		}
		if other.Role == salestate.Seller && !other.Ended() && other.Header.Offer.Buyer == buyer &&
			(other.Phase == salestate.PhaseDelivering || other.Phase == salestate.PhaseOffered) && other.Header.Offer.FundDeadline >= head {
			record, err := s.net.Contract.Record(ctx, other.SaleID)
			if err == nil && record.State != escrow.StateNone {
				continue
			}
			return typedCLIError(ExitConflict, "sale_buyer_has_unfunded_offer", "This buyer has not funded the sale "+shortSaleID(other.SaleID)+" yet.",
				fmt.Sprintf("Wait for it to be funded or to expire at block %d.", other.Header.Offer.FundDeadline), nil)
		}
	}
	gasPrice, err := s.net.Backend.SuggestGasPrice(ctx)
	if err != nil {
		return saleTxError("read the gas price", err)
	}
	need := new(big.Int).Add(price, new(big.Int).Mul(gasPrice, big.NewInt(gasFund)))
	balance, err := s.net.Backend.BalanceAt(ctx, buyer, nil)
	if err != nil {
		return saleTxError("read the buyer's balance", err)
	}
	if balance.Cmp(need) < 0 {
		return typedCLIError(ExitConflict, "sale_buyer_cannot_pay", fmt.Sprintf("The buyer %s holds %s TAO, less than the price and gas (%s TAO).", buyer.Hex(), formatTAO(balance), formatTAO(need)),
			"Nothing was delivered. Ask the buyer to fund its sale wallet.", nil)
	}
	return nil
}

// buildPackage hashes, encrypts and signs the dataset into a package named after its sale id.
func (s *saleSession) buildPackage(ctx context.Context, room salestate.Room, binding saleroom.Binding, file, name, packageDir string,
	price *big.Int, revealPeriod, challengePeriod, fundWithin, packageBytes, head uint64) (*salestate.Sale, error) {
	building := filepath.Join(packageDir, ".beam-sale-"+randomHex(8))
	treeDir := building + ".trees"
	defer os.RemoveAll(treeDir)
	nonce := new(big.Int).SetBytes(common.FromHex(randomHex(16)))
	s.renderer.Progress(fmt.Sprintf("Hashing and encrypting %s (%d bytes).", name, packageBytes))
	built, err := saleroom.Build(ctx, saleroom.BuildInput{
		PlainPath: file, PackagePath: building, TreeDir: treeDir, Domain: s.net.Domain, Secret: s.wallet.Secret, Name: name,
		Terms: func(root common.Hash, size uint64) (datasale.Offer, error) {
			// Hashing a large dataset takes many blocks, so the funding deadline counts from the head once it is done.
			now, err := s.net.Backend.BlockNumber(ctx)
			if err != nil {
				return datasale.Offer{}, err
			}
			return datasale.Offer{
				Seller: s.wallet.Address(), Buyer: binding.Buyer, PlainRoot: root, SizeBytes: size, Price: price,
				FundDeadline: now + saleDeliveryBlocks(packageBytes) + fundWithin, RevealPeriod: revealPeriod,
				ChallengePeriod: challengePeriod, Nonce: nonce,
			}, nil
		},
		Sign: func(digest common.Hash) ([]byte, error) { return datasale.SignDigest(s.wallet.Key, digest) },
	})
	if err != nil {
		return nil, cliError(ExitOperationFailed, "Could not build the sale package.", "Check the dataset and that --package-dir has room for a copy of it.", err)
	}
	final := filepath.Join(packageDir, saleroom.PackageName(built.SaleID))
	if err := os.Rename(building, final); err != nil {
		_ = os.Remove(building)
		return nil, cliError(ExitOperationFailed, "Could not name the sale package.", "", err)
	}
	return &salestate.Sale{
		SaleID: built.SaleID, Role: salestate.Seller, ChainID: s.net.Domain.ChainID, Escrow: s.net.Domain.Escrow,
		RoomID: room.RoomID, MemberID: binding.MemberID, Header: built.Header, StartBlock: head,
		Phase: salestate.PhaseDelivering, Package: final,
	}, nil
}

// keyNotHeldReason is the delivery failure a buyer's agent reports for a package sealed under a channel key it does not
// hold yet, as when the seller publishes right after the buyer joined.
const keyNotHeldReason = "agent_reported_object_key_unavailable"

// maxPublishAttempts bounds how many times one package is published to a buyer whose agent did not hold the key yet.
const maxPublishAttempts = 3

// deliverPackage publishes the package to the buyer's member and follows the delivery to its end. It publishes again,
// after one poll interval, when the buyer's agent did not hold the channel key yet. A recorded publication, from an
// interrupted run, is followed before any new one.
func (s *saleSession) deliverPackage(ctx context.Context, sale *salestate.Sale, room salestate.Room) error {
	target := saleRoom{RoomID: room.RoomID, SellerMemberID: room.MemberID, ChannelID: room.ChannelID}
	for attempt := 1; ; attempt++ {
		if sale.PublicationID == "" {
			key := "sale-" + shortSaleID(sale.SaleID)
			if attempt > 1 {
				key += "-" + strconv.Itoa(attempt)
			}
			publication, err := s.app.publishSalePackage(ctx, s.cfg, s.paths, target, sale.Package, sale.MemberID, key)
			if err != nil {
				return err
			}
			sale.PublicationID = publication
			if err := s.state.Save(sale); err != nil {
				return cliError(ExitOperationFailed, "Could not record the sale.", "", err)
			}
		}
		delivery, err := s.followDelivery(ctx, sale, target)
		if err != nil {
			return err
		}
		if delivery.Delivered() {
			sale.Phase = salestate.PhaseOffered
			if err := s.state.Save(sale); err != nil {
				return cliError(ExitOperationFailed, "Could not record the sale.", "", err)
			}
			// The buyer holds the package now, and nothing in the sale needs the seller's copy again.
			if err := os.Remove(sale.Package); err != nil && !errors.Is(err, os.ErrNotExist) {
				s.renderer.Progress("Could not remove the delivered package " + sale.Package + ": " + err.Error())
			}
			s.renderer.Progress("Delivered the package to the buyer.")
			return nil
		}
		if delivery.Reason != keyNotHeldReason || attempt >= maxPublishAttempts {
			sale.Phase = salestate.PhaseDeliveryFailed
			_ = s.state.Save(sale)
			return describeDeliveryFailure(delivery)
		}
		s.renderer.Progress("The buyer's agent did not hold the channel's key yet, as just after it joins. Publishing again.")
		sale.PublicationID = ""
		if err := s.app.saleSleep(ctx); err != nil {
			return err
		}
	}
}

// followDelivery waits for one publication's delivery to the buyer to end, and reports how it ended.
func (s *saleSession) followDelivery(ctx context.Context, sale *salestate.Sale, target saleRoom) (saleDelivery, error) {
	said := ""
	for {
		delivery, err := s.app.salePackageDelivery(ctx, s.cfg, s.paths, target, sale.PublicationID, sale.MemberID)
		if err != nil {
			return saleDelivery{}, err
		}
		if delivery.Done() {
			return delivery, nil
		}
		if state := firstNonEmpty(delivery.MemberState, delivery.TransferState); state != said {
			s.renderer.Progress("Delivering to the buyer: " + state + ".")
			said = state
		}
		if err := s.app.saleSleep(ctx); err != nil {
			return saleDelivery{}, err
		}
	}
}

// driveSeller carries the seller's side forward from the escrow's state: reveal once funded, settle once the
// challenge window has closed. It waits for funding and for the challenge window only when asked to.
func (s *saleSession) driveSeller(ctx context.Context, sale *salestate.Sale, waitFunding, waitSettle bool) (saleReport, error) {
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
			if !waitFunding {
				return saleReportOf(*sale, &record, head), nil
			}
			say(fmt.Sprintf("Waiting for the buyer to fund, until block %d (%s).", offer.FundDeadline, blocksAway(head, offer.FundDeadline)))
		case escrow.StateFunded:
			if err := s.ledger.Funded(sale.SaleID); err != nil {
				s.renderer.Progress("Could not update the delivery budget: " + err.Error())
			}
			// The key is revealed only on funding that cannot be undone: a funding block dropped from the chain
			// would leave a revealed key with nothing paid.
			final, err := s.finalRecord(ctx, sale.SaleID)
			if err != nil && !errors.Is(err, escrow.ErrUnreachable) {
				return saleReport{}, saleTxError("read the final sale", err)
			}
			if err != nil || final.State != escrow.StateFunded {
				say("The buyer funded the sale. Waiting for the funding to be final.")
				break
			}
			say("The buyer's funding is final. Revealing the key.")
			if err := s.act(ctx, say, func() error { return s.reveal(ctx, sale, record, head) }); err != nil {
				return saleReport{}, err
			}
			continue
		case escrow.StateKeyReleased:
			if head > record.Deadline {
				say("The challenge window has closed. Settling the sale.")
				if err := s.act(ctx, say, func() error { return s.settle(ctx, sale) }); err != nil {
					return saleReport{}, err
				}
				continue
			}
			if !waitSettle {
				return saleReportOf(*sale, &record, head), nil
			}
			say(fmt.Sprintf("The key is revealed. The sale settles after block %d (%s).", record.Deadline, blocksAway(head, record.Deadline+1)))
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

func saleCommitments(sale *salestate.Sale) escrow.Commitments {
	return escrow.Commitments{CipherRoot: sale.Header.CipherRoot, KeyCommitment: sale.Header.KeyCommitment}
}

// reveal publishes the sale key. Every condition the escrow checks is checked here first, because a reveal that
// reverts still makes the key public: the funded commitments are the ones this wallet signed, the key matches its
// commitment, and the deadline leaves the safety margin. The key is never simulated through an RPC endpoint.
func (s *saleSession) reveal(ctx context.Context, sale *salestate.Sale, record escrow.Record, head uint64) error {
	key := datasale.SellerKey(s.wallet.Secret, sale.SaleID)
	commitments := saleCommitments(sale)
	margin := s.app.saleSafetyMargin()
	// A reveal already broadcast made the key public, so getting one included comes before every other rule.
	exposed := len(sale.Transactions["reveal"]) > 0 || len(sale.Transactions["reveal.reverted"]) > 0
	saleID, idErr := datasale.SaleID(s.net.Domain, sale.Header.Offer)
	switch {
	case idErr != nil || saleID != sale.SaleID:
		return typedCLIError(ExitConflict, "sale_record_mismatch", "The sale's record does not match its signed offer, so the key is not revealed.", "", idErr)
	case record.Commitments != commitments.Hash():
		return typedCLIError(ExitConflict, "sale_commitments_mismatch", "The sale was funded with commitments this wallet did not sign, so the key is not revealed.", "", nil)
	case datasale.KeyCommitment(sale.SaleID, key) != commitments.KeyCommitment:
		return typedCLIError(ExitConflict, "sale_key_mismatch", "This wallet does not hold the sale's key, so it is not revealed.", "Resume the sale from the wallet that offered it.", nil)
	case !exposed && head+margin > record.Deadline:
		return typedCLIError(ExitOperationFailed, "sale_too_late", fmt.Sprintf("It is too late to reveal the key safely: the deadline is block %d.", record.Deadline),
			"The key stays secret. The buyer can reclaim its payment after the deadline.", nil)
	}
	keys, need, err := s.gasKeys(ctx, gasReveal, nil)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		if !exposed {
			return typedCLIError(ExitOperationFailed, "sale_wallet_no_gas", fmt.Sprintf("No account of the sale wallet holds the %s TAO a reveal needs.", formatTAO(need)),
				fmt.Sprintf("Fund %s; the reveal deadline is block %d.", s.wallet.Address().Hex(), record.Deadline), nil)
		}
		keys = []*ecdsa.PrivateKey{s.wallet.Key}
		if s.wallet.Backup != nil {
			keys = append(keys, s.wallet.Backup)
		}
	}
	if exposed && head+margin > record.Deadline {
		// Inside the margin only the deadline itself still holds the reveal back.
		margin = 0
	}
	data, err := escrow.RevealData(sale.Header.Offer, commitments, key)
	if err != nil {
		return err
	}
	_, err = s.transact(ctx, sale, s.sender(keys[0], keys[1:]...), "reveal", escrow.Request{
		Data: data, GasLimit: gasReveal, Deadline: record.Deadline, Margin: margin, Secret: true, AnySender: true,
	})
	if err != nil {
		return saleTxError("reveal the key", err)
	}
	s.renderer.Progress("Revealed the key.")
	return nil
}

func (s *saleSession) settle(ctx context.Context, sale *salestate.Sale) error {
	keys, need, err := s.gasKeys(ctx, gasSettle, nil)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return typedCLIError(ExitOperationFailed, "sale_wallet_no_gas", fmt.Sprintf("No account of the sale wallet holds the %s TAO settling needs.", formatTAO(need)), "Fund "+s.wallet.Address().Hex()+".", nil)
	}
	data, err := escrow.SettleData(sale.Header.Offer)
	if err != nil {
		return err
	}
	if _, err := s.transact(ctx, sale, s.sender(keys[0], keys[1:]...), "settle", escrow.Request{Data: data, GasLimit: gasSettle, AnySender: true}); err != nil {
		return saleTxError("settle the sale", err)
	}
	return nil
}

// endSale records the sale's final state and removes the working files it no longer needs: the seller's package, and
// the buyer's proof trees and delivered package.
func (s *saleSession) endSale(ctx context.Context, sale *salestate.Sale, outcome string, record escrow.Record, head uint64) (saleReport, error) {
	if !sale.Ended() {
		sale.Phase = salestate.PhaseEnded
		sale.Outcome = outcome
		if sale.Package != "" {
			_ = os.Remove(sale.Package)
		}
		if sale.TreeDir != "" {
			_ = os.RemoveAll(sale.TreeDir)
		}
		if sale.Role == salestate.Buyer {
			s.dropDelivered(ctx, sale)
		}
		if err := s.state.Save(sale); err != nil {
			return saleReport{}, cliError(ExitOperationFailed, "Could not record the sale.", "", err)
		}
	}
	report := saleReportOf(*sale, &record, head)
	if outcome == salestate.OutcomeSettled && sale.Role == salestate.Seller || outcome == salestate.OutcomeRefunded && sale.Role == salestate.Buyer {
		if credit, err := s.net.Contract.Credit(ctx, s.wallet.Address()); err == nil && credit.Sign() > 0 {
			report.Next = fmt.Sprintf("the escrow holds %s TAO for this wallet: collect it with `beam sale withdraw`", formatTAO(credit))
		}
	}
	return report, nil
}

// saleRevoke withdraws an offer the buyer has not funded.
func (a *App) saleRevoke(ctx context.Context, args []string, cfg config.Config, paths config.Paths, renderer output.Renderer) error {
	if len(args) != 1 {
		return usage("sale revoke requires one SALE_ID")
	}
	s, err := a.openSaleSession(ctx, cfg, paths, renderer)
	if err != nil {
		return err
	}
	sale, err := s.findSale(args[0])
	if err != nil {
		return err
	}
	if sale.Role != salestate.Seller {
		return usage("only the seller can revoke an offer")
	}
	record, _, err := s.chainState(ctx, sale.SaleID)
	if err != nil {
		return err
	}
	if record.State != escrow.StateNone {
		return typedCLIError(ExitConflict, "sale_not_revocable", "The sale is "+record.State.String()+"; only an offer nobody funded can be revoked.", "Run `beam sale resume "+shortSaleID(sale.SaleID)+"`.", nil)
	}
	keys, need, err := s.gasKeys(ctx, gasRevoke, nil)
	if err != nil {
		return err
	}
	if len(keys) == 0 || keys[0] != s.wallet.Key {
		return typedCLIError(ExitOperationFailed, "sale_wallet_no_gas", fmt.Sprintf("The sale wallet needs %s TAO to revoke.", formatTAO(need)), "Fund "+s.wallet.Address().Hex()+".", nil)
	}
	data, err := escrow.RevokeData(sale.Header.Offer)
	if err != nil {
		return err
	}
	if _, err := s.transact(ctx, &sale, s.sender(s.wallet.Key), "revoke", escrow.Request{Data: data, GasLimit: gasRevoke}); err != nil {
		if record, _, readErr := s.chainState(ctx, sale.SaleID); readErr == nil && record.State == escrow.StateFunded {
			return typedCLIError(ExitConflict, "sale_funded", "The buyer funded the sale before it could be revoked.",
				"Reveal the key with `beam sale resume "+shortSaleID(sale.SaleID)+"` to be paid.", err)
		}
		return saleTxError("revoke the offer", err)
	}
	record, head, err := s.chainState(ctx, sale.SaleID)
	if err != nil {
		return err
	}
	report, err := s.endSale(ctx, &sale, salestate.OutcomeRevoked, record, head)
	if err != nil {
		return err
	}
	return renderSaleReport(renderer, report)
}

func (s *saleSession) findSale(id string) (salestate.Sale, error) {
	sale, err := s.state.Find(id)
	if errors.Is(err, salestate.ErrNotFound) {
		return salestate.Sale{}, typedCLIError(ExitNotFound, "sale_unknown", "No sale "+id+" on this machine.", "List them with `beam sale status`.", nil)
	}
	if err != nil {
		return salestate.Sale{}, cliError(ExitOperationFailed, "Could not read the sale record.", "", err)
	}
	if sale.ChainID == nil || sale.ChainID.Cmp(s.net.Domain.ChainID) != 0 || sale.Escrow != s.net.Domain.Escrow {
		return salestate.Sale{}, typedCLIError(ExitConfig, "sale_other_escrow", fmt.Sprintf("The sale belongs to the escrow %s on chain %s, not the configured one.", sale.Escrow.Hex(), sale.ChainID),
			"Select the context configured for that escrow with --context.", nil)
	}
	return sale, nil
}

func saleLimitError(err error) error {
	return typedCLIError(ExitOperationFailed, "sale_limit", strings.ToUpper(err.Error()[:1])+err.Error()[1:]+".",
		"See the limits with `beam sale wallet show`; a person can raise them with `beam sale wallet limits`.", err)
}
