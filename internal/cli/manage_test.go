package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maddawik/wam/internal/clitest"
	"github.com/maddawik/wam/internal/lock"
	"github.com/maddawik/wam/internal/manifest"
)

const emptyManifest = "[game]\nflavor = \"mainline\"\n\n[addons]\n"

func (e syncEnv) run(t *testing.T, cmd string, args ...string) clitest.Result {
	t.Helper()
	a := append([]string{cmd, "-f", e.manifest, "--dir", e.addons}, args...)
	return clitest.Run(t, clitest.Opts{Args: a, Env: e.vars})
}

func alphaReleases(t *testing.T) map[string][]fakeRelease {
	return map[string][]fakeRelease{
		"acme/alpha": {
			{Tag: "v2", Assets: map[string][]byte{"a.zip": alphaZip(t, "two")}},
			{Tag: "v1", Assets: map[string][]byte{"a.zip": alphaZip(t, "one")}},
		},
	}
}

func TestAddInstallsLocksAndWritesManifest(t *testing.T) {
	e := newSyncEnv(t, fakeGitHub(t, alphaReleases(t)), emptyManifest)
	r := e.run(t, "add", "github:acme/alpha")
	if r.ExitCode != 0 {
		t.Fatalf("exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	if !strings.Contains(r.Stdout, "github:acme/alpha") || !strings.Contains(r.Stdout, "v2") {
		t.Errorf("stdout = %q", r.Stdout)
	}
	if got := read(t, filepath.Join(e.addons, "Alpha", "Alpha.toc")); got != "two" {
		t.Errorf("installed %q", got)
	}
	m, err := manifest.Load(e.manifest)
	if err != nil || len(m.Addons) != 1 || m.Addons[0].Version != "latest" {
		t.Errorf("manifest = %+v, err %v", m, err)
	}
	l, _ := lock.Load(e.lock)
	if got := l.Addons["github:acme/alpha"]; got.Tag != "v2" || got.Version != "latest" {
		t.Errorf("lock = %+v", got)
	}
	// Add then sync is a no-op.
	if r := e.sync(t); r.ExitCode != 0 || r.Stdout != "" {
		t.Errorf("sync after add: exit %d stdout %q", r.ExitCode, r.Stdout)
	}
}

func TestAddPinWritesExactTag(t *testing.T) {
	e := newSyncEnv(t, fakeGitHub(t, alphaReleases(t)), emptyManifest)
	if r := e.run(t, "add", "github:acme/alpha", "--pin", "v1"); r.ExitCode != 0 {
		t.Fatalf("exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	if got := read(t, e.manifest); !strings.Contains(got, `'github:acme/alpha' = 'v1'`) && !strings.Contains(got, `"github:acme/alpha" = "v1"`) {
		t.Errorf("manifest:\n%s", got)
	}
	if got := read(t, filepath.Join(e.addons, "Alpha", "Alpha.toc")); got != "one" {
		t.Errorf("installed %q", got)
	}
}

func TestAddDuplicateExitsFiveAndChangesNothing(t *testing.T) {
	e := newSyncEnv(t, fakeGitHub(t, alphaReleases(t)), oneAddon)
	r := e.run(t, "add", "github:acme/alpha", "--pin", "v1")
	if r.ExitCode != 5 || !strings.Contains(r.Stderr, "update") {
		t.Errorf("exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	if got := read(t, e.manifest); got != oneAddon {
		t.Errorf("manifest changed:\n%s", got)
	}
}

func TestAddMissingRepoOrTagExitsFourAndChangesNothing(t *testing.T) {
	for _, args := range [][]string{
		{"github:acme/nope"},
		{"github:acme/alpha", "--pin", "v9"},
	} {
		e := newSyncEnv(t, fakeGitHub(t, alphaReleases(t)), emptyManifest)
		r := e.run(t, "add", args...)
		if r.ExitCode != 4 {
			t.Errorf("%v: exit = %d, stderr = %q", args, r.ExitCode, r.Stderr)
		}
		if got := read(t, e.manifest); got != emptyManifest {
			t.Errorf("%v: manifest changed:\n%s", args, got)
		}
		if _, err := os.Stat(e.lock); err == nil {
			t.Errorf("%v: lock written", args)
		}
	}
}

func TestAddRejectsMalformedAddon(t *testing.T) {
	for _, arg := range []string{"acme/alpha", "github:alpha", "github:a/b/c", "gitlab:a/b"} {
		e := newSyncEnv(t, fakeGitHub(t, nil), emptyManifest)
		if r := e.run(t, "add", arg); r.ExitCode != 2 {
			t.Errorf("%s: exit = %d, stderr = %q", arg, r.ExitCode, r.Stderr)
		}
	}
}

func TestRemoveDeletesOwnedFoldersAndEntries(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{
		"acme/alpha": {{Tag: "v1", Assets: map[string][]byte{"a.zip": makeZip(t, map[string]string{"Alpha/a.lua": "a", "AlphaOptions/o.lua": "o"})}}},
		"acme/beta":  {{Tag: "v1", Assets: map[string][]byte{"b.zip": makeZip(t, map[string]string{"Beta/b.lua": "b"})}}},
	})
	e := newSyncEnv(t, gh, oneAddon+`"github:acme/beta" = "latest"`+"\n")
	if r := e.sync(t); r.ExitCode != 0 {
		t.Fatalf("sync: %d %s", r.ExitCode, r.Stderr)
	}
	write(t, filepath.Join(e.addons, "Unmanaged", "x.lua"), "x")

	r := e.run(t, "remove", "github:acme/alpha")
	if r.ExitCode != 0 || !strings.Contains(r.Stdout, "github:acme/alpha") {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", r.ExitCode, r.Stdout, r.Stderr)
	}
	for _, f := range []string{"Alpha", "AlphaOptions"} {
		if _, err := os.Stat(filepath.Join(e.addons, f)); err == nil {
			t.Errorf("%s still there", f)
		}
	}
	for _, f := range []string{"Beta", "Unmanaged"} {
		if _, err := os.Stat(filepath.Join(e.addons, f)); err != nil {
			t.Errorf("%s gone: %v", f, err)
		}
	}
	m, _ := manifest.Load(e.manifest)
	l, _ := lock.Load(e.lock)
	if len(m.Addons) != 1 || m.Addons[0].Key != "github:acme/beta" || len(l.Addons) != 1 {
		t.Errorf("manifest %+v, lock %+v", m.Addons, l.Addons)
	}
}

func TestRemoveUnknownAddonExitsFour(t *testing.T) {
	e := newSyncEnv(t, fakeGitHub(t, nil), oneAddon)
	r := e.run(t, "remove", "github:acme/other")
	if r.ExitCode != 4 {
		t.Errorf("exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	if got := read(t, e.manifest); got != oneAddon {
		t.Errorf("manifest changed:\n%s", got)
	}
}

func TestRemoveAddonNeverSyncedDropsManifestEntry(t *testing.T) {
	e := newSyncEnv(t, fakeGitHub(t, nil), oneAddon)
	if r := e.run(t, "remove", "github:acme/alpha"); r.ExitCode != 0 {
		t.Fatalf("exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	if m, _ := manifest.Load(e.manifest); len(m.Addons) != 0 {
		t.Errorf("manifest = %+v", m.Addons)
	}
}
