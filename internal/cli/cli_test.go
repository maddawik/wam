package cli_test

import (
	"strings"
	"testing"

	"github.com/maddawik/wam/internal/clitest"
)

func TestBareWamPrintsHelpAndExitsZero(t *testing.T) {
	r := clitest.Run(t, clitest.Opts{})
	if r.ExitCode != 0 {
		t.Fatalf("exit = %d, stderr = %q", r.ExitCode, r.Stderr)
	}
	if !strings.Contains(r.Stdout, "Usage:") {
		t.Errorf("stdout lacks usage: %q", r.Stdout)
	}
}

func TestUnknownCommandSuggestsOnStderrAndExitsTwo(t *testing.T) {
	r := clitest.Run(t, clitest.Opts{Args: []string{"syc"}})
	if r.ExitCode != 2 {
		t.Errorf("exit = %d, want 2", r.ExitCode)
	}
	if !strings.Contains(r.Stderr, "Did you mean") || !strings.Contains(r.Stderr, "sync") {
		t.Errorf("stderr lacks suggestion: %q", r.Stderr)
	}
	if r.Stdout != "" {
		t.Errorf("stdout = %q, want empty", r.Stdout)
	}
}

func TestVersionPrintsInjectedVersion(t *testing.T) {
	r := clitest.Run(t, clitest.Opts{Args: []string{"version"}, Version: "1.2.3"})
	if r.ExitCode != 0 || strings.TrimSpace(r.Stdout) != "1.2.3" {
		t.Errorf("exit=%d stdout=%q", r.ExitCode, r.Stdout)
	}
}

func TestCompletionEmitsScriptForEachShell(t *testing.T) {
	for _, sh := range []string{"bash", "zsh", "fish", "powershell"} {
		r := clitest.Run(t, clitest.Opts{Args: []string{"completion", sh}})
		if r.ExitCode != 0 || len(r.Stdout) < 100 {
			t.Errorf("%s: exit=%d stdout len=%d stderr=%q", sh, r.ExitCode, len(r.Stdout), r.Stderr)
		}
	}
}

func TestHelpWorksAtEveryDepthAndShowsExamples(t *testing.T) {
	for _, args := range [][]string{
		{"--help"}, {"-h"}, {"help", "sync"}, {"sync", "-h"}, {"sync", "--help"},
		{"help", "version"}, {"version", "--help"},
		{"help", "completion"}, {"completion", "zsh", "--help"},
	} {
		r := clitest.Run(t, clitest.Opts{Args: args})
		if r.ExitCode != 0 {
			t.Errorf("%v: exit=%d stderr=%q", args, r.ExitCode, r.Stderr)
		}
		if !strings.Contains(r.Stdout, "Examples:") {
			t.Errorf("%v: no examples in %q", args, r.Stdout)
		}
	}
}

func TestUsageErrorExitsTwo(t *testing.T) {
	r := clitest.Run(t, clitest.Opts{Args: []string{"version", "--bogus"}})
	if r.ExitCode != 2 || r.Stderr == "" {
		t.Errorf("exit=%d stderr=%q", r.ExitCode, r.Stderr)
	}
}
