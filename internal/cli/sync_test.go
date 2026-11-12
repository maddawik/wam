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
	"strings"
	"testing"

	"github.com/maddawik/wam/internal/clitest"
)

type fakeRelease struct {
	Tag        string
	Draft      bool
	Prerelease bool
	// Assets maps asset name to content.
	Assets map[string][]byte
}

// fakeGitHub serves the subset of the GitHub REST API wam uses, from repos
// keyed "owner/repo", newest release first.
func fakeGitHub(t *testing.T, repos map[string][]fakeRelease) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	for repo, releases := range repos {
		mux.HandleFunc("/repos/"+repo+"/releases", func(w http.ResponseWriter, _ *http.Request) {
			var out []map[string]any
			for _, r := range releases {
				var assets []map[string]any
				for name := range r.Assets {
					assets = append(assets, map[string]any{
						"name":                 name,
						"browser_download_url": fmt.Sprintf("%s/download/%s/%s/%s", srv.URL, repo, r.Tag, name),
					})
				}
				out = append(out, map[string]any{
					"tag_name": r.Tag, "draft": r.Draft, "prerelease": r.Prerelease, "assets": assets,
				})
			}
			_ = json.NewEncoder(w).Encode(out)
		})
		for _, r := range releases {
			for name, body := range r.Assets {
				mux.HandleFunc(fmt.Sprintf("/download/%s/%s/%s", repo, r.Tag, name), func(w http.ResponseWriter, _ *http.Request) {
					_, _ = w.Write(body)
				})
			}
		}
	}
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
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

func newSyncEnv(t *testing.T, gh *httptest.Server, manifest string) syncEnv {
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

	lock := read(t, e.lock)
	for _, want := range []string{
		`github:acme/alpha`,
		`source = 'github'`,
		`repo = 'acme/alpha'`,
		`tag = 'v2.0.0'`,
		`asset = 'Alpha-v2.0.0.zip'`,
		`url = '` + gh.URL + `/download/acme/alpha/v2.0.0/Alpha-v2.0.0.zip'`,
		`sha256 = '` + sha(zipBytes) + `'`,
		`'Alpha'`,
		`'AlphaOptions'`,
	} {
		if !strings.Contains(lock, want) {
			t.Errorf("lock lacks %s:\n%s", want, lock)
		}
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
