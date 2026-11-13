package cli_test

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// snapshot is every path under roots with its content, so a preview can be
// shown to have changed nothing.
func snapshot(t *testing.T, roots ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				out[p] = "<dir>"
				return nil
			}
			b, _ := os.ReadFile(p)
			out[p] = string(b)
			return nil
		})
	}
	return out
}

func (e syncEnv) state(t *testing.T) map[string]string {
	t.Helper()
	return snapshot(t, filepath.Dir(e.manifest))
}

func same(t *testing.T, before, after map[string]string) {
	t.Helper()
	if len(before) != len(after) {
		t.Errorf("filesystem changed: %d paths before, %d after", len(before), len(after))
	}
	for p, v := range before {
		if after[p] != v {
			t.Errorf("%s changed", p)
		}
	}
}

type planJSON struct {
	Preview bool `json:"preview"`
	Changes []struct {
		Action  string   `json:"action"`
		Addon   string   `json:"addon"`
		Version string   `json:"version"`
		From    string   `json:"from"`
		To      string   `json:"to"`
		Adds    []string `json:"adds"`
		Removes []string `json:"removes"`
	} `json:"changes"`
}

func TestSyncPreviewShowsNewAddonWithoutChangingAnything(t *testing.T) {
	e := newSyncEnv(t, fakeGitHub(t, alphaReleases(t)), oneAddon)
	before := e.state(t)
	r := e.sync(t, "--preview")
	if r.ExitCode != 0 {
		t.Fatalf("exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	if !strings.Contains(r.Stdout, "+ github:acme/alpha v2") || !strings.Contains(r.Stdout, "Alpha") {
		t.Errorf("stdout = %q", r.Stdout)
	}
	same(t, before, e.state(t))
}

func TestSyncPreviewShortFlag(t *testing.T) {
	e := newSyncEnv(t, fakeGitHub(t, alphaReleases(t)), oneAddon)
	if r := e.sync(t, "-n"); r.ExitCode != 0 || !strings.Contains(r.Stdout, "+ github:acme/alpha") {
		t.Errorf("exit = %d, stdout = %q, stderr = %q", r.ExitCode, r.Stdout, r.Stderr)
	}
}

func TestSyncPreviewShowsUpdateAndRemove(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{
		"acme/alpha": {
			{Tag: "v2", Assets: map[string][]byte{"a.zip": makeZip(t, map[string]string{"Alpha/a.lua": "2", "AlphaNew/n.lua": "n"})}},
			{Tag: "v1", Assets: map[string][]byte{"a.zip": makeZip(t, map[string]string{"Alpha/a.lua": "1", "AlphaOld/o.lua": "o"})}},
		},
		"acme/beta": {{Tag: "v1", Assets: map[string][]byte{"b.zip": makeZip(t, map[string]string{"Beta/b.lua": "b"})}}},
	})
	e := newSyncEnv(t, gh, "[game]\nflavor = \"mainline\"\n\n[addons]\n"+pinnedLine("github:acme/alpha", "v1")+"\"github:acme/beta\" = \"latest\"\n")
	if r := e.sync(t); r.ExitCode != 0 {
		t.Fatalf("sync: %d %s", r.ExitCode, r.Stderr)
	}
	write(t, e.manifest, "[game]\nflavor = \"mainline\"\n\n[addons]\n"+pinnedLine("github:acme/alpha", "v2"))
	before := e.state(t)

	r := e.sync(t, "--preview", "--json")
	if r.ExitCode != 0 {
		t.Fatalf("exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	var p planJSON
	if err := json.Unmarshal([]byte(r.Stdout), &p); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, r.Stdout)
	}
	if !p.Preview || len(p.Changes) != 2 {
		t.Fatalf("plan = %+v", p)
	}
	rm, up := p.Changes[0], p.Changes[1]
	if rm.Action != "remove" {
		rm, up = up, rm
	}
	if rm.Action != "remove" || rm.Addon != "github:acme/beta" || len(rm.Removes) != 1 || rm.Removes[0] != "Beta" {
		t.Errorf("remove = %+v", rm)
	}
	if up.Action != "update" || up.Addon != "github:acme/alpha" || up.From != "v1" || up.To != "v2" ||
		strings.Join(up.Adds, ",") != "AlphaNew" || strings.Join(up.Removes, ",") != "AlphaOld" {
		t.Errorf("update = %+v", up)
	}
	same(t, before, e.state(t))

	text := e.sync(t, "--preview").Stdout
	for _, want := range []string{"~ github:acme/alpha v1 -> v2", "- github:acme/beta"} {
		if !strings.Contains(text, want) {
			t.Errorf("text missing %q:\n%s", want, text)
		}
	}
}

func pinnedLine(key, tag string) string { return "\"" + key + "\" = \"" + tag + "\"\n" }

func TestSyncPreviewWhenUpToDateIsEmpty(t *testing.T) {
	e := newSyncEnv(t, fakeGitHub(t, alphaReleases(t)), oneAddon)
	if r := e.sync(t); r.ExitCode != 0 {
		t.Fatalf("sync: %d %s", r.ExitCode, r.Stderr)
	}
	before := e.state(t)
	r := e.sync(t, "--preview", "--json")
	var p planJSON
	if err := json.Unmarshal([]byte(r.Stdout), &p); err != nil || r.ExitCode != 0 || !p.Preview || len(p.Changes) != 0 {
		t.Errorf("exit %d, plan %+v, err %v, stdout %q", r.ExitCode, p, err, r.Stdout)
	}
	if !strings.Contains(r.Stdout, `"changes": []`) {
		t.Errorf("changes must be [] not null: %q", r.Stdout)
	}
	same(t, before, e.state(t))
}

func TestAddPreviewChangesNothing(t *testing.T) {
	e := newSyncEnv(t, fakeGitHub(t, alphaReleases(t)), emptyManifest)
	before := e.state(t)
	r := e.run(t, "add", "github:acme/alpha", "-n")
	if r.ExitCode != 0 || !strings.Contains(r.Stdout, "+ github:acme/alpha v2") || !strings.Contains(r.Stdout, "Alpha") {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", r.ExitCode, r.Stdout, r.Stderr)
	}
	same(t, before, e.state(t))

	r = e.run(t, "add", "github:acme/alpha", "--pin", "v1", "--preview", "--json")
	var p planJSON
	if err := json.Unmarshal([]byte(r.Stdout), &p); err != nil || len(p.Changes) != 1 {
		t.Fatalf("plan %+v, err %v, stdout %q", p, err, r.Stdout)
	}
	if c := p.Changes[0]; c.Action != "add" || c.Version != "v1" || c.To != "v1" || len(c.Adds) != 1 || c.Adds[0] != "Alpha" {
		t.Errorf("change = %+v", c)
	}
	same(t, before, e.state(t))
}

func TestAddPreviewStillRefusesDuplicatesAndConflicts(t *testing.T) {
	e := newSyncEnv(t, fakeGitHub(t, alphaReleases(t)), oneAddon)
	if r := e.run(t, "add", "github:acme/alpha", "-n"); r.ExitCode != 5 {
		t.Errorf("duplicate: exit = %d", r.ExitCode)
	}
	e = newSyncEnv(t, fakeGitHub(t, alphaReleases(t)), emptyManifest)
	write(t, filepath.Join(e.addons, "Alpha", "x.lua"), "mine")
	before := e.state(t)
	if r := e.run(t, "add", "github:acme/alpha", "-n"); r.ExitCode != 5 {
		t.Errorf("unmanaged folder: exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	same(t, before, e.state(t))
}

func TestRemovePreviewChangesNothing(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{
		"acme/alpha": {{Tag: "v1", Assets: map[string][]byte{"a.zip": makeZip(t, map[string]string{"Alpha/a.lua": "a", "AlphaOptions/o.lua": "o"})}}},
	})
	e := newSyncEnv(t, gh, oneAddon)
	if r := e.sync(t); r.ExitCode != 0 {
		t.Fatalf("sync: %d %s", r.ExitCode, r.Stderr)
	}
	before := e.state(t)
	r := e.run(t, "remove", "github:acme/alpha", "--preview")
	if r.ExitCode != 0 || !strings.Contains(r.Stdout, "- github:acme/alpha") || !strings.Contains(r.Stdout, "AlphaOptions") {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", r.ExitCode, r.Stdout, r.Stderr)
	}
	same(t, before, e.state(t))

	r = e.run(t, "remove", "github:acme/alpha", "-n", "--json")
	var p planJSON
	if err := json.Unmarshal([]byte(r.Stdout), &p); err != nil || len(p.Changes) != 1 {
		t.Fatalf("plan %+v, err %v", p, err)
	}
	if c := p.Changes[0]; c.Action != "remove" || c.From != "v1" || len(c.Removes) != 2 {
		t.Errorf("change = %+v", c)
	}
	same(t, before, e.state(t))
}

func TestPreviewHelpSaysItUsesTheNetwork(t *testing.T) {
	for _, cmd := range []string{"sync", "add"} {
		e := newSyncEnv(t, fakeGitHub(t, nil), emptyManifest)
		r := e.run(t, cmd, "--help")
		if !strings.Contains(r.Stdout, "--preview") || !strings.Contains(r.Stdout, "network") {
			t.Errorf("%s help:\n%s", cmd, r.Stdout)
		}
	}
}
