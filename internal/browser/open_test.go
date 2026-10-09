package browser

import "testing"

func TestOpenRejectsNonHTTPURLWithoutLaunchingProcess(t *testing.T) {
	for _, rawURL := range []string{
		"file:///tmp/credentials.json",
		"javascript:alert(1)",
		"https://",
	} {
		if err := Open(rawURL); err == nil {
			t.Fatalf("Open(%q) unexpectedly succeeded", rawURL)
		}
	}
}

func TestCommandUsesURLAsSeparateArgument(t *testing.T) {
	rawURL := "https://auth.beam.network/connect?user_code=BEAM-CODE"
	name, args, err := command(rawURL)
	if err != nil {
		t.Skipf("platform has no browser launcher: %v", err)
	}
	if name == "" || len(args) == 0 || args[len(args)-1] != rawURL {
		t.Fatalf("command=%q args=%q", name, args)
	}
}
