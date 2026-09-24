package command

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Beam-Network/beam-cli-public/internal/version"
)

func TestActionRunWithExplicitFixtures(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node.js is not installed")
	}
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	actionDirectory := createTestAction(t)
	t.Chdir(actionDirectory)
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(
		context.Background(),
		[]string{
			"action", "run", ".",
			"--config", "fixtures/config.json",
			"--inputs", "fixtures/input.json",
			"--secrets", "fixtures/secrets.json",
		},
		&stdout,
		&stderr,
	)
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	var result struct {
		Outputs struct {
			Total      float64 `json:"total"`
			SecretSeen bool    `json:"secretSeen"`
		} `json:"outputs"`
		State     map[string]any `json:"state"`
		Artifacts []any          `json:"artifacts"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("invalid action result: %v\n%s", err, stdout.String())
	}
	if result.Outputs.Total != 5 || !result.Outputs.SecretSeen {
		t.Fatalf("unexpected action result: %#v", result.Outputs)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("[info] test action ran")) {
		t.Fatalf("missing action log: %s", stderr.String())
	}
	if bytes.Contains(stdout.Bytes(), []byte("must-never-be-printed")) ||
		bytes.Contains(stderr.Bytes(), []byte("must-never-be-printed")) {
		t.Fatal("action secret leaked to command output")
	}
}

func TestActionRunJSONSuppressesLogs(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node.js is not installed")
	}
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	actionDirectory := createTestAction(t)
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(
		context.Background(),
		[]string{"--json", "action", "run", actionDirectory},
		&stdout,
		&stderr,
	)
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("JSON command wrote stderr: %s", stderr.String())
	}
	var result map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("invalid JSON output: %v\n%s", err, stdout.String())
	}
}

func TestActionRunRejectsUnknownOption(t *testing.T) {
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(
		context.Background(),
		[]string{"action", "run", ".", "--wat"},
		&stdout,
		&stderr,
	)
	if code != ExitUsage {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
}

func TestActionRunReportsMissingNode(t *testing.T) {
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	actionDirectory := createTestAction(t)
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(
		context.Background(),
		[]string{"action", "run", actionDirectory},
		&stdout,
		&stderr,
	)
	if code != ExitOperationFailed {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !bytes.Contains(stderr.Bytes(), []byte("Node.js is required")) {
		t.Fatalf("missing Node.js diagnosis: %s", stderr.String())
	}
}

func TestActionHelpDocumentsRun(t *testing.T) {
	t.Setenv("BEAM_CONFIG_DIR", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := New(version.BuildInfo{Version: "test"}).Run(
		context.Background(),
		[]string{"help", "action"},
		&stdout,
		&stderr,
	)
	if code != ExitOK {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	for _, expected := range []string{version.CLIName() + " action", "run [directory]", "--config", "--inputs", "--secrets"} {
		if !bytes.Contains(stdout.Bytes(), []byte(expected)) {
			t.Fatalf("action help missing %q:\n%s", expected, stdout.String())
		}
	}
}

func createTestAction(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, directory := range []string{"dist", "fixtures"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(root, "beam-action.json"), `{
  "name": "@beam/test-action",
  "version": "1.0.0",
  "apiVersion": "workflow-actions/v1",
  "runtime": {"type": "node", "placements": ["local-workers"]},
  "entrypoint": "dist/index.mjs",
  "permissions": []
}`)
	writeTestFile(t, filepath.Join(root, "dist", "index.mjs"), `
export async function execute({ config, inputs }, context) {
  context.logger.info("test action ran", { fixture: true });
  const token = await context.secrets.get("token");
  return {
    outputs: {
      total: Number(config.base || 0) + Number(inputs.value || 0),
      secretSeen: token === "must-never-be-printed"
    }
  };
}
`)
	writeTestFile(t, filepath.Join(root, "fixtures", "config.json"), `{"base":2}`)
	writeTestFile(t, filepath.Join(root, "fixtures", "input.json"), `{"value":3}`)
	writeTestFile(t, filepath.Join(root, "fixtures", "secrets.json"), `{"token":"must-never-be-printed"}`)
	return root
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
