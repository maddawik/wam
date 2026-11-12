package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// holdOpen opens path the way a running WoW holds its addon files.
func holdOpen(t *testing.T, path string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
}

func assertInUse(t *testing.T, stderr string, code int) {
	t.Helper()
	if code != 5 || !strings.Contains(stderr, "Alpha") || !strings.Contains(stderr, "close WoW and retry") {
		t.Errorf("exit = %d, stderr = %q; want 5 naming Alpha and asking to close WoW", code, stderr)
	}
}

func TestSyncUpgradeOfAFolderInUseExitsFiveAndKeepsIt(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{
		"acme/alpha": {
			{Tag: "v2", Assets: map[string][]byte{"a.zip": alphaZip(t, "v2")}},
			{Tag: "v1", Assets: map[string][]byte{"a.zip": alphaZip(t, "v1")}},
		},
	})
	e := newSyncEnv(t, gh, pinned("v1"))
	if r := e.sync(t); r.ExitCode != 0 {
		t.Fatalf("v1: exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	toc := filepath.Join(e.addons, "Alpha", "Alpha.toc")
	holdOpen(t, toc)
	write(t, e.manifest, pinned("v2"))

	r := e.sync(t)
	assertInUse(t, r.Stderr, r.ExitCode)
	if got := read(t, toc); got != "v1" {
		t.Errorf("Alpha.toc = %q, want v1 untouched", got)
	}
	assertNoTempFiles(t, e.addons)
}

func TestSyncRemovingAFolderInUseExitsFiveAndKeepsIt(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{
		"acme/alpha": {{Tag: "v1", Assets: map[string][]byte{"a.zip": alphaZip(t, "v1")}}},
	})
	e := newSyncEnv(t, gh, oneAddon)
	if r := e.sync(t); r.ExitCode != 0 {
		t.Fatalf("exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	toc := filepath.Join(e.addons, "Alpha", "Alpha.toc")
	holdOpen(t, toc)
	write(t, e.manifest, "[game]\nflavor = \"mainline\"\n\n[addons]\n")

	r := e.sync(t)
	assertInUse(t, r.Stderr, r.ExitCode)
	if got := read(t, toc); got != "v1" {
		t.Errorf("Alpha.toc = %q, want it kept", got)
	}
}
