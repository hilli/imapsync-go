package compat_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheGeneratedOptionDocIsCurrent fails when docs/imapsync-options.md no
// longer matches the table it is generated from.
//
// Generating the doc is only worth anything if something notices when it was
// not regenerated. Without this test the file would be exactly as stale as a
// hand-written one, and more convincing for looking machine-made: a reader
// would trust "refused" in a table that had been true six commits ago.
//
// It regenerates into a temporary copy of docs/ rather than over the real file,
// so a failing test reports the drift instead of quietly repairing it.
func TestTheGeneratedOptionDocIsCurrent(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary to capture its help output")
	}

	root := moduleRoot(t)
	committed, err := os.ReadFile(filepath.Join(root, "docs", "imapsync-options.md"))
	if err != nil {
		t.Fatalf("reading the committed doc: %v (run `go generate ./...`)", err)
	}

	// The generator writes to <root>/docs, so it is pointed at a scratch root
	// holding a docs directory and nothing else.
	scratch := t.TempDir()
	if err := os.Mkdir(filepath.Join(scratch, "docs"), 0o755); err != nil {
		t.Fatalf("preparing the scratch root: %v", err)
	}

	cmd := exec.CommandContext(t.Context(), "go", "run", "./internal/docsgen")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "DOCSGEN_OUT="+scratch)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("running the generator: %v\n%s", err, out)
	}

	regenerated, err := os.ReadFile(filepath.Join(scratch, "docs", "imapsync-options.md"))
	if err != nil {
		t.Fatalf("reading the regenerated doc: %v", err)
	}

	if string(committed) != string(regenerated) {
		t.Errorf("docs/imapsync-options.md is stale: run `go generate ./...` and commit the result.\n%s",
			firstDifference(string(committed), string(regenerated)))
	}
}

// firstDifference reports the first line that differs, which is far more use
// than a diff of two 500-line files.
func firstDifference(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(al) && i < len(bl); i++ {
		if al[i] != bl[i] {
			return fmt.Sprintf("first difference at line %d:\n  committed:   %s\n  regenerated: %s",
				i+1, al[i], bl[i])
		}
	}
	return fmt.Sprintf("the files differ in length: committed has %d lines, regenerated has %d",
		len(al), len(bl))
}

// moduleRoot walks up from the test's directory to the one holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}
