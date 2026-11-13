package cli_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/maddawik/wam/internal/clitest"
	"github.com/maddawik/wam/internal/errs"
	"github.com/maddawik/wam/internal/locate"
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
	// onDownload, if set, runs before an asset is served.
	onDownload func(*http.Request)
	// intercept, if set, sees every request first and answers it by
	// returning true.
	intercept func(http.ResponseWriter, *http.Request) bool
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
	if f.intercept != nil && f.intercept(w, req) {
		return
	}
	if f.onDownload != nil && strings.HasPrefix(req.URL.Path, "/download/") {
		f.onDownload(req)
	}
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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
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
	if got.Tree == "" {
		t.Error("lock entry has no tree hash")
	}
	want.Tree = got.Tree
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
	// Nothing is written outside the AddOns folder, nor left inside it.
	for dir, want := range map[string]int{filepath.Dir(e.addons): 1, e.addons: 0} {
		if got, err := os.ReadDir(dir); err != nil || len(got) != want {
			t.Errorf("%s holds %v (err %v), want %d entries", dir, got, err, want)
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

func twoAddons(alpha, beta string) string {
	return "[game]\nflavor = \"mainline\"\n\n[addons]\n" + alpha + beta
}

const (
	alphaLine = "\"github:acme/alpha\" = \"latest\"\n"
	betaLine  = "\"github:acme/beta\" = \"latest\"\n"
)

func TestSyncRefusesAFolderAnotherAddonOwnsIgnoringCase(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{
		"acme/alpha": {{Tag: "v1", Assets: map[string][]byte{"a.zip": makeZip(t, map[string]string{"Questie/a.lua": "alpha"})}}},
		"acme/beta":  {{Tag: "v1", Assets: map[string][]byte{"b.zip": makeZip(t, map[string]string{"questie/b.lua": "beta"})}}},
	})
	e := newSyncEnv(t, gh, twoAddons(alphaLine, ""))
	if r := e.sync(t); r.ExitCode != 0 {
		t.Fatalf("first sync: exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	write(t, e.manifest, twoAddons(alphaLine, betaLine))

	r := e.sync(t)
	if r.ExitCode != 5 {
		t.Fatalf("exit = %d, want 5; stderr = %q", r.ExitCode, r.Stderr)
	}
	for _, want := range []string{"github:acme/beta", "questie", "github:acme/alpha"} {
		if !strings.Contains(r.Stderr, want) {
			t.Errorf("stderr lacks %q: %q", want, r.Stderr)
		}
	}
	if got := read(t, filepath.Join(e.addons, "Questie", "a.lua")); got != "alpha" {
		t.Errorf("alpha's folder was touched: %q", got)
	}
	l, err := lock.Load(e.lock)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := l.Addons["github:acme/beta"]; ok {
		t.Error("beta was locked although its install was refused")
	}
}

func TestSyncRefusesAnUnmanagedFolderIgnoringCase(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{
		"acme/alpha": {{Tag: "v1", Assets: map[string][]byte{"a.zip": makeZip(t, map[string]string{"Questie/a.lua": "new"})}}},
	})
	e := newSyncEnv(t, gh, oneAddon)
	if err := os.MkdirAll(filepath.Join(e.addons, "questie"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(e.addons, "questie", "mine.lua"), "mine")

	r := e.sync(t)
	if r.ExitCode != 5 || !strings.Contains(r.Stderr, "Questie") || !strings.Contains(r.Stderr, "unmanaged") {
		t.Errorf("exit = %d, want 5 naming the folder as unmanaged; stderr = %q", r.ExitCode, r.Stderr)
	}
	if got := read(t, filepath.Join(e.addons, "questie", "mine.lua")); got != "mine" {
		t.Errorf("unmanaged folder was touched: %q", got)
	}
	entries, err := os.ReadDir(e.addons)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("AddOns has %d entries, want only the unmanaged folder", len(entries))
	}
}

func TestSyncRemovesExactlyTheDroppedAddonsFolders(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{
		"acme/alpha": {{Tag: "v1", Assets: map[string][]byte{"a.zip": makeZip(t, map[string]string{
			"Alpha/a.lua": "a", "AlphaOptions/o.lua": "o",
		})}}},
		"acme/beta": {{Tag: "v1", Assets: map[string][]byte{"b.zip": makeZip(t, map[string]string{"Beta/b.lua": "b"})}}},
	})
	e := newSyncEnv(t, gh, twoAddons(alphaLine, betaLine))
	if err := os.MkdirAll(filepath.Join(e.addons, "Handmade"), 0o755); err != nil {
		t.Fatal(err)
	}
	if r := e.sync(t); r.ExitCode != 0 {
		t.Fatalf("first sync: exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}

	write(t, e.manifest, twoAddons("", betaLine))
	r := e.sync(t)
	if r.ExitCode != 0 {
		t.Fatalf("second sync: exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	if !strings.Contains(r.Stdout, "removed github:acme/alpha") {
		t.Errorf("stdout = %q, want the removal reported", r.Stdout)
	}
	var left []string
	entries, err := os.ReadDir(e.addons)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range entries {
		left = append(left, d.Name())
	}
	if want := []string{"Beta", "Handmade"}; !reflect.DeepEqual(left, want) {
		t.Errorf("AddOns = %v, want %v", left, want)
	}
	l, err := lock.Load(e.lock)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := l.Addons["github:acme/alpha"]; ok {
		t.Error("dropped addon is still in the lock")
	}
	if _, ok := l.Addons["github:acme/beta"]; !ok {
		t.Error("beta was removed from the lock")
	}
}

func TestSyncKeepsGoingPastAFailingAddon(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{
		"acme/beta": {{Tag: "v1", Assets: map[string][]byte{"b.zip": makeZip(t, map[string]string{"Beta/b.lua": "b"})}}},
	})
	e := newSyncEnv(t, gh, twoAddons(alphaLine, betaLine))

	r := e.sync(t)
	if r.ExitCode != 1 || !strings.Contains(r.Stderr, "github:acme/alpha") {
		t.Errorf("exit = %d, stderr = %q; want 1 naming alpha", r.ExitCode, r.Stderr)
	}
	if got := read(t, filepath.Join(e.addons, "Beta", "b.lua")); got != "b" {
		t.Errorf("Beta = %q", got)
	}
	if !strings.Contains(r.Stdout, "installed github:acme/beta v1") {
		t.Errorf("stdout = %q, want beta reported", r.Stdout)
	}
	l, err := lock.Load(e.lock)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := l.Addons["github:acme/beta"]; !ok {
		t.Error("beta not locked")
	}
}

func TestSyncSummarisesEveryFailureAndExitsOne(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{
		"acme/beta":  {{Tag: "v1", Assets: map[string][]byte{"b.zip": makeZip(t, map[string]string{"Beta/b.lua": "b"})}}},
		"acme/gamma": {{Tag: "v1", Assets: map[string][]byte{"g.zip": makeZip(t, map[string]string{"Handmade/g.lua": "g"})}}},
	})
	e := newSyncEnv(t, gh, twoAddons(alphaLine, betaLine)+"\"github:acme/gamma\" = \"latest\"\n")
	if err := os.MkdirAll(filepath.Join(e.addons, "Handmade"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, asJSON := range []bool{false, true} {
		var extra []string
		if asJSON {
			extra = []string{"--json"}
		}
		r := e.sync(t, extra...)
		if r.ExitCode != 1 {
			t.Errorf("json=%v: exit = %d, want 1; stderr = %q", asJSON, r.ExitCode, r.Stderr)
		}
		for _, want := range []string{"github:acme/alpha", "github:acme/gamma", "Handmade"} {
			if !strings.Contains(r.Stderr, want) {
				t.Errorf("json=%v: stderr lacks %q: %q", asJSON, want, r.Stderr)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(e.addons, "Beta", "b.lua")); err != nil {
		t.Errorf("beta not installed: %v", err)
	}
}

// assertNoTempFiles fails if dir holds anything wam uses only while working.
func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		if strings.HasPrefix(e.Name(), ".wam-") {
			t.Errorf("%s left in %s", e.Name(), dir)
		}
	}
}

func TestSyncUpgradeReplacesTheAddonsFolders(t *testing.T) {
	v1 := makeZip(t, map[string]string{"Alpha/a.lua": "v1", "Alpha/old.lua": "v1", "AlphaOld/x.lua": "v1"})
	v2 := makeZip(t, map[string]string{"Alpha/a.lua": "v2", "AlphaNew/y.lua": "v2"})
	gh := fakeGitHub(t, map[string][]fakeRelease{
		"acme/alpha": {{Tag: "v2", Assets: map[string][]byte{"a.zip": v2}}, {Tag: "v1", Assets: map[string][]byte{"a.zip": v1}}},
	})
	e := newSyncEnv(t, gh, pinned("v1"))
	if r := e.sync(t); r.ExitCode != 0 {
		t.Fatalf("v1: exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	write(t, e.manifest, pinned("v2"))

	if r := e.sync(t); r.ExitCode != 0 {
		t.Fatalf("v2: exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	if got := read(t, filepath.Join(e.addons, "Alpha", "a.lua")); got != "v2" {
		t.Errorf("a.lua = %q, want v2", got)
	}
	for _, p := range []string{"Alpha/old.lua", "AlphaOld"} {
		if _, err := os.Stat(filepath.Join(e.addons, filepath.FromSlash(p))); err == nil {
			t.Errorf("%s survived the upgrade", p)
		}
	}
	if _, err := os.Stat(filepath.Join(e.addons, "AlphaNew", "y.lua")); err != nil {
		t.Errorf("AlphaNew not installed: %v", err)
	}
	assertNoTempFiles(t, e.addons)
}

func TestSyncInterruptedMidDownloadExits130AndLeavesNoTempFiles(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{
		"acme/alpha": {{Tag: "v1", Assets: map[string][]byte{"a.zip": alphaZip(t, "v1")}}},
	})
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	gh.onDownload = func(req *http.Request) {
		// The signal arrives while the asset is downloading.
		cancel(errs.Interrupt(syscall.SIGINT))
		<-req.Context().Done()
	}
	e := newSyncEnv(t, gh, oneAddon)

	r := clitest.Run(t, clitest.Opts{Args: []string{"sync", "-f", e.manifest, "--dir", e.addons}, Env: e.vars, Context: ctx})
	if r.ExitCode != 130 {
		t.Errorf("exit = %d, want 130; stderr = %q", r.ExitCode, r.Stderr)
	}
	assertNoTempFiles(t, e.addons)
	if _, err := os.Stat(filepath.Join(e.addons, "Alpha")); err == nil {
		t.Error("Alpha installed although the run was interrupted")
	}
}

func TestSyncRestoresWhatACrashedRunLeftHalfSwapped(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{
		"acme/alpha": {{Tag: "v1", Assets: map[string][]byte{"a.zip": alphaZip(t, "v1")}}},
	})
	e := newSyncEnv(t, gh, oneAddon)
	if r := e.sync(t); r.ExitCode != 0 {
		t.Fatalf("exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	// A run killed after moving Alpha aside, before placing its replacement.
	aside := filepath.Join(e.addons, ".wam-old-1", "d")
	if err := os.MkdirAll(aside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(e.addons, "Alpha"), filepath.Join(aside, "Alpha")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(e.addons, ".wam-tmp-2"), 0o755); err != nil {
		t.Fatal(err)
	}

	r := e.sync(t)
	if r.ExitCode != 0 || r.Stdout != "" {
		t.Errorf("exit = %d, stdout = %q, stderr = %q; want the folder restored, not reinstalled", r.ExitCode, r.Stdout, r.Stderr)
	}
	if _, err := os.Stat(filepath.Join(e.addons, "Alpha", "Alpha.toc")); err != nil {
		t.Errorf("Alpha not restored: %v", err)
	}
	assertNoTempFiles(t, e.addons)
}

func TestSyncInterruptedPartWayLocksWhatItInstalled(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{
		"acme/alpha": {{Tag: "v1", Assets: map[string][]byte{"a.zip": alphaZip(t, "v1")}}},
		"acme/beta":  {{Tag: "v1", Assets: map[string][]byte{"b.zip": makeZip(t, map[string]string{"Beta/b.lua": "b"})}}},
	})
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	gh.onDownload = func(req *http.Request) {
		if strings.Contains(req.URL.Path, "acme/beta") {
			cancel(errs.Interrupt(syscall.SIGTERM))
			<-req.Context().Done()
		}
	}
	e := newSyncEnv(t, gh, oneAddon+`"github:acme/beta" = "latest"`+"\n")

	r := clitest.Run(t, clitest.Opts{Args: []string{"sync", "-f", e.manifest, "--dir", e.addons}, Env: e.vars, Context: ctx})
	if r.ExitCode != 143 {
		t.Errorf("exit = %d, want 143; stderr = %q", r.ExitCode, r.Stderr)
	}
	l, err := lock.Load(e.lock)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := l.Addons["github:acme/alpha"]; !ok || len(l.Addons) != 1 {
		t.Errorf("lock = %+v, want just alpha", l.Addons)
	}
	assertNoTempFiles(t, e.addons)
}

// releaseJSON is modelled on the BigWigs packager's release.json, as shipped
// by DialogKey_Numy: one entry per zip, each listing the flavors it supports.
func releaseJSON(t *testing.T, entries map[string][]string) []byte {
	t.Helper()
	type meta struct {
		Flavor    string `json:"flavor"`
		Interface int    `json:"interface"`
	}
	type rel struct {
		Name     string `json:"name"`
		Filename string `json:"filename"`
		NoLib    bool   `json:"nolib"`
		Metadata []meta `json:"metadata"`
	}
	var out struct {
		Releases []rel `json:"releases"`
	}
	for file, flavors := range entries {
		r := rel{Name: "Addon", Filename: file}
		for _, f := range flavors {
			r.Metadata = append(r.Metadata, meta{Flavor: f, Interface: 50500})
		}
		out.Releases = append(out.Releases, r)
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func entryWith(spec string, flavor string) string {
	return "[game]\nflavor = \"" + flavor + "\"\n\n[addons]\n\"github:acme/alpha\" = " + spec + "\n"
}

func zipMarker(t *testing.T, marker string) []byte {
	return makeZip(t, map[string]string{"Alpha/Alpha.toc": marker})
}

func TestSyncPicksZipFromReleaseJSONForManifestFlavor(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{"acme/alpha": {{Tag: "v1", Assets: map[string][]byte{
		"Alpha-v1-mainline.zip": zipMarker(t, "mainline"),
		"Alpha-v1-forever.zip":  zipMarker(t, "forever"),
		"release.json": releaseJSON(t, map[string][]string{
			"Alpha-v1-mainline.zip": {"mainline"},
			"Alpha-v1-forever.zip":  {"forever"},
		}),
	}}}})
	e := newSyncEnv(t, gh, entryWith(`"latest"`, "forever"))
	if r := e.sync(t); r.ExitCode != 0 {
		t.Fatalf("exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	if got := read(t, filepath.Join(e.addons, "Alpha", "Alpha.toc")); got != "forever" {
		t.Errorf("installed %q, want forever", got)
	}
}

func TestSyncAssetGlobOverridesReleaseJSON(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{"acme/alpha": {{Tag: "v1", Assets: map[string][]byte{
		"Alpha-v1-mainline.zip": zipMarker(t, "mainline"),
		"Alpha-v1-forever.zip":  zipMarker(t, "forever"),
		"release.json": releaseJSON(t, map[string][]string{
			"Alpha-v1-mainline.zip": {"mainline"},
			"Alpha-v1-forever.zip":  {"forever"},
		}),
	}}}})
	e := newSyncEnv(t, gh, entryWith(`{ version = "latest", asset = "*-mainline.zip" }`, "forever"))
	if r := e.sync(t); r.ExitCode != 0 {
		t.Fatalf("exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	if got := read(t, filepath.Join(e.addons, "Alpha", "Alpha.toc")); got != "mainline" {
		t.Errorf("installed %q, want mainline", got)
	}
}

func TestSyncUsesOnlyZipWithoutReleaseJSON(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{"acme/alpha": {{Tag: "v1", Assets: map[string][]byte{
		"Alpha.zip": zipMarker(t, "only"), "notes.txt": []byte("hi"),
	}}}})
	e := newSyncEnv(t, gh, oneAddon)
	if r := e.sync(t); r.ExitCode != 0 {
		t.Fatalf("exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
}

func TestSyncAmbiguousReleaseExits5AndListsCandidates(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{"acme/alpha": {{Tag: "v1", Assets: map[string][]byte{
		"Alpha-a.zip": zipMarker(t, "a"), "Alpha-b.zip": zipMarker(t, "b"),
	}}}})
	e := newSyncEnv(t, gh, oneAddon)
	r := e.sync(t)
	if r.ExitCode != 5 {
		t.Fatalf("exit = %d, want 5; stderr = %q", r.ExitCode, r.Stderr)
	}
	for _, want := range []string{"Alpha-a.zip", "Alpha-b.zip", "asset"} {
		if !strings.Contains(r.Stderr, want) {
			t.Errorf("stderr %q lacks %q", r.Stderr, want)
		}
	}
}

func TestSyncReleaseJSONWithoutMatchingFlavorExits5(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{"acme/alpha": {{Tag: "v1", Assets: map[string][]byte{
		"Alpha-a.zip": zipMarker(t, "a"), "Alpha-b.zip": zipMarker(t, "b"),
		"release.json": releaseJSON(t, map[string][]string{"Alpha-a.zip": {"mainline"}, "Alpha-b.zip": {"classic"}}),
	}}}})
	e := newSyncEnv(t, gh, entryWith(`"latest"`, "forever"))
	r := e.sync(t)
	if r.ExitCode != 5 || !strings.Contains(r.Stderr, "Alpha-a.zip") || !strings.Contains(r.Stderr, "Alpha-b.zip") {
		t.Errorf("exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
}

func TestSyncAssetGlobMatchingNothingOrSeveralExits5(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{"acme/alpha": {{Tag: "v1", Assets: map[string][]byte{
		"Alpha-a.zip": zipMarker(t, "a"), "Alpha-b.zip": zipMarker(t, "b"),
	}}}})
	for _, glob := range []string{"*.tar.gz", "Alpha-*.zip"} {
		e := newSyncEnv(t, gh, entryWith(`{ version = "latest", asset = "`+glob+`" }`, "mainline"))
		if r := e.sync(t); r.ExitCode != 5 {
			t.Errorf("glob %q: exit = %d, stderr = %q", glob, r.ExitCode, r.Stderr)
		}
	}
}

func TestSyncReleaseWithNoZipExits5WithClearMessage(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{"acme/alpha": {{Tag: "v1", Assets: map[string][]byte{
		"notes.txt": []byte("hi"),
	}}}})
	e := newSyncEnv(t, gh, oneAddon)
	r := e.sync(t)
	if r.ExitCode != 5 || !strings.Contains(r.Stderr, "no installable zip") {
		t.Errorf("exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
}

// pathsEnv is a manifest in a fake config dir plus an install the probe can
// find, so sync runs with no flags.
type pathsEnv struct {
	gh      *fakeServer
	config  string
	home    string
	install string
	vars    map[string]string
}

func newPathsEnv(t *testing.T, flavor string) pathsEnv {
	t.Helper()
	gh := fakeGitHub(t, map[string][]fakeRelease{"acme/alpha": {{Tag: "v1", Assets: map[string][]byte{
		"alpha.zip": makeZip(t, map[string]string{"Alpha/Alpha.toc": "x"}),
	}}}})
	root := t.TempDir()
	e := pathsEnv{gh: gh, home: filepath.Join(root, "home"), install: filepath.Join(root, "wow")}
	e.config = filepath.Join(root, "cfg", "wam")
	e.vars = map[string]string{"WAM_GITHUB_API": gh.URL, "XDG_CONFIG_HOME": filepath.Join(root, "cfg")}
	write(t, filepath.Join(e.config, "wam.toml"), "[game]\nflavor = \""+flavor+"\"\n\n[addons]\n\"github:acme/alpha\" = \"latest\"\n")
	return e
}

func (e pathsEnv) run(t *testing.T, args ...string) clitest.Result {
	t.Helper()
	probe := locate.Probe{Roots: []string{e.install}}
	return clitest.Run(t, clitest.Opts{Args: args, Env: e.vars, Home: e.home, Probe: &probe})
}

func TestSyncWithNoFlagsUsesConfigDirAndDetectedInstall(t *testing.T) {
	e := newPathsEnv(t, "mainline")
	addons := filepath.Join(e.install, "_retail_", "Interface", "AddOns")
	if err := os.MkdirAll(filepath.Join(e.install, "_retail_"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := e.run(t, "sync")
	if r.ExitCode != 0 {
		t.Fatalf("exit = %d; stderr = %q", r.ExitCode, r.Stderr)
	}
	if got := read(t, filepath.Join(addons, "Alpha", "Alpha.toc")); got != "x" {
		t.Errorf("addon not installed to detected dir: %q", got)
	}
	if _, err := os.Stat(filepath.Join(e.config, "wam.lock")); err != nil {
		t.Errorf("lock not beside the manifest in the config dir: %v", err)
	}
}

func TestSyncConfigDirDefaultsToTheHomeConfigDir(t *testing.T) {
	e := newPathsEnv(t, "mainline")
	delete(e.vars, "XDG_CONFIG_HOME")
	// ~/.config/wam, or ~/AppData/Roaming/wam on Windows when APPDATA is unset.
	cfg := filepath.Join(e.home, ".config", "wam")
	if runtime.GOOS == "windows" {
		cfg = filepath.Join(e.home, "AppData", "Roaming", "wam")
	}
	write(t, filepath.Join(cfg, "wam.toml"), oneAddon)
	r := e.run(t, "sync", "--dir", filepath.Join(t.TempDir(), "AddOns"))
	if r.ExitCode != 0 {
		t.Fatalf("exit = %d; stderr = %q", r.ExitCode, r.Stderr)
	}
	if _, err := os.Stat(filepath.Join(cfg, "wam.lock")); err != nil {
		t.Errorf("lock not in %s: %v", cfg, err)
	}
}

func TestSyncFileFlagBeatsWamFileEnv(t *testing.T) {
	e := newPathsEnv(t, "mainline")
	flagFile := filepath.Join(t.TempDir(), "flag.toml")
	envFile := filepath.Join(t.TempDir(), "env.toml")
	write(t, flagFile, oneAddon)
	write(t, envFile, "not toml [")
	e.vars["WAM_FILE"] = envFile
	if r := e.run(t, "sync", "-f", flagFile, "--dir", t.TempDir()); r.ExitCode != 0 {
		t.Fatalf("flag: exit = %d; stderr = %q", r.ExitCode, r.Stderr)
	}
	// Without the flag, WAM_FILE (invalid) is what loads.
	if r := e.run(t, "sync", "--dir", t.TempDir()); r.ExitCode != 2 {
		t.Errorf("env: exit = %d, want 2 for the invalid WAM_FILE; stderr = %q", r.ExitCode, r.Stderr)
	}
}

func TestSyncDirPrecedence(t *testing.T) {
	e := newPathsEnv(t, "mainline")
	tmp := t.TempDir()
	flagDir, envDir, manDir := filepath.Join(tmp, "flag"), filepath.Join(tmp, "env"), filepath.Join(tmp, "man")
	write(t, filepath.Join(e.config, "wam.toml"),
		"[game]\nflavor = \"mainline\"\ndir = \""+filepath.ToSlash(manDir)+"\"\n\n[addons]\n\"github:acme/alpha\" = \"latest\"\n")
	has := func(dir string) bool { _, err := os.Stat(filepath.Join(dir, "Alpha")); return err == nil }
	reset := func() {
		for _, d := range []string{flagDir, envDir, manDir} {
			_ = os.RemoveAll(d)
		}
		_ = os.Remove(filepath.Join(e.config, "wam.lock"))
	}

	e.vars["WAM_DIR"] = envDir
	if r := e.run(t, "sync", "--dir", flagDir); r.ExitCode != 0 || !has(flagDir) || has(envDir) || has(manDir) {
		t.Errorf("--dir should win: exit %d, stderr %q", r.ExitCode, r.Stderr)
	}
	reset()
	if r := e.run(t, "sync"); r.ExitCode != 0 || !has(envDir) || has(manDir) {
		t.Errorf("WAM_DIR should beat the manifest's dir: exit %d, stderr %q", r.ExitCode, r.Stderr)
	}
	reset()
	delete(e.vars, "WAM_DIR")
	if r := e.run(t, "sync"); r.ExitCode != 0 || !has(manDir) {
		t.Errorf("manifest dir should be used: exit %d, stderr %q", r.ExitCode, r.Stderr)
	}
}

func TestSyncUnknownFlavorExitsTwoListingValidOnes(t *testing.T) {
	e := newPathsEnv(t, "retail")
	r := e.run(t, "sync")
	if r.ExitCode != 2 {
		t.Fatalf("exit = %d, want 2; stderr = %q", r.ExitCode, r.Stderr)
	}
	for _, s := range []string{`"retail"`, "mainline", "forever"} {
		if !strings.Contains(r.Stderr, s) {
			t.Errorf("stderr %q lacks %s", r.Stderr, s)
		}
	}
}

func TestSyncUndetectableDirExitsTwoSayingHowToSetIt(t *testing.T) {
	e := newPathsEnv(t, "mainline") // no _retail_ folder under the install
	r := e.run(t, "sync")
	if r.ExitCode != 2 {
		t.Fatalf("exit = %d, want 2; stderr = %q", r.ExitCode, r.Stderr)
	}
	for _, s := range []string{"--dir", "WAM_DIR", "dir"} {
		if !strings.Contains(r.Stderr, s) {
			t.Errorf("stderr %q lacks %q", r.Stderr, s)
		}
	}
}

func TestSyncWithNoHomeAndNoFileIsAUsageError(t *testing.T) {
	e := newPathsEnv(t, "mainline")
	delete(e.vars, "XDG_CONFIG_HOME")
	e.home = ""
	r := e.run(t, "sync")
	if r.ExitCode != 2 || !strings.Contains(r.Stderr, "home directory") || !strings.Contains(r.Stderr, "-f") {
		t.Errorf("exit = %d, stderr = %q; want usage error naming the home directory and -f", r.ExitCode, r.Stderr)
	}
}
