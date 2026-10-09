package command

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/version"
)

func TestSharedV2PackageInspectPackVerify(t *testing.T) {
	fixtures := os.Getenv("BEAM_REGISTRY_FIXTURES")
	if fixtures == "" {
		t.Skip("set BEAM_REGISTRY_FIXTURES to the Website registry-core fixtures directory")
	}
	root := filepath.Join(t.TempDir(), "action")
	materialize := exec.Command("node", filepath.Join(fixtures, "materialize-project.mjs"), "v2-single.valid.json", root)
	if output, err := materialize.CombinedOutput(); err != nil {
		t.Fatalf("materialize: %s: %v", output, err)
	}
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	app := New(version.BuildInfo{Version: "test"})
	archive := filepath.Join(t.TempDir(), "action.tgz")
	for _, args := range [][]string{
		{"registry", "inspect", root, "--json"},
		{"registry", "pack", root, "--out", archive, "--json"},
		{"registry", "verify", archive, "--json"},
	} {
		var stdout, stderr bytes.Buffer
		if code := app.Run(context.Background(), args, &stdout, &stderr); code != ExitOK {
			t.Fatalf("%v: code=%d stderr=%s", args, code, stderr.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
			t.Fatalf("%v: %s: %v", args, stdout.String(), err)
		}
		if args[1] == "verify" && payload["valid"] != true {
			t.Fatalf("verify result: %#v", payload)
		}
	}
}

func TestVerifyRejectsSharedUnknownCapability(t *testing.T) {
	fixtures := os.Getenv("BEAM_REGISTRY_FIXTURES")
	if fixtures == "" {
		t.Skip("set BEAM_REGISTRY_FIXTURES to the Website registry-core fixtures directory")
	}
	root := filepath.Join(t.TempDir(), "action")
	materialize := exec.Command("node", filepath.Join(fixtures, "materialize-project.mjs"), "v2-unknown-capability.invalid.json", root)
	if output, err := materialize.CombinedOutput(); err != nil {
		t.Fatalf("materialize: %s: %v", output, err)
	}
	archive := filepath.Join(t.TempDir(), "invalid.tgz")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)
	for _, name := range []string{"beam-action.json", "beam-project.json", "dist/index.mjs"} {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(context.Background(), []string{"registry", "verify", archive}, &stdout, &stderr)
	if code != ExitUsage || !strings.Contains(stderr.String(), "Archive manifest is invalid") {
		t.Fatalf("verify code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}
