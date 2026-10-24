package registry

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var packProjectFiles = []string{
	"beam-action.json",
	"beam-project.json",
	"package.json",
	"README.md",
	"dist/index.mjs",
	"dist/index.mjs.map",
	"dist/helper.mjs",
	"dist/.env",
	"src/index.ts",
	"docs/guide.md",
	".env",
	".env.production",
	".npmrc",
	"config.local",
	"key.pem",
	"id_ed25519",
	"id_rsa.pub",
	"id_ecdsa-cert.pub",
	"keys/id_dsa-cert",
	"id_utils.mjs",
	"src/util.test.ts",
	"dist/index.spec.mjs",
	".DS_Store",
	"test/a.js",
	"nested/tests/b.js",
	"nested/__tests__/c.js",
	"node_modules/x/index.js",
	"fixtures/secrets.local.json",
	".git/config",
	".beam-packages/old.tgz",
}

func selectedPackFiles(t *testing.T, extra map[string]string, entrypoint string) ([]string, error) {
	t.Helper()
	root := t.TempDir()
	contents := map[string]string{
		"beam-project.json": `{"entrypoint":"dist/index.mjs"}` + "\n",
		"package.json":      "{}\n",
	}
	for file, content := range extra {
		contents[file] = content
	}
	files := append([]string{}, packProjectFiles...)
	for file := range extra {
		files = append(files, file)
	}
	for _, file := range files {
		path := filepath.Join(root, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		content, ok := contents[file]
		if !ok {
			content = "x\n"
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return packageFiles(root, entrypoint)
}

func assertPackFiles(t *testing.T, extra map[string]string, want []string) {
	t.Helper()
	got, err := selectedPackFiles(t, extra, "dist/index.mjs")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("files = %q\nwant    %q", got, want)
	}
}

func TestPackDenyListDropsSecretsTestsDependenciesAndLocalFiles(t *testing.T) {
	assertPackFiles(t, nil, []string{
		"README.md",
		"beam-action.json",
		"beam-project.json",
		"dist/helper.mjs",
		"dist/index.mjs",
		"docs/guide.md",
		"id_utils.mjs",
		"package.json",
		"src/index.ts",
	})
}

func TestBeamignoreExcludesMoreButCannotReincludeDeniedFiles(t *testing.T) {
	assertPackFiles(t, map[string]string{
		".beamignore": "# published elsewhere\ndocs/\n*.ts\n/README.md\n!.env\n!test/a.js\n",
	}, []string{"beam-action.json", "beam-project.json", "dist/helper.mjs", "dist/index.mjs", "id_utils.mjs", "package.json"})
}

func TestPackageJSONFilesIsAnIncludeListThatKeepsManifestAndEntrypoint(t *testing.T) {
	assertPackFiles(t, map[string]string{
		"package.json": `{"files":["dist/","!dist/helper.mjs"]}` + "\n",
	}, []string{"beam-action.json", "beam-project.json", "dist/index.mjs", "package.json"})
	assertPackFiles(t, map[string]string{
		"package.json": `{"files":["src"]}` + "\n",
		".beamignore":  "dist/\n",
	}, []string{"beam-action.json", "beam-project.json", "dist/index.mjs", "package.json", "src/index.ts"})
	if _, err := selectedPackFiles(t, map[string]string{"package.json": `{"files":"dist"}`}, "dist/index.mjs"); err == nil || !strings.Contains(err.Error(), "files must be an array of strings") {
		t.Fatalf("error = %v", err)
	}
}

func TestPackRefusesAnEntrypointInsideADeniedDirectory(t *testing.T) {
	_, err := selectedPackFiles(t, nil, "test/a.js")
	if err == nil || !strings.Contains(err.Error(), `test/a.js is required`) || !strings.Contains(err.Error(), `"test/" directory`) {
		t.Fatalf("error = %v", err)
	}
	_, err = selectedPackFiles(t, map[string]string{"dist/main.test.mjs": "x\n"}, "dist/main.test.mjs")
	if err == nil || !strings.Contains(err.Error(), `dist/main.test.mjs is required`) || !strings.Contains(err.Error(), `"main.test.mjs" matches a file name`) {
		t.Fatalf("error = %v", err)
	}
}
