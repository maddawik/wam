// Package clitest runs the wam root command in-process for tests.
package clitest

import (
	"bytes"
	"strings"
	"testing"

	"github.com/maddawik/wam/internal/cli"
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
	code := cli.Execute(o.Args, cli.Env{
		Stdin:     strings.NewReader(o.Stdin),
		Stdout:    &stdout,
		Stderr:    &stderr,
		StdinTTY:  o.StdinTTY,
		StdoutTTY: o.StdoutTTY,
		StderrTTY: o.StderrTTY,
		Vars:      env,
	}, version)
	return Result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: code, TempDir: t.TempDir()}
}
