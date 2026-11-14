package cli_test

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maddawik/wam/internal/clitest"
)

// manyAddons is n addons, acme/addon0 to acme/addon<n-1>, each installing
// one folder Addon<i>.
func manyAddons(t *testing.T, n int) (map[string][]fakeRelease, string) {
	t.Helper()
	repos := map[string][]fakeRelease{}
	manifest := "[game]\nflavor = \"mainline\"\n\n[addons]\n"
	for i := range n {
		repo := fmt.Sprintf("acme/addon%d", i)
		folder := fmt.Sprintf("Addon%d", i)
		repos[repo] = []fakeRelease{{Tag: "v1", Assets: map[string][]byte{
			folder + ".zip": makeZip(t, map[string]string{folder + "/" + folder + ".toc": folder}),
		}}}
		manifest += fmt.Sprintf("%q = \"latest\"\n", "github:"+repo)
	}
	return repos, manifest
}

func TestSyncDownloadsAtMostFourAddonsAtOnce(t *testing.T) {
	repos, manifest := manyAddons(t, 7)
	gh := fakeGitHub(t, repos)
	// Hold each download until a fifth request is in flight, which a cap
	// of four never allows, so the peak is the run's real concurrency.
	gh.onDownload = func(*http.Request) { gh.waitInFlight(5, 300*time.Millisecond) }
	e := newSyncEnv(t, gh, manifest)

	r := e.sync(t)
	if r.ExitCode != 0 {
		t.Fatalf("exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	if got := gh.peakInFlight(); got != 4 {
		t.Errorf("peak concurrent requests = %d, want 4", got)
	}
	for i := range 7 {
		if _, err := os.Stat(filepath.Join(e.addons, fmt.Sprintf("Addon%d", i))); err != nil {
			t.Errorf("Addon%d not installed: %v", i, err)
		}
	}
}

func TestUpdateDownloadsAtMostFourAddonsAtOnce(t *testing.T) {
	repos, manifest := manyAddons(t, 6)
	gh := fakeGitHub(t, repos)
	e := newSyncEnv(t, gh, manifest)
	if r := e.sync(t); r.ExitCode != 0 {
		t.Fatalf("sync: exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	gh.resetPeak()
	gh.onDownload = func(*http.Request) { gh.waitInFlight(5, 300*time.Millisecond) }

	if r := e.cmd(t, "update"); r.ExitCode != 0 {
		t.Fatalf("update: exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	if got := gh.peakInFlight(); got != 4 {
		t.Errorf("peak concurrent requests = %d, want 4", got)
	}
}

func TestSyncFailingDownloadDoesNotStopTheOthers(t *testing.T) {
	repos, manifest := manyAddons(t, 4)
	gh := fakeGitHub(t, repos)
	failed := make(chan struct{})
	gh.intercept = func(w http.ResponseWriter, req *http.Request) bool {
		if strings.HasPrefix(req.URL.Path, "/download/acme/addon0/") {
			http.Error(w, "boom", http.StatusInternalServerError)
			close(failed)
			return true
		}
		return false
	}
	// The others are still downloading when addon0 fails.
	gh.onDownload = func(*http.Request) {
		select {
		case <-failed:
		case <-time.After(2 * time.Second):
		}
	}
	e := newSyncEnv(t, gh, manifest)

	r := e.sync(t)
	if r.ExitCode != 1 || !strings.Contains(r.Stderr, "github:acme/addon0") {
		t.Errorf("exit = %d, stderr = %q; want 1 naming addon0", r.ExitCode, r.Stderr)
	}
	for i := 1; i < 4; i++ {
		if !strings.Contains(r.Stdout, fmt.Sprintf("installed github:acme/addon%d v1\n", i)) {
			t.Errorf("stdout = %q, want addon%d installed", r.Stdout, i)
		}
	}
	if _, err := os.Stat(filepath.Join(e.addons, "Addon0")); err == nil {
		t.Error("Addon0 installed although its download failed")
	}
	assertNoTempFiles(t, e.addons)
}

func TestSyncFolderClaimedByTwoNewAddonsGoesToTheFirstInTheManifest(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{
		"acme/alpha": {{Tag: "v1", Assets: map[string][]byte{"a.zip": makeZip(t, map[string]string{"Shared/a.lua": "alpha"})}}},
		"acme/beta":  {{Tag: "v1", Assets: map[string][]byte{"b.zip": makeZip(t, map[string]string{"Shared/b.lua": "beta"})}}},
	})
	// beta finishes downloading well before alpha.
	gh.onDownload = func(req *http.Request) {
		if strings.Contains(req.URL.Path, "acme/alpha") {
			time.Sleep(200 * time.Millisecond)
		}
	}
	e := newSyncEnv(t, gh, twoAddons(alphaLine, betaLine))

	r := e.sync(t)
	if r.ExitCode != 1 || !strings.Contains(r.Stderr, "github:acme/beta") {
		t.Fatalf("exit = %d, stderr = %q; want 1 with beta refused", r.ExitCode, r.Stderr)
	}
	if got := read(t, filepath.Join(e.addons, "Shared", "a.lua")); got != "alpha" {
		t.Errorf("Shared/a.lua = %q, want alpha's", got)
	}
	if r.Stdout != "installed github:acme/alpha v1\n" {
		t.Errorf("stdout = %q, want only alpha installed", r.Stdout)
	}
}

func TestSyncPrintsEachAddonAsItFinishes(t *testing.T) {
	repos, manifest := manyAddons(t, 2)
	gh := fakeGitHub(t, repos)
	e := newSyncEnv(t, gh, manifest)
	var live syncBuffer
	// addon1 is held until addon0's line is on stdout, which never happens
	// if output waits for the whole run.
	var printedFirst atomic.Bool
	gh.onDownload = func(req *http.Request) {
		if !strings.Contains(req.URL.Path, "acme/addon1") {
			return
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(live.String(), "installed github:acme/addon0") {
				printedFirst.Store(true)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	r := clitest.Run(t, clitest.Opts{Args: []string{"sync", "-f", e.manifest, "--dir", e.addons}, Env: e.vars, Stdout: &live})
	if r.ExitCode != 0 {
		t.Fatalf("exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	if !printedFirst.Load() {
		t.Errorf("addon0 was not printed before addon1 finished; stdout = %q", r.Stdout)
	}
	if want := "installed github:acme/addon0 v1\ninstalled github:acme/addon1 v1\n"; r.Stdout != want {
		t.Errorf("stdout = %q, want %q", r.Stdout, want)
	}
}

func TestSyncProgressWithoutATerminalIsOnePlainLinePerAddon(t *testing.T) {
	repos, manifest := manyAddons(t, 3)
	e := newSyncEnv(t, fakeGitHub(t, repos), manifest)

	r := e.sync(t)
	if r.ExitCode != 0 {
		t.Fatalf("exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	if strings.ContainsAny(r.Stderr, "\r\x1b") {
		t.Errorf("stderr has redraws or ANSI: %q", r.Stderr)
	}
	lines := strings.Split(strings.TrimSuffix(r.Stderr, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("stderr = %q, want one line per addon", r.Stderr)
	}
	for i := range 3 {
		if !strings.Contains(r.Stderr, fmt.Sprintf("github:acme/addon%d installed v1", i)) {
			t.Errorf("stderr lacks addon%d: %q", i, r.Stderr)
		}
	}
	if !strings.HasPrefix(lines[2], "[3/3] ") {
		t.Errorf("last line = %q, want it counted [3/3]", lines[2])
	}
	if strings.ContainsAny(r.Stdout, "\r\x1b[") {
		t.Errorf("stdout is not clean data: %q", r.Stdout)
	}
}

func TestSyncProgressOnATerminalRedrawsStderrOnly(t *testing.T) {
	repos, manifest := manyAddons(t, 3)
	e := newSyncEnv(t, fakeGitHub(t, repos), manifest)

	r := clitest.Run(t, clitest.Opts{Args: []string{"sync", "-f", e.manifest, "--dir", e.addons}, Env: e.vars, StderrTTY: true})
	if r.ExitCode != 0 {
		t.Fatalf("exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	if !strings.Contains(r.Stderr, "\r\x1b[K") || !strings.Contains(r.Stderr, "downloading") {
		t.Errorf("stderr = %q, want a redrawn status line", r.Stderr)
	}
	if !strings.HasSuffix(r.Stderr, "\r\x1b[K") {
		t.Errorf("stderr = %q, want the status line cleared at the end", r.Stderr)
	}
	if strings.ContainsAny(r.Stdout, "\r\x1b") || strings.Count(r.Stdout, "\n") != 3 {
		t.Errorf("stdout = %q, want three clean lines", r.Stdout)
	}
}

func TestSyncProgressIsSilentUnderQuietAndJSON(t *testing.T) {
	repos, manifest := manyAddons(t, 2)
	gh := fakeGitHub(t, repos)
	for _, flag := range []string{"--quiet", "--json"} {
		e := newSyncEnv(t, gh, manifest)
		r := clitest.Run(t, clitest.Opts{Args: []string{"sync", "-f", e.manifest, "--dir", e.addons, flag}, Env: e.vars, StderrTTY: true})
		if r.ExitCode != 0 || r.Stderr != "" {
			t.Errorf("%s: exit = %d, stderr = %q; want nothing on stderr", flag, r.ExitCode, r.Stderr)
		}
	}
}

// syncBuffer is a bytes.Buffer safe to read while the run writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
