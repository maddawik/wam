package cli_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maddawik/wam/internal/clitest"
)

type errBody struct {
	Error struct {
		Code      string `json:"code"`
		ExitCode  int    `json:"exit_code"`
		Message   string `json:"message"`
		Retryable bool   `json:"retryable"`
	} `json:"error"`
}

func syncJSON(t *testing.T, e syncEnv) (clitest.Result, errBody) {
	t.Helper()
	r := e.sync(t, "--json")
	var b errBody
	if err := json.Unmarshal([]byte(r.Stderr), &b); err != nil {
		t.Fatalf("stderr is not an error envelope: %q", r.Stderr)
	}
	return r, b
}

func TestSyncSendsGitHubTokenOnlyWhenSet(t *testing.T) {
	for _, tc := range []struct {
		name, token, want string
	}{
		{"set", "s3cret", "Bearer s3cret"},
		{"unset", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var seen []string
			gh := fakeGitHub(t, nil)
			gh.intercept = func(w http.ResponseWriter, r *http.Request) bool {
				mu.Lock()
				seen = append(seen, r.Header.Get("Authorization"))
				mu.Unlock()
				return false
			}
			e := newSyncEnv(t, gh, oneAddon)
			if tc.token != "" {
				e.vars["GITHUB_TOKEN"] = tc.token
			}
			e.sync(t)
			if len(seen) == 0 {
				t.Fatal("no request reached GitHub")
			}
			for _, got := range seen {
				if got != tc.want {
					t.Errorf("Authorization = %q, want %q", got, tc.want)
				}
			}
		})
	}
}

func TestSyncRateLimitExitsOneAndHintsAtToken(t *testing.T) {
	reset := time.Now().Add(10 * time.Minute).Unix()
	for _, tc := range []struct {
		name   string
		status int
	}{{"403", 403}, {"429", 429}} {
		t.Run(tc.name, func(t *testing.T) {
			gh := fakeGitHub(t, nil)
			gh.intercept = func(w http.ResponseWriter, r *http.Request) bool {
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.Header().Set("X-RateLimit-Reset", itoa(reset))
				w.WriteHeader(tc.status)
				return true
			}
			r, b := syncJSON(t, newSyncEnv(t, gh, oneAddon))
			if r.ExitCode != 1 || b.Error.ExitCode != 1 || b.Error.Code != "rate_limited" {
				t.Errorf("exit = %d, body = %+v", r.ExitCode, b.Error)
			}
			for _, want := range []string{"GITHUB_TOKEN", "0 requests left", time.Unix(reset, 0).UTC().Format(time.RFC3339)} {
				if !strings.Contains(b.Error.Message, want) {
					t.Errorf("message %q lacks %q", b.Error.Message, want)
				}
			}
		})
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestSyncRejectedTokenExitsThree(t *testing.T) {
	for _, status := range []int{401, 403} {
		gh := fakeGitHub(t, nil)
		gh.intercept = func(w http.ResponseWriter, r *http.Request) bool {
			w.Header().Set("X-RateLimit-Remaining", "42")
			w.WriteHeader(status)
			return true
		}
		e := newSyncEnv(t, gh, oneAddon)
		e.vars["GITHUB_TOKEN"] = "bad"
		r, b := syncJSON(t, e)
		if r.ExitCode != 3 || b.Error.Code != "auth" || b.Error.Retryable {
			t.Errorf("status %d: exit = %d, body = %+v", status, r.ExitCode, b.Error)
		}
		if !strings.Contains(b.Error.Message, "GITHUB_TOKEN") {
			t.Errorf("message %q does not mention GITHUB_TOKEN", b.Error.Message)
		}
	}
}

func TestSyncMissingTagExitsFourNamingTheTag(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{"acme/alpha": {{Tag: "v1"}}})
	e := newSyncEnv(t, gh, `[game]
flavor = "mainline"

[addons]
"github:acme/alpha" = "v9"
`)
	r, b := syncJSON(t, e)
	if r.ExitCode != 4 || b.Error.Code != "not_found" || !strings.Contains(b.Error.Message, "v9") {
		t.Errorf("exit = %d, body = %+v", r.ExitCode, b.Error)
	}
}

func TestSyncMissingRepoJSONCode(t *testing.T) {
	r, b := syncJSON(t, newSyncEnv(t, fakeGitHub(t, nil), oneAddon))
	if r.ExitCode != 4 || b.Error.Code != "not_found" || !strings.Contains(b.Error.Message, "acme/alpha") {
		t.Errorf("exit = %d, body = %+v", r.ExitCode, b.Error)
	}
}

func TestSyncNetworkFailureIsRetryable(t *testing.T) {
	gh := fakeGitHub(t, nil)
	e := newSyncEnv(t, gh, oneAddon)
	gh.Close() // connection refused
	r, b := syncJSON(t, e)
	if r.ExitCode != 1 || b.Error.Code != "runtime" || !b.Error.Retryable {
		t.Errorf("exit = %d, body = %+v", r.ExitCode, b.Error)
	}
}

func TestSyncTakesNoTokenArgument(t *testing.T) {
	r := clitest.Run(t, clitest.Opts{Args: []string{"sync", "--token", "x"}})
	if r.ExitCode != 2 {
		t.Errorf("exit = %d, want 2 (unknown flag)", r.ExitCode)
	}
}

func TestSyncRejectsUnauthenticatedWithTokenHint(t *testing.T) {
	gh := fakeGitHub(t, nil)
	gh.intercept = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("X-RateLimit-Remaining", "42")
		w.WriteHeader(401)
		return true
	}
	r, b := syncJSON(t, newSyncEnv(t, gh, oneAddon))
	if r.ExitCode != 3 || b.Error.Code != "auth" || !strings.Contains(b.Error.Message, "GITHUB_TOKEN") {
		t.Errorf("exit = %d, body = %+v", r.ExitCode, b.Error)
	}
}

func TestSyncNoPublishedReleaseExitsFour(t *testing.T) {
	gh := fakeGitHub(t, map[string][]fakeRelease{"acme/alpha": {{Tag: "v1", Draft: true}}})
	r, b := syncJSON(t, newSyncEnv(t, gh, oneAddon))
	if r.ExitCode != 4 || b.Error.Code != "not_found" || !strings.Contains(b.Error.Message, "acme/alpha") {
		t.Errorf("exit = %d, body = %+v", r.ExitCode, b.Error)
	}
}

func downloadFails(t *testing.T, status int, headers map[string]string) (clitest.Result, errBody) {
	t.Helper()
	gh := fakeGitHub(t, map[string][]fakeRelease{"acme/alpha": {{
		Tag: "v1", Assets: map[string][]byte{"alpha.zip": alphaZip(t, "x")},
	}}})
	gh.intercept = func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.URL.Path, "/download/") {
			return false
		}
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		return true
	}
	return syncJSON(t, newSyncEnv(t, gh, oneAddon))
}

func TestSyncDownloadRateLimitExitsOneWithHint(t *testing.T) {
	r, b := downloadFails(t, 429, map[string]string{"X-RateLimit-Remaining": "0"})
	if r.ExitCode != 1 || b.Error.Code != "rate_limited" || !b.Error.Retryable ||
		!strings.Contains(b.Error.Message, "GITHUB_TOKEN") {
		t.Errorf("exit = %d, body = %+v", r.ExitCode, b.Error)
	}
}

func TestSyncDownloadForbiddenExitsThree(t *testing.T) {
	r, b := downloadFails(t, 403, nil)
	if r.ExitCode != 3 || b.Error.Code != "auth" {
		t.Errorf("exit = %d, body = %+v", r.ExitCode, b.Error)
	}
}

func TestSyncDownloadServerErrorIsRetryable(t *testing.T) {
	r, b := downloadFails(t, 503, nil)
	if r.ExitCode != 1 || b.Error.Code != "runtime" || !b.Error.Retryable {
		t.Errorf("exit = %d, body = %+v", r.ExitCode, b.Error)
	}
}
