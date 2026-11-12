package cli_test

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/maddawik/wam/internal/clitest"
	"github.com/maddawik/wam/internal/lock"
)

type fakeRelease struct {
	Tag        string
	Draft      bool
	Prerelease bool
	// Assets maps asset name to content.
	Assets map[string][]byte
}

// fakeServer serves the subset of the GitHub REST API wam uses, from repos
// keyed "owner/repo", newest release first. Tests can change a repo between
// syncs with set.
type fakeServer struct {
	*httptest.Server
	mu       sync.Mutex
	repos    map[string][]fakeRelease
	releases map[string]int // requests to the releases endpoints, by repo
}

func fakeGitHub(t *testing.T, repos map[string][]fakeRelease) *fakeServer {
	t.Helper()
	f := &fakeServer{repos: repos, releases: map[string]int{}}
	if f.repos == nil {
		f.repos = map[string][]fakeRelease{}
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

// set replaces a repo's releases.
func (f *fakeServer) set(repo string, releases []fakeRelease) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos[repo] = releases
}

// lookups is how many times wam asked about repo's releases.
func (f *fakeServer) lookups(repo string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.releases[repo]
}

func (f *fakeServer) json(w http.ResponseWriter, repo string, r fakeRelease) map[string]any {
	var assets []map[string]any
	for name := range r.Assets {
		assets = append(assets, map[string]any{
			"name":                 name,
			"browser_download_url": fmt.Sprintf("%s/download/%s/%s/%s", f.URL, repo, r.Tag, name),
		})
	}
	return map[string]any{"tag_name": r.Tag, "draft": r.Draft, "prerelease": r.Prerelease, "assets": assets}
}

func (f *fakeServer) serve(w http.ResponseWriter, req *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := req.URL.Path
	if rest, ok := strings.CutPrefix(path, "/download/"); ok {
		parts := strings.Split(rest, "/")
		if len(parts) >= 4 {
			repo := parts[0] + "/" + parts[1]
			tag := strings.Join(parts[2:len(parts)-1], "/")
			name := parts[len(parts)-1]
			for _, r := range f.repos[repo] {
				if r.Tag == tag {
					if body, ok := r.Assets[name]; ok {
						_, _ = w.Write(body)
						return
					}
				}
			}
		}
		http.NotFound(w, req)
		return
	}
	rest, ok := strings.CutPrefix(path, "/repos/")
	if !ok {
		http.NotFound(w, req)
		return
	}
	if repo, tag, ok := strings.Cut(rest, "/releases/tags/"); ok {
		f.releases[repo]++
		for _, r := range f.repos[repo] {
			if r.Tag == tag {
				_ = json.NewEncoder(w).Encode(f.json(w, repo, r))
				return
			}
		}
		http.NotFound(w, req)
		return
	}
	if repo, ok := strings.CutSuffix(rest, "/releases"); ok {
		releases, known := f.repos[repo]
		if !known {
			http.NotFound(w, req)
			return
		}
		f.releases[repo]++
		var out []map[string]any
		for _, r := range releases {
			out = append(out, f.json(w, repo, r))
		}
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	http.NotFound(w, req)
}

// makeZip builds a zip from path→content pairs. Paths ending in "/" are
// directories.
func makeZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// syncEnv is a fake GitHub plus a manifest and AddOns dir in a temp dir.
type syncEnv struct {
	manifest, lock, addons string
	vars                   map[string]string
}

func newSyncEnv(t *testing.T, gh *fakeServer, manifest string) syncEnv {
	t.Helper()
	root := t.TempDir()
	e := syncEnv{
		manifest: filepath.Join(root, "wam.toml"),
		lock:     filepath.Join(root, "wam.lock"),
		addons:   filepath.Join(root, "wow", "Interface", "AddOns"),
		vars:     map[string]string{"WAM_GITHUB_API": gh.URL},
	}
	write(t, e.manifest, manifest)
	return e
}

func (e syncEnv) sync(t *testing.T, extra ...string) clitest.Result {
	t.Helper()
	args := append([]string{"sync", "-f", e.manifest, "--dir", e.addons}, extra...)
	return clitest.Run(t, clitest.Opts{Args: args, Env: e.vars})
}

const oneAddon = `[game]
flavor = "mainline"

[addons]
"github:acme/alpha" = "latest"
`

func TestSyncInstallsLatestReleaseAndWritesLock(t *testing.T) {
	zipBytes := makeZip(t, map[string]string{
		"Alpha/Alpha.toc":  "## Title: Alpha",
		"Alpha/core.lua":   "-- core",
		"AlphaOptions/a.x": "opts",
	})
	gh := fakeGitHub(t, map[string][]fakeRelease{
		"acme/alpha": {{Tag: "v2.0.0", Assets: map[string][]byte{"Alpha-v2.0.0.zip": zipBytes}}},
	})
	e := newSyncEnv(t, gh, oneAddon)

	r := e.sync(t)
	if r.ExitCode != 0 {
		t.Fatalf("exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}

	if got := read(t, filepath.Join(e.addons, "Alpha", "Alpha.toc")); got != "## Title: Alpha" {
		t.Errorf("Alpha.toc = %q", got)
	}
	if got := read(t, filepath.Join(e.addons, "AlphaOptions", "a.x")); got != "opts" {
		t.Errorf("a.x = %q", got)
	}

	l, err := lock.Load(e.lock)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := l.Addons["github:acme/alpha"]
	if !ok {
		t.Fatalf("lock has no github:acme/alpha: %+v", l.Addons)
	}
	want := lock.Entry{
		Source: "github", Repo: "acme/alpha", Version: "latest", Tag: "v2.0.0", Asset: "Alpha-v2.0.0.zip",
		URL:    gh.URL + "/download/acme/alpha/v2.0.0/Alpha-v2.0.0.zip",
		SHA256: sha(zipBytes), Folders: []string{"Alpha", "AlphaOptions"},
	}
	slices.Sort(got.Folders)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("lock entry = %+v, want %+v", got, want)
	}
}

func TestSyncSkipsDraftsAndPrereleasesForLatest(t *testing.T) {
	zipFor := func(marker string) []byte {
		return makeZip(t, map[string]string{"Alpha/Alpha.toc": marker})
	}
	gh := fakeGitHub(t, map[string][]fakeRelease{
		"acme/alpha": {
			{Tag: "v4.0.0-draft", Draft: true, Assets: map[string][]byte{"a.zip": zipFor("draft")}},
			{Tag: "v3.0.0-beta", Prerelease: true, Assets: map[string][]byte{"a.zip": zipFor("beta")}},
			{Tag: "v2.0.0", Assets: map[string][]byte{"a.zip": zipFor("stable")}},
			{Tag: "v1.0.0", Assets: map[string][]byte{"a.zip": zipFor("old")}},
		},
	})
	e := newSyncEnv(t, gh, oneAddon)

	if r := e.sync(t); r.ExitCode != 0 {
		t.Fatalf("exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	if got := read(t, filepath.Join(e.addons, "Alpha", "Alpha.toc")); got != "stable" {
		t.Errorf("installed %q, want the newest stable release", got)
	}
	if lock := read(t, e.lock); !strings.Contains(lock, `tag = 'v2.0.0'`) {
		t.Errorf("lock does not record v2.0.0:\n%s", lock)
	}
}

func TestSyncInstallsEveryManifestEntry(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{
		"acme/alpha": {{Tag: "v1", Assets: map[string][]byte{"a.zip": makeZip(t, map[string]string{"Alpha/a.lua": "a"})}}},
		"acme/beta":  {{Tag: "v9", Assets: map[string][]byte{"b.zip": makeZip(t, map[string]string{"Beta/b.lua": "b"})}}},
	})
	e := newSyncEnv(t, gh, oneAddon+`"github:acme/beta" = "latest"`+"\n")

	r := e.sync(t, "--json")
	if r.ExitCode != 0 {
		t.Fatalf("exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	for _, p := range []string{"Alpha/a.lua", "Beta/b.lua"} {
		if _, err := os.Stat(filepath.Join(e.addons, filepath.FromSlash(p))); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
	lock := read(t, e.lock)
	for _, want := range []string{`github:acme/alpha`, `github:acme/beta`, `tag = 'v9'`} {
		if !strings.Contains(lock, want) {
			t.Errorf("lock lacks %s:\n%s", want, lock)
		}
	}
	var changes []struct{ Addon, Tag string }
	if err := json.Unmarshal([]byte(r.Stdout), &changes); err != nil || len(changes) != 2 {
		t.Errorf("stdout = %q (err %v), want two changes", r.Stdout, err)
	}
}

func TestSyncTwiceChangesNothingTheSecondTime(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{
		"acme/alpha": {{Tag: "v1", Assets: map[string][]byte{"a.zip": makeZip(t, map[string]string{"Alpha/a.lua": "a"})}}},
	})
	e := newSyncEnv(t, gh, oneAddon)
	if r := e.sync(t); r.ExitCode != 0 {
		t.Fatalf("first sync: exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	before := read(t, e.lock)

	r := e.sync(t)
	if r.ExitCode != 0 || r.Stdout != "" {
		t.Errorf("second sync: exit = %d, stdout = %q, stderr = %q", r.ExitCode, r.Stdout, r.Stderr)
	}
	if after := read(t, e.lock); after != before {
		t.Errorf("lock changed:\n%s\n---\n%s", before, after)
	}
}

func TestSyncMissingRepoExitsFour(t *testing.T) {
	gh := fakeGitHub(t, nil)
	e := newSyncEnv(t, gh, oneAddon)
	r := e.sync(t)
	if r.ExitCode != 4 || !strings.Contains(r.Stderr, "acme/alpha") {
		t.Errorf("exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	if _, err := os.Stat(e.lock); err == nil {
		t.Error("lock written although nothing was installed")
	}
}

func TestSyncRefusesToOverwriteAFolderItDoesNotOwn(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{
		"acme/alpha": {{Tag: "v1", Assets: map[string][]byte{"a.zip": makeZip(t, map[string]string{"Alpha/a.lua": "new"})}}},
	})
	e := newSyncEnv(t, gh, oneAddon)
	if err := os.MkdirAll(filepath.Join(e.addons, "Alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(e.addons, "Alpha", "mine.lua"), "mine")

	r := e.sync(t)
	if r.ExitCode != 5 {
		t.Errorf("exit = %d, want 5; stderr = %q", r.ExitCode, r.Stderr)
	}
	if got := read(t, filepath.Join(e.addons, "Alpha", "mine.lua")); got != "mine" {
		t.Errorf("existing folder was touched: %q", got)
	}
}

func TestSyncRejectsZipEntriesThatEscapeTheAddOnsFolder(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{
		"acme/alpha": {{Tag: "v1", Assets: map[string][]byte{"a.zip": makeZip(t, map[string]string{"../evil.lua": "x"})}}},
	})
	e := newSyncEnv(t, gh, oneAddon)
	r := e.sync(t)
	if r.ExitCode != 5 {
		t.Errorf("exit = %d, want 5; stderr = %q", r.ExitCode, r.Stderr)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(e.addons), "evil.lua")); err == nil {
		t.Error("zip entry escaped the AddOns folder")
	}
}

func TestSyncWithoutFileOrDirIsAUsageError(t *testing.T) {
	for _, args := range [][]string{{"sync"}, {"sync", "-f", "wam.toml"}} {
		r := clitest.Run(t, clitest.Opts{Args: args})
		if r.ExitCode != 2 {
			t.Errorf("%v: exit = %d, want 2; stderr = %q", args, r.ExitCode, r.Stderr)
		}
	}
}

const questieTag = "bundle/v12.7.0+v1.0.14"

func alphaZip(t *testing.T, marker string) []byte {
	t.Helper()
	return makeZip(t, map[string]string{"Alpha/Alpha.toc": marker})
}

func pinned(tag string) string {
	return "[game]\nflavor = \"mainline\"\n\n[addons]\n\"github:acme/alpha\" = \"" + tag + "\"\n"
}

func TestSyncPinToQuestieStyleTagInstalls(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{
		"acme/alpha": {
			{Tag: "bundle/v13.0.0+v1.0.15", Assets: map[string][]byte{"a.zip": alphaZip(t, "newer")}},
			{Tag: questieTag, Assets: map[string][]byte{"a.zip": alphaZip(t, "pinned")}},
		},
	})
	e := newSyncEnv(t, gh, pinned(questieTag))
	r := e.sync(t)
	if r.ExitCode != 0 {
		t.Fatalf("exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	if got := read(t, filepath.Join(e.addons, "Alpha", "Alpha.toc")); got != "pinned" {
		t.Errorf("installed %q, want the pinned release", got)
	}
	l, err := lock.Load(e.lock)
	if err != nil {
		t.Fatal(err)
	}
	if got := l.Addons["github:acme/alpha"].Tag; got != questieTag {
		t.Errorf("locked tag = %q", got)
	}
}

func TestSyncPinMatchesTagsExactly(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{
		"acme/alpha": {{Tag: "v1.0.0", Assets: map[string][]byte{"a.zip": alphaZip(t, "x")}}},
	})
	e := newSyncEnv(t, gh, pinned("1.0.0"))
	if r := e.sync(t); r.ExitCode != 4 {
		t.Errorf("exit = %d, want 4; stderr = %q", r.ExitCode, r.Stderr)
	}
}

func TestSyncPinToPrereleaseInstallsIt(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{
		"acme/alpha": {
			{Tag: "v2.0.0-beta.1", Prerelease: true, Assets: map[string][]byte{"a.zip": alphaZip(t, "beta")}},
			{Tag: "v1.0.0", Assets: map[string][]byte{"a.zip": alphaZip(t, "stable")}},
		},
	})
	e := newSyncEnv(t, gh, pinned("v2.0.0-beta.1"))
	if r := e.sync(t); r.ExitCode != 0 {
		t.Fatalf("exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	if got := read(t, filepath.Join(e.addons, "Alpha", "Alpha.toc")); got != "beta" {
		t.Errorf("installed %q, want the prerelease", got)
	}
}

func TestSyncMissingTagExitsFour(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{
		"acme/alpha": {{Tag: "v1.0.0", Assets: map[string][]byte{"a.zip": alphaZip(t, "x")}}},
	})
	e := newSyncEnv(t, gh, pinned("v9.9.9"))
	r := e.sync(t)
	if r.ExitCode != 4 || !strings.Contains(r.Stderr, "v9.9.9") {
		t.Errorf("exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
}

func TestSyncKeepsLockedLatestWhenNewerReleaseAppears(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{
		"acme/alpha": {{Tag: "v1.0.0", Assets: map[string][]byte{"a.zip": alphaZip(t, "one")}}},
	})
	e := newSyncEnv(t, gh, oneAddon)
	if r := e.sync(t); r.ExitCode != 0 {
		t.Fatalf("first sync: exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	gh.set("acme/alpha", []fakeRelease{
		{Tag: "v2.0.0", Assets: map[string][]byte{"a.zip": alphaZip(t, "two")}},
		{Tag: "v1.0.0", Assets: map[string][]byte{"a.zip": alphaZip(t, "one")}},
	})
	lookups := gh.lookups("acme/alpha")
	before := read(t, e.lock)

	// A fresh machine: the lock is there, the AddOns folder is not.
	if err := os.RemoveAll(e.addons); err != nil {
		t.Fatal(err)
	}
	r := e.sync(t)
	if r.ExitCode != 0 {
		t.Fatalf("second sync: exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	if got := read(t, filepath.Join(e.addons, "Alpha", "Alpha.toc")); got != "one" {
		t.Errorf("installed %q, want the locked release", got)
	}
	if after := read(t, e.lock); after != before {
		t.Errorf("lock changed:\n%s\n---\n%s", before, after)
	}
	if got := gh.lookups("acme/alpha"); got != lookups {
		t.Errorf("sync re-resolved a locked addon (%d lookups, had %d)", got, lookups)
	}
}

func TestSyncChangedPinReResolvesOnlyThatAddon(t *testing.T) {
	betaZip := func(m string) []byte { return makeZip(t, map[string]string{"Beta/b.lua": m}) }
	gh := fakeGitHub(t, map[string][]fakeRelease{
		"acme/alpha": {
			{Tag: "v2.0.0", Assets: map[string][]byte{"a.zip": alphaZip(t, "two")}},
			{Tag: "v1.0.0", Assets: map[string][]byte{"a.zip": alphaZip(t, "one")}},
		},
		"acme/beta": {{Tag: "v1", Assets: map[string][]byte{"b.zip": betaZip("b")}}},
	})
	both := func(alpha string) string {
		return pinned(alpha) + "\"github:acme/beta\" = \"latest\"\n"
	}
	e := newSyncEnv(t, gh, both("v1.0.0"))
	if r := e.sync(t); r.ExitCode != 0 {
		t.Fatalf("first sync: exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	betaLookups := gh.lookups("acme/beta")

	write(t, e.manifest, both("v2.0.0"))
	if r := e.sync(t); r.ExitCode != 0 {
		t.Fatalf("second sync: exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	if got := read(t, filepath.Join(e.addons, "Alpha", "Alpha.toc")); got != "two" {
		t.Errorf("Alpha = %q, want the new pin installed over the old", got)
	}
	if got := gh.lookups("acme/beta"); got != betaLookups {
		t.Errorf("beta was re-resolved (%d lookups, had %d)", got, betaLookups)
	}
	l, err := lock.Load(e.lock)
	if err != nil {
		t.Fatal(err)
	}
	if got := l.Addons["github:acme/alpha"].Tag; got != "v2.0.0" {
		t.Errorf("locked tag = %q", got)
	}
}

func TestSyncSecondRunMakesNoFilesystemChanges(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{
		"acme/alpha": {{Tag: "v1", Assets: map[string][]byte{"a.zip": alphaZip(t, "a")}}},
	})
	e := newSyncEnv(t, gh, oneAddon)
	if r := e.sync(t); r.ExitCode != 0 {
		t.Fatalf("first sync: exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	snap := func() map[string]string {
		out := map[string]string{}
		root := filepath.Dir(e.lock)
		_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			out[p] = fmt.Sprintf("%v %d %s", info.Mode(), info.Size(), info.ModTime())
			return nil
		})
		return out
	}
	before := snap()
	if r := e.sync(t); r.ExitCode != 0 {
		t.Fatalf("second sync: exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	if after := snap(); !reflect.DeepEqual(before, after) {
		t.Errorf("filesystem changed:\n%v\n---\n%v", before, after)
	}
}

func TestSyncRefusesTamperedAssetWithExitFive(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{
		"acme/alpha": {{Tag: "v1", Assets: map[string][]byte{"a.zip": alphaZip(t, "good")}}},
	})
	e := newSyncEnv(t, gh, oneAddon)
	if r := e.sync(t); r.ExitCode != 0 {
		t.Fatalf("first sync: exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	gh.set("acme/alpha", []fakeRelease{{Tag: "v1", Assets: map[string][]byte{"a.zip": alphaZip(t, "evil")}}})
	if err := os.RemoveAll(e.addons); err != nil {
		t.Fatal(err)
	}
	r := e.sync(t)
	if r.ExitCode != 5 {
		t.Fatalf("exit = %d, want 5; stderr = %q", r.ExitCode, r.Stderr)
	}
	if !strings.Contains(r.Stderr, "wam update github:acme/alpha") {
		t.Errorf("stderr lacks the fix command: %q", r.Stderr)
	}
	if _, err := os.Stat(filepath.Join(e.addons, "Alpha")); err == nil {
		t.Error("tampered asset was installed")
	}
}
