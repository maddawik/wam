// Package clitest runs the wam root command in-process for tests.
package clitest

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/maddawik/wam/internal/cli"
	"github.com/maddawik/wam/internal/locate"
	"github.com/spf13/cobra"
)

// Opts configures a single in-process run. Nothing is read from the real
// process environment: Env is the only source of environment variables.
type Opts struct {
	Args      []string
	Stdin     string
	Env       map[string]string
	StdinTTY  bool
	StdoutTTY bool
	StderrTTY bool
	Version   string
	// GOOS and Home default to the host's. Probe defaults to the real
	// probe for GOOS.
	GOOS  string
	Home  string
	Probe *locate.Probe
	// Context defaults to context.Background(). Cancel it with an
	// errs.Interrupt cause to simulate a signal.
	Context context.Context
	// Commands are added to the root, to inject behaviour no real command
	// has yet (a given error, a panic, a prompt).
	Commands []*cobra.Command
	// Stdout, if set, also receives stdout as it is written, for tests that
	// watch output during the run. It must be safe for concurrent reads.
	Stdout io.Writer
}

// Result is what a run produced.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	// TempDir is a fresh directory removed when the test ends.
	TempDir string
}

// Run executes the root command with o and captures its outputs.
func Run(t *testing.T, o Opts) Result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	env := o.Env
	if env == nil {
		env = map[string]string{}
	}
	version := o.Version
	if version == "" {
		version = "test"
	}
	ctx := o.Context
	if ctx == nil {
		ctx = context.Background()
	}
	var out io.Writer = &stdout
	if o.Stdout != nil {
		out = io.MultiWriter(&stdout, o.Stdout)
	}
	code := cli.Execute(ctx, o.Args, cli.Env{
		Stdin:     strings.NewReader(o.Stdin),
		Stdout:    out,
		Stderr:    &stderr,
		StdinTTY:  o.StdinTTY,
		StdoutTTY: o.StdoutTTY,
		StderrTTY: o.StderrTTY,
		Vars:      env,
		GOOS:      o.GOOS,
		Home:      o.Home,
		Probe:     o.Probe,
	}, version, o.Commands...)
	return Result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: code, TempDir: t.TempDir()}
}
