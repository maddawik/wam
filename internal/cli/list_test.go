package cli_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maddawik/wam/internal/lock"
)

func TestListShowsInstalledAddon(t *testing.T) {
	gh := fakeGitHub(t, alphaReleases(t))
	e := newSyncEnv(t, gh, oneAddon)
	e.sync(t)

	r := e.cmd(t, "list")
	if r.ExitCode != 0 {
		t.Fatalf("exit %d: %s", r.ExitCode, r.Stderr)
	}
	if got := strings.Fields(r.Stdout); strings.Join(got, " ") != "github:acme/alpha latest v2 ✓ installed" {
		t.Errorf("stdout = %q", r.Stdout)
	}
}

func TestListShowsMissingWhenFoldersAreAbsent(t *testing.T) {
	gh := fakeGitHub(t, alphaReleases(t))
	e := newSyncEnv(t, gh, oneAddon)
	e.sync(t)
	if err := os.RemoveAll(filepath.Join(e.addons, "Alpha")); err != nil {
		t.Fatal(err)
	}
	r := e.cmd(t, "list")
	if got := strings.Join(strings.Fields(r.Stdout), " "); got != "github:acme/alpha latest v2 ✗ missing" {
		t.Errorf("stdout = %q", r.Stdout)
	}
}

func TestListShowsModifiedWhenOwnedFoldersDiffer(t *testing.T) {
	gh := fakeGitHub(t, alphaReleases(t))
	e := newSyncEnv(t, gh, oneAddon)
	e.sync(t)
	write(t, filepath.Join(e.addons, "Alpha", "Alpha.toc"), "edited")
	if got := strings.Join(strings.Fields(e.cmd(t, "list").Stdout), " "); got != "github:acme/alpha latest v2 ! modified" {
		t.Errorf("edited file: %q", got)
	}
	e.sync(t) // sync leaves a modified install as is
	write(t, filepath.Join(e.addons, "Alpha", "Alpha.toc"), "two")
	write(t, filepath.Join(e.addons, "Alpha", "extra.lua"), "x")
	if got := strings.Join(strings.Fields(e.cmd(t, "list").Stdout), " "); got != "github:acme/alpha latest v2 ! modified" {
		t.Errorf("extra file: %q", got)
	}
}

func TestListIgnoresLocksWithoutATreeHash(t *testing.T) {
	gh := fakeGitHub(t, alphaReleases(t))
	e := newSyncEnv(t, gh, oneAddon)
	e.sync(t)
	l, _ := lock.Load(e.lock)
	en := l.Addons["github:acme/alpha"]
	en.Tree = ""
	l.Addons["github:acme/alpha"] = en
	_ = lock.Save(e.lock, l)
	write(t, filepath.Join(e.addons, "Alpha", "Alpha.toc"), "edited")
	if got := e.cmd(t, "list").Stdout; !strings.Contains(got, "✓ installed") {
		t.Errorf("stdout = %q", got)
	}
}

func TestListShowsUnsyncedAddonAsMissing(t *testing.T) {
	gh := fakeGitHub(t, alphaReleases(t))
	e := newSyncEnv(t, gh, oneAddon)
	if got := strings.Join(strings.Fields(e.cmd(t, "list").Stdout), " "); got != "github:acme/alpha latest - ✗ missing" {
		t.Errorf("stdout = %q", got)
	}
}

func TestListMakesNoNetworkCalls(t *testing.T) {
	gh := fakeGitHub(t, alphaReleases(t))
	e := newSyncEnv(t, gh, oneAddon)
	e.sync(t)
	n := 0
	gh.intercept = func(http.ResponseWriter, *http.Request) bool { n++; return false }
	if r := e.cmd(t, "list"); r.ExitCode != 0 {
		t.Fatal(r.Stderr)
	}
	if n != 0 {
		t.Errorf("list made %d requests", n)
	}
}

func TestListJSONFieldNames(t *testing.T) {
	gh := fakeGitHub(t, alphaReleases(t))
	e := newSyncEnv(t, gh, oneAddon)
	e.sync(t)
	var got []map[string]string
	r := e.cmd(t, "list", "--json")
	if err := json.Unmarshal([]byte(r.Stdout), &got); err != nil || len(got) != 1 {
		t.Fatalf("json = %q (%v)", r.Stdout, err)
	}
	want := map[string]string{"addon": "github:acme/alpha", "version": "latest", "locked": "v2", "status": "installed"}
	for k, v := range want {
		if got[0][k] != v {
			t.Errorf("%s = %q, want %q", k, got[0][k], v)
		}
	}
	if len(got[0]) != len(want) {
		t.Errorf("fields = %v", got[0])
	}
}

func TestListEmptyStateInAllThreeModes(t *testing.T) {
	gh := fakeGitHub(t, nil)
	e := newSyncEnv(t, gh, "[game]\nflavor = \"mainline\"\n")
	if r := e.cmdTTY(t, true, "list"); r.ExitCode != 0 || strings.TrimSpace(r.Stdout) == "" {
		t.Errorf("tty: exit %d stdout %q", r.ExitCode, r.Stdout)
	}
	if r := e.cmd(t, "list", "--json"); r.ExitCode != 0 || strings.TrimSpace(r.Stdout) != "[]" {
		t.Errorf("json: stdout %q", r.Stdout)
	}
	if r := e.cmd(t, "list"); r.ExitCode != 0 || r.Stdout != "" {
		t.Errorf("piped: stdout %q", r.Stdout)
	}
}

func TestListShowsMissingWhenOneOfSeveralFoldersIsAbsent(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{"acme/alpha": {{Tag: "v1", Assets: map[string][]byte{
		"a.zip": makeZip(t, map[string]string{"Alpha/Alpha.toc": "a", "AlphaOptions/AlphaOptions.toc": "o"}),
	}}}})
	e := newSyncEnv(t, gh, oneAddon)
	e.sync(t)
	if err := os.RemoveAll(filepath.Join(e.addons, "AlphaOptions")); err != nil {
		t.Fatal(err)
	}
	if got := e.cmd(t, "list").Stdout; !strings.Contains(got, "✗ missing") {
		t.Errorf("stdout = %q", got)
	}
}
