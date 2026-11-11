package cli_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"syscall"
	"testing"

	"github.com/maddawik/wam/internal/clitest"
	"github.com/maddawik/wam/internal/errs"
	"github.com/spf13/cobra"
)

// failing returns a command named "fail" whose run returns err.
func failing(err error) *cobra.Command {
	return &cobra.Command{
		Use: "fail",
		RunE: func(*cobra.Command, []string) error {
			return err
		},
	}
}

func TestTypedErrorsMapToExitCodes(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{errs.New(errs.Runtime, "upstream broke", "wam sync"), 1},
		{errors.New("untyped failure"), 1},
		{errs.New(errs.Usage, "missing --flavor", "wam init --flavor mainline"), 2},
		{errs.New(errs.Auth, "token rejected", "export GITHUB_TOKEN=<token>"), 3},
		{errs.New(errs.NotFound, "no such repo", "wam add github:owner/repo"), 4},
		{errs.New(errs.Conflict, "folder owned by another addon", "wam list"), 5},
		{errs.Interrupt(syscall.SIGINT), 130},
		{errs.Interrupt(syscall.SIGTERM), 143},
		{errs.Wrap(errs.NotFound, errors.New("404"), "no such repo", "wam list"), 4},
	}
	for _, c := range cases {
		r := clitest.Run(t, clitest.Opts{Args: []string{"fail"}, Commands: []*cobra.Command{failing(c.err)}})
		if r.ExitCode != c.want {
			t.Errorf("%v: exit = %d, want %d", c.err, r.ExitCode, c.want)
		}
		if r.Stdout != "" {
			t.Errorf("%v: stdout = %q, want empty", c.err, r.Stdout)
		}
	}
}

func TestErrorTextIsOneLineThenFixCommand(t *testing.T) {
	r := clitest.Run(t, clitest.Opts{
		Args:     []string{"fail"},
		Commands: []*cobra.Command{failing(errs.New(errs.Auth, "GitHub rejected the token", "export GITHUB_TOKEN=<new token>"))},
	})
	lines := strings.Split(strings.TrimRight(r.Stderr, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("stderr = %q, want two lines", r.Stderr)
	}
	if !strings.Contains(lines[0], "Error") || !strings.Contains(lines[0], "GitHub rejected the token") {
		t.Errorf("first line = %q", lines[0])
	}
	if !strings.HasSuffix(lines[1], "export GITHUB_TOKEN=<new token>") {
		t.Errorf("second line = %q", lines[1])
	}
}

func TestUntypedErrorStillHasFixLine(t *testing.T) {
	r := clitest.Run(t, clitest.Opts{Args: []string{"fail"}, Commands: []*cobra.Command{failing(errors.New("disk full"))}})
	lines := strings.Split(strings.TrimRight(r.Stderr, "\n"), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "disk full") || !strings.Contains(lines[1], "wam fail --help") {
		t.Errorf("stderr = %q", r.Stderr)
	}
}

func TestCancelledContextReportsSignalExitCode(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errs.Interrupt(syscall.SIGTERM))
	wait := &cobra.Command{
		Use: "fail",
		RunE: func(cmd *cobra.Command, _ []string) error {
			<-cmd.Context().Done()
			return cmd.Context().Err()
		},
	}
	r := clitest.Run(t, clitest.Opts{Args: []string{"fail"}, Context: ctx, Commands: []*cobra.Command{wait}})
	if r.ExitCode != 143 {
		t.Errorf("exit = %d, want 143; stderr = %q", r.ExitCode, r.Stderr)
	}
}

type envelope struct {
	Error struct {
		Code      string `json:"code"`
		ExitCode  int    `json:"exit_code"`
		Message   string `json:"message"`
		Run       string `json:"run"`
		Retryable bool   `json:"retryable"`
	} `json:"error"`
}

func TestJSONErrorEnvelopeOnStderrAndNothingOnStdout(t *testing.T) {
	e := errs.New(errs.NotFound, "no release matches v9", "wam update foo")
	e.Code = "release_not_found"
	r := clitest.Run(t, clitest.Opts{Args: []string{"fail", "--json"}, Commands: []*cobra.Command{failing(e)}})
	if r.Stdout != "" {
		t.Errorf("stdout = %q, want empty", r.Stdout)
	}
	var env envelope
	if err := json.Unmarshal([]byte(r.Stderr), &env); err != nil {
		t.Fatalf("stderr is not JSON: %v: %q", err, r.Stderr)
	}
	got := env.Error
	if got.Code != "release_not_found" || got.ExitCode != 4 || got.Message != "no release matches v9" || got.Run != "wam update foo" || got.Retryable {
		t.Errorf("envelope = %+v", got)
	}
	if r.ExitCode != 4 {
		t.Errorf("exit = %d", r.ExitCode)
	}
}

func TestJSONEnvelopeDefaultsCodeFromKind(t *testing.T) {
	r := clitest.Run(t, clitest.Opts{Args: []string{"fail", "--json"}, Commands: []*cobra.Command{failing(errs.Interrupt(syscall.SIGINT))}})
	var env envelope
	if err := json.Unmarshal([]byte(r.Stderr), &env); err != nil {
		t.Fatalf("stderr is not JSON: %q", r.Stderr)
	}
	if env.Error.Code != "interrupted" || env.Error.ExitCode != 130 || !env.Error.Retryable {
		t.Errorf("envelope = %+v", env.Error)
	}
}

func TestJSONEnvelopeForUsageErrors(t *testing.T) {
	for _, args := range [][]string{{"syc", "--json"}, {"version", "--bogus", "--json"}, {"--json", "version", "extra"}} {
		r := clitest.Run(t, clitest.Opts{Args: args})
		var env envelope
		if err := json.Unmarshal([]byte(r.Stderr), &env); err != nil {
			t.Errorf("%v: stderr is not JSON: %q", args, r.Stderr)
			continue
		}
		if r.Stdout != "" || r.ExitCode != 2 || env.Error.ExitCode != 2 || env.Error.Code != "usage" || env.Error.Run == "" {
			t.Errorf("%v: exit=%d stdout=%q envelope=%+v", args, r.ExitCode, r.Stdout, env.Error)
		}
	}
}
