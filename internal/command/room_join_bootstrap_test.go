package command

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadRoomJoinBootstrapOptionsUsesOwnerOnlyInvitationFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invitation.token")
	if err := os.WriteFile(path, []byte("btri_secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	token, leaseTTL, key, err := readRoomJoinBootstrapOptions([]string{
		"btr_room_test", "--invitation-file", path, "--lease-ttl", "90", "--idempotency-key", "join-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if token != "btri_secret" || leaseTTL != 90 || key != "join-test" {
		t.Fatalf("options token=%q lease=%d key=%q", token, leaseTTL, key)
	}
}

func TestRemoveRoomJoinCoordinatorOption(t *testing.T) {
	args := removeRoomJoinOption([]string{
		"btr_room_test", "--coordinator=https://coordinator.test", "--invitation-token", "btri_secret",
	}, "--coordinator")
	want := []string{"btr_room_test", "--invitation-token", "btri_secret"}
	if len(args) != len(want) {
		t.Fatalf("clean args=%q", args)
	}
	for index := range want {
		if args[index] != want[index] {
			t.Fatalf("clean args=%q", args)
		}
	}
}
