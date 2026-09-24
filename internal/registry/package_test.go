package registry

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCanonicalManifestMatchesRegistryJSONStringify(t *testing.T) {
	got, err := CanonicalManifest(map[string]any{"text": "Research & development <safe>\u2028next\u2029line"})
	if err != nil {
		t.Fatal(err)
	}
	want := "{\"text\":\"Research & development <safe>\u2028next\u2029line\"}"
	if string(got) != want {
		t.Fatalf("canonical = %q, want %q", got, want)
	}
	literal, err := CanonicalManifest(map[string]any{"text": `\u2028`})
	if err != nil {
		t.Fatal(err)
	}
	if string(literal) != `{"text":"\\u2028"}` {
		t.Fatalf("escaped literal changed: %q", literal)
	}
}

func TestInspectRejectsInvalidLegacyVersion(t *testing.T) {
	root := t.TempDir()
	manifest := `{"name":"@beam/example","version":"not-semver","apiVersion":"workflow-actions/v1","entrypoint":"index.mjs","runtime":{"placements":["local-workers"]}}`
	if err := os.WriteFile(filepath.Join(root, "beam-action.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.mjs"), []byte("export const execute = () => ({});"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Inspect(root); err == nil || err.Error() != "manifest version must be an exact semver version" {
		t.Fatalf("Inspect error = %v", err)
	}
}

func TestInspectAndPack(t *testing.T) {
	root := t.TempDir()
	manifest := `{
  "name": "@beam/example",
  "version": "1.2.3",
  "apiVersion": "workflow-actions/v1",
  "entrypoint": "dist/index.mjs",
  "runtime": {"placements": ["local-workers"]},
  "permissions": ["filesystem:read"]
}`
	if err := os.MkdirAll(filepath.Join(root, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "beam-action.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dist", "index.mjs"), []byte("export const x = 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "example.tgz")
	artifact, err := Pack(root, output)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Package != "@beam/example" || artifact.SizeBytes == 0 || artifact.ArtifactChecksum == "" {
		t.Fatalf("artifact = %#v", artifact)
	}
}

func TestInspectRejectsEntrypointTraversal(t *testing.T) {
	root := t.TempDir()
	manifest := `{
  "name": "@beam/example",
  "version": "1.0.0",
  "apiVersion": "workflow-actions/v1",
  "entrypoint": "../secret",
  "runtime": {"placements": ["local-workers"]}
}`
	if err := os.WriteFile(filepath.Join(root, "beam-action.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Inspect(root); err == nil {
		t.Fatal("expected traversal error")
	}
}

func TestPackDoesNotArchiveItsExistingOutput(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{
  "name":"@beam/example",
  "version":"1.0.0",
  "apiVersion":"workflow-actions/v1",
  "entrypoint":"dist/index.mjs",
  "runtime":{"placements":["local-workers"]}
}`
	if err := os.WriteFile(filepath.Join(root, "beam-action.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dist", "index.mjs"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "package.tgz")
	if err := os.WriteFile(output, []byte("old package"), 0o644); err != nil {
		t.Fatal(err)
	}
	artifact, err := Pack(root, output)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Files != 2 {
		t.Fatalf("files=%d, want 2", artifact.Files)
	}
}
