// Package clitest runs the wam root command in-process for tests.
package clitest

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/maddawik/wam/internal/cli"
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
	// Context defaults to context.Background(). Cancel it with an
	// errs.Interrupt cause to simulate a signal.
	Context context.Context
	// Commands are added to the root, to inject behaviour no real command
	// has yet (a given error, a panic, a prompt).
	Commands []*cobra.Command
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
	code := cli.Execute(ctx, o.Args, cli.Env{
		Stdin:     strings.NewReader(o.Stdin),
		Stdout:    &stdout,
		Stderr:    &stderr,
		StdinTTY:  o.StdinTTY,
		StdoutTTY: o.StdoutTTY,
		StderrTTY: o.StderrTTY,
		Vars:      env,
	}, version, o.Commands...)
	return Result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: code, TempDir: t.TempDir()}
}
