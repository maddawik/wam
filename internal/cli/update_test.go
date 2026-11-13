package cli_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maddawik/wam/internal/clitest"
	"github.com/maddawik/wam/internal/lock"
)

const bothAddons = `[game]
flavor = "mainline"

[addons]
"github:acme/alpha" = "latest"
"github:acme/beta" = "latest"
`

func twoRepos(t *testing.T) map[string][]fakeRelease {
	return map[string][]fakeRelease{
		"acme/alpha": {{Tag: "v1", Assets: map[string][]byte{"a.zip": alphaZip(t, "one")}}},
		"acme/beta":  {{Tag: "b1", Assets: map[string][]byte{"b.zip": makeZip(t, map[string]string{"Beta/Beta.toc": "b-one"})}}},
	}
}

func (e syncEnv) cmd(t *testing.T, cmd string, extra ...string) clitest.Result {
	t.Helper()
	return e.cmdTTY(t, false, cmd, extra...)
}

func (e syncEnv) cmdTTY(t *testing.T, tty bool, cmd string, extra ...string) clitest.Result {
	t.Helper()
	args := append([]string{cmd, "-f", e.manifest, "--dir", e.addons}, extra...)
	return clitest.Run(t, clitest.Opts{Args: args, Env: e.vars, StdoutTTY: tty})
}

// release publishes a new newest release for repo.
func release(gh *fakeServer, repo string, r fakeRelease) {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	gh.repos[repo] = append([]fakeRelease{r}, gh.repos[repo]...)
}

func TestUpdateMovesUnpinnedAddonsAndRewritesLock(t *testing.T) {
	gh := fakeGitHub(t, twoRepos(t))
	e := newSyncEnv(t, gh, bothAddons)
	if r := e.sync(t); r.ExitCode != 0 {
		t.Fatal(r.Stderr)
	}
	release(gh, "acme/alpha", fakeRelease{Tag: "v2", Assets: map[string][]byte{"a.zip": alphaZip(t, "two")}})

	r := e.cmd(t, "update")
	if r.ExitCode != 0 {
		t.Fatalf("exit %d: %s", r.ExitCode, r.Stderr)
	}
	if !strings.Contains(r.Stdout, "updated github:acme/alpha v1 -> v2") || strings.Contains(r.Stdout, "beta") {
		t.Errorf("stdout = %q", r.Stdout)
	}
	if got := read(t, filepath.Join(e.addons, "Alpha", "Alpha.toc")); got != "two" {
		t.Errorf("installed %q", got)
	}
	l, _ := lock.Load(e.lock)
	if got := l.Addons["github:acme/alpha"].Tag; got != "v2" {
		t.Errorf("lock tag = %q", got)
	}
	if r := e.cmd(t, "update"); r.ExitCode != 0 || r.Stdout != "" {
		t.Errorf("second update: exit %d stdout %q", r.ExitCode, r.Stdout)
	}
}

func TestUpdateLeavesPinnedAddonsAlone(t *testing.T) {
	gh := fakeGitHub(t, alphaReleases(t))
	e := newSyncEnv(t, gh, pinned("v1"))
	if r := e.sync(t); r.ExitCode != 0 {
		t.Fatal(r.Stderr)
	}
	release(gh, "acme/alpha", fakeRelease{Tag: "v3", Assets: map[string][]byte{"a.zip": alphaZip(t, "three")}})

	if r := e.cmd(t, "update"); r.ExitCode != 0 || r.Stdout != "" {
		t.Errorf("bulk: exit %d stdout %q stderr %q", r.ExitCode, r.Stdout, r.Stderr)
	}
	r := e.cmd(t, "update", "github:acme/alpha")
	if r.ExitCode != 0 || !strings.Contains(r.Stderr, "pinned") {
		t.Errorf("named: exit %d stderr %q", r.ExitCode, r.Stderr)
	}
	if got := read(t, filepath.Join(e.addons, "Alpha", "Alpha.toc")); got != "one" {
		t.Errorf("installed %q", got)
	}
	l, _ := lock.Load(e.lock)
	if got := l.Addons["github:acme/alpha"].Tag; got != "v1" {
		t.Errorf("lock tag = %q", got)
	}
}

func TestUpdateNamedTouchesOnlyThatAddon(t *testing.T) {
	gh := fakeGitHub(t, twoRepos(t))
	e := newSyncEnv(t, gh, bothAddons)
	e.sync(t)
	release(gh, "acme/alpha", fakeRelease{Tag: "v2", Assets: map[string][]byte{"a.zip": alphaZip(t, "two")}})
	release(gh, "acme/beta", fakeRelease{Tag: "b2", Assets: map[string][]byte{"b.zip": makeZip(t, map[string]string{"Beta/Beta.toc": "b-two"})}})

	if r := e.cmd(t, "update", "github:acme/beta"); r.ExitCode != 0 {
		t.Fatal(r.Stderr)
	}
	l, _ := lock.Load(e.lock)
	if l.Addons["github:acme/alpha"].Tag != "v1" || l.Addons["github:acme/beta"].Tag != "b2" {
		t.Errorf("lock = %+v", l.Addons)
	}
	if got := read(t, filepath.Join(e.addons, "Alpha", "Alpha.toc")); got != "one" {
		t.Errorf("alpha installed %q", got)
	}
}

func TestUpdateUnknownAddonExitsFour(t *testing.T) {
	e := newSyncEnv(t, fakeGitHub(t, alphaReleases(t)), oneAddon)
	if r := e.cmd(t, "update", "github:acme/nope"); r.ExitCode != 4 {
		t.Errorf("exit %d stderr %q", r.ExitCode, r.Stderr)
	}
}

func TestUpdateAcceptsNewAssetHash(t *testing.T) {
	gh := fakeGitHub(t, alphaReleases(t))
	e := newSyncEnv(t, gh, pinned("v1"))
	e.sync(t)
	// The asset behind v1 changes, so a restore would fail the hash check.
	gh.set("acme/alpha", []fakeRelease{{Tag: "v1", Assets: map[string][]byte{"a.zip": alphaZip(t, "changed")}}})

	r := e.cmd(t, "update", "github:acme/alpha")
	if r.ExitCode != 0 || !strings.Contains(r.Stdout, "github:acme/alpha") {
		t.Fatalf("exit %d stdout %q stderr %q", r.ExitCode, r.Stdout, r.Stderr)
	}
	if got := read(t, filepath.Join(e.addons, "Alpha", "Alpha.toc")); got != "changed" {
		t.Errorf("installed %q", got)
	}
	l, _ := lock.Load(e.lock)
	if got := l.Addons["github:acme/alpha"].SHA256; got != sha(alphaZip(t, "changed")) {
		t.Errorf("lock sha = %q", got)
	}
}

func TestOutdatedListsOnlyAddonsWithNewerReleases(t *testing.T) {
	gh := fakeGitHub(t, twoRepos(t))
	e := newSyncEnv(t, gh, bothAddons)
	e.sync(t)
	release(gh, "acme/alpha", fakeRelease{Tag: "v2", Assets: map[string][]byte{"a.zip": alphaZip(t, "two")}})

	r := e.cmd(t, "outdated")
	if r.ExitCode != 0 || strings.TrimSpace(r.Stdout) != "github:acme/alpha v1 -> v2" {
		t.Errorf("exit %d stdout %q stderr %q", r.ExitCode, r.Stdout, r.Stderr)
	}
	var got []map[string]string
	r = e.cmd(t, "outdated", "--json")
	if err := json.Unmarshal([]byte(r.Stdout), &got); err != nil || len(got) != 1 ||
		got[0]["addon"] != "github:acme/alpha" || got[0]["locked"] != "v1" || got[0]["latest"] != "v2" {
		t.Errorf("json = %q (%v)", r.Stdout, err)
	}
}

func TestOutdatedSkipsPinnedAddons(t *testing.T) {
	gh := fakeGitHub(t, alphaReleases(t))
	e := newSyncEnv(t, gh, pinned("v1"))
	e.sync(t)
	if r := e.cmd(t, "outdated"); r.ExitCode != 0 || r.Stdout != "" {
		t.Errorf("exit %d stdout %q", r.ExitCode, r.Stdout)
	}
}

func TestOutdatedEmptyStateInAllThreeModes(t *testing.T) {
	gh := fakeGitHub(t, alphaReleases(t))
	e := newSyncEnv(t, gh, oneAddon)
	e.sync(t)

	if r := e.cmdTTY(t, true, "outdated"); r.ExitCode != 0 || strings.TrimSpace(r.Stdout) == "" {
		t.Errorf("tty: exit %d stdout %q", r.ExitCode, r.Stdout)
	}
	if r := e.cmd(t, "outdated", "--json"); r.ExitCode != 0 || strings.TrimSpace(r.Stdout) != "[]" {
		t.Errorf("json: stdout %q", r.Stdout)
	}
	if r := e.cmd(t, "outdated"); r.ExitCode != 0 || r.Stdout != "" {
		t.Errorf("piped: stdout %q", r.Stdout)
	}
}

func TestOutdatedChangesNothingOnDisk(t *testing.T) {
	gh := fakeGitHub(t, alphaReleases(t))
	e := newSyncEnv(t, gh, pinned("v1"))
	e.sync(t)
	gh.set("acme/alpha", append([]fakeRelease{{Tag: "v9", Assets: map[string][]byte{"a.zip": alphaZip(t, "nine")}}}, alphaReleases(t)["acme/alpha"]...))
	e2 := newSyncEnv(t, gh, oneAddon)
	e2.addons, e2.lock = e.addons, e.lock
	l, _ := lock.Load(e.lock)
	_ = lock.Save(e2.lock, l)
	beforeLock, beforeToc := read(t, e2.lock), read(t, filepath.Join(e.addons, "Alpha", "Alpha.toc"))
	beforeManifest := read(t, e2.manifest)

	if r := e2.cmd(t, "outdated"); r.ExitCode != 0 {
		t.Fatal(r.Stderr)
	}
	if read(t, e2.lock) != beforeLock || read(t, e2.manifest) != beforeManifest || read(t, filepath.Join(e.addons, "Alpha", "Alpha.toc")) != beforeToc {
		t.Error("outdated changed files")
	}
}
