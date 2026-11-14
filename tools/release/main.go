// Command release cuts a wam release. It checks that the repo is ready, then
// creates and pushes an annotated tag, which triggers the release workflow.
// It calls git and gh directly so it works without a POSIX shell.
//
// Usage: mise run release v1.2.3
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
)

const (
	branch   = "main"
	remote   = "origin"
	workflow = "ci.yml"
)

// semverTag is a v-prefixed semver version with an optional prerelease.
var semverTag = regexp.MustCompile(`^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)

// runner runs a command and returns its trimmed stdout.
type runner func(ctx context.Context, name string, args ...string) (string, error)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: mise run release <version>, e.g. mise run release v0.1.0")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := release(ctx, execRun, os.Stderr, os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

// release checks that tag can be cut from HEAD, then tags and pushes it.
func release(ctx context.Context, run runner, log io.Writer, tag string) error {
	if err := check(ctx, run, tag); err != nil {
		return err
	}
	if _, err := run(ctx, "git", "tag", "-a", tag, "-m", "wam "+tag); err != nil {
		return err
	}
	if _, err := run(ctx, "git", "push", remote, "refs/tags/"+tag); err != nil {
		return err
	}
	_, err := fmt.Fprintf(log, "pushed %s; the release workflow publishes it\n", tag)
	return err
}

// check refuses an invalid or existing tag, a dirty tree, a branch other
// than an up-to-date main, and a HEAD without a passing CI run.
func check(ctx context.Context, run runner, tag string) error {
	if !semverTag.MatchString(tag) {
		return fmt.Errorf("%q is not a v-prefixed semver version, e.g. v0.1.0", tag)
	}
	status, err := run(ctx, "git", "status", "--porcelain")
	if err != nil {
		return err
	}
	if status != "" {
		return errors.New("the working tree has uncommitted changes")
	}
	cur, err := run(ctx, "git", "branch", "--show-current")
	if err != nil {
		return err
	}
	if cur != branch {
		return fmt.Errorf("on branch %q; check out %s", cur, branch)
	}
	if _, err := run(ctx, "git", "fetch", "--quiet", "--tags", remote, branch); err != nil {
		return err
	}
	head, err := run(ctx, "git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	upstream, err := run(ctx, "git", "rev-parse", remote+"/"+branch)
	if err != nil {
		return err
	}
	if head != upstream {
		return fmt.Errorf("%s is not at %s/%s; pull or push first", branch, remote, branch)
	}
	if out, err := run(ctx, "git", "tag", "--list", tag); err != nil {
		return err
	} else if out != "" {
		return fmt.Errorf("tag %s already exists", tag)
	}
	if out, err := run(ctx, "git", "ls-remote", "--tags", remote, "refs/tags/"+tag); err != nil {
		return err
	} else if out != "" {
		return fmt.Errorf("tag %s already exists on %s", tag, remote)
	}
	return checkCI(ctx, run, head)
}

// checkCI requires the latest CI run for sha on main to have passed.
func checkCI(ctx context.Context, run runner, sha string) error {
	out, err := run(ctx, "gh", "run", "list",
		"--workflow", workflow, "--branch", branch, "--commit", sha, "--event", "push",
		"--limit", "1", "--json", "status,conclusion,url")
	if err != nil {
		return err
	}
	var runs []struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		URL        string `json:"url"`
	}
	if err := json.Unmarshal([]byte(out), &runs); err != nil {
		return fmt.Errorf("cannot read gh run list output: %w", err)
	}
	short := sha[:min(len(sha), 7)]
	switch {
	case len(runs) == 0:
		return fmt.Errorf("no CI run found for %s", short)
	case runs[0].Status != "completed":
		return fmt.Errorf("CI for %s is still %s: %s", short, runs[0].Status, runs[0].URL)
	case runs[0].Conclusion != "success":
		return fmt.Errorf("CI for %s did not pass (%s): %s", short, runs[0].Conclusion, runs[0].URL)
	}
	return nil
}

func execRun(ctx context.Context, name string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
		}
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}
