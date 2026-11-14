package main

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

// fakeRepo answers commands from a map keyed by the joined command line, and
// records what ran.
type fakeRepo struct {
	out   map[string]string
	calls []string
}

func (f *fakeRepo) run(_ context.Context, name string, args ...string) (string, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, line)
	for prefix, out := range f.out {
		if strings.HasPrefix(line, prefix) {
			if out == "ERR" {
				return "", errors.New(line + ": failed")
			}
			return out, nil
		}
	}
	return "", nil
}

const sha = "0123456789abcdef"

func ready() map[string]string {
	return map[string]string{
		"git branch --show-current": "main",
		"git rev-parse HEAD":        sha,
		"git rev-parse origin/main": sha,
		"gh run list":               `[{"status":"completed","conclusion":"success","url":"u"}]`,
	}
}

func TestReleaseTagsAndPushes(t *testing.T) {
	f := &fakeRepo{out: ready()}
	if err := release(context.Background(), f.run, io.Discard, "v0.1.0"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"git tag -a v0.1.0 -m wam v0.1.0", "git push origin refs/tags/v0.1.0"} {
		if !slices.Contains(f.calls, want) {
			t.Errorf("missing %q in %q", want, f.calls)
		}
	}
	if gh := f.calls[slices.IndexFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, "gh ") })]; !strings.Contains(gh, "--commit "+sha) {
		t.Errorf("gh run list not filtered to HEAD: %q", gh)
	}
}

func TestReleaseRefuses(t *testing.T) {
	tests := []struct {
		name string
		tag  string
		set  map[string]string
		want string
	}{
		{"no v prefix", "0.1.0", nil, "not a v-prefixed semver"},
		{"not semver", "v1.2", nil, "not a v-prefixed semver"},
		{"leading zero", "v01.2.3", nil, "not a v-prefixed semver"},
		{"build metadata", "v1.2.3+meta", nil, "not a v-prefixed semver"},
		{"dirty tree", "v0.1.0", map[string]string{"git status --porcelain": " M main.go"}, "uncommitted changes"},
		{"other branch", "v0.1.0", map[string]string{"git branch --show-current": "feature"}, `on branch "feature"`},
		{"behind origin", "v0.1.0", map[string]string{"git rev-parse origin/main": "fedcba"}, "not at origin/main"},
		{"tag exists locally", "v0.1.0", map[string]string{"git tag --list": "v0.1.0"}, "already exists"},
		{"tag exists on remote", "v0.1.0", map[string]string{"git ls-remote": sha + "\trefs/tags/v0.1.0"}, "already exists on origin"},
		{"no CI run", "v0.1.0", map[string]string{"gh run list": `[]`}, "no CI run"},
		{"CI running", "v0.1.0", map[string]string{"gh run list": `[{"status":"in_progress","conclusion":"","url":"u"}]`}, "still in_progress"},
		{"CI failed", "v0.1.0", map[string]string{"gh run list": `[{"status":"completed","conclusion":"failure","url":"u"}]`}, "did not pass (failure)"},
		{"fetch fails", "v0.1.0", map[string]string{"git fetch": "ERR"}, "failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := ready()
			for k, v := range tt.set {
				out[k] = v
			}
			f := &fakeRepo{out: out}
			err := release(context.Background(), f.run, io.Discard, tt.tag)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
			for _, c := range f.calls {
				if strings.HasPrefix(c, "git tag -a") || strings.HasPrefix(c, "git push") {
					t.Errorf("tagged or pushed despite refusing: %q", c)
				}
			}
		})
	}
}
