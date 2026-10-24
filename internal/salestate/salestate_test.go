package salestate

import (
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Beam-Network/beam-cli-public/internal/escrow"
	"github.com/ethereum/go-ethereum/common"
)

func TestSalesAreFoundByPrefixAndListedNewestFirst(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	older := &Sale{SaleID: common.HexToHash("0xaa01" + strings.Repeat("0", 60)), Role: Seller, ChainID: big.NewInt(945), Phase: PhaseOffered}
	newer := &Sale{SaleID: common.HexToHash("0xaa02" + strings.Repeat("0", 60)), Role: Buyer, ChainID: big.NewInt(945), Phase: PhaseChecked}
	if err := store.Save(older); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if err := store.Save(newer); err != nil {
		t.Fatal(err)
	}
	if err := store.AddAttempt(newer, "fund", escrow.Attempt{Hash: common.HexToHash("0x01"), Nonce: 7}); err != nil {
		t.Fatal(err)
	}
	sales, err := store.List()
	if err != nil || len(sales) != 2 || sales[0].SaleID != newer.SaleID {
		t.Fatalf("List = %+v, %v", sales, err)
	}
	if got := sales[0].Transactions["fund"]; len(got) != 1 || got[0].Nonce != 7 {
		t.Fatalf("the recorded attempt is %+v", got)
	}
	found, err := store.Find("0xAA02")
	if err != nil || found.SaleID != newer.SaleID {
		t.Fatalf("Find by prefix = %+v, %v", found, err)
	}
	if _, err := store.Find("aa0"); err == nil {
		t.Fatal("a prefix shared by both sales matched one")
	}
	if _, err := store.Find("cc"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Find of an unknown sale = %v", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(store.SaleDir(newer.SaleID), "sale.json"))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("the record file is %v, %v; want owner-only", info.Mode(), err)
		}
	}
}

func TestRoomRecordsRefuseIdsThatLeaveTheirDirectory(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	if err := store.SaveRoom(Room{RoomID: "btr_room_1", Role: Seller, ChannelID: "btr_channel_1", MemberID: "btr_member_1"}); err != nil {
		t.Fatal(err)
	}
	room, err := store.LoadRoom("btr_room_1")
	if err != nil || room.ChannelID != "btr_channel_1" {
		t.Fatalf("LoadRoom = %+v, %v", room, err)
	}
	for _, id := range []string{"", "../escape", "a/b", "..", "x.json"} {
		if err := store.SaveRoom(Room{RoomID: id}); err == nil {
			t.Errorf("SaveRoom(%q) was accepted", id)
		}
	}
	if _, err := store.LoadRoom("btr_room_2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LoadRoom of an unknown Room = %v", err)
	}
}
