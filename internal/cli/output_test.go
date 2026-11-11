package cli_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/maddawik/wam/internal/cli"
	"github.com/maddawik/wam/internal/clitest"
	"github.com/maddawik/wam/internal/errs"
	"github.com/spf13/cobra"
)

// status prints a colored status word on stdout and a diagnostic on stderr.
func status() *cobra.Command {
	return &cobra.Command{
		Use: "status",
		Run: func(cmd *cobra.Command, _ []string) {
			s := cli.From(cmd)
			_, _ = fmt.Fprintln(s.Out, s.OutColor().Green("✓ installed"))
			s.Infof("info line")
			s.Debugf("debug line")
		},
	}
}

func TestColorOnAtTTY(t *testing.T) {
	r := clitest.Run(t, clitest.Opts{Args: []string{"status"}, StdoutTTY: true, Commands: []*cobra.Command{status()}})
	if !strings.Contains(r.Stdout, "\x1b[") {
		t.Errorf("stdout = %q, want color at a TTY", r.Stdout)
	}
}

func TestColorOff(t *testing.T) {
	cases := map[string]clitest.Opts{
		"NO_COLOR":   {Args: []string{"status"}, StdoutTTY: true, Env: map[string]string{"NO_COLOR": "1"}},
		"TERM=dumb":  {Args: []string{"status"}, StdoutTTY: true, Env: map[string]string{"TERM": "dumb"}},
		"--no-color": {Args: []string{"status", "--no-color"}, StdoutTTY: true},
		"not a TTY":  {Args: []string{"status"}, StdoutTTY: false},
	}
	for name, o := range cases {
		o.Commands = []*cobra.Command{status()}
		r := clitest.Run(t, o)
		if strings.Contains(r.Stdout, "\x1b[") {
			t.Errorf("%s: stdout = %q, want no color", name, r.Stdout)
		}
		if !strings.Contains(r.Stdout, "✓ installed") {
			t.Errorf("%s: status word missing: %q", name, r.Stdout)
		}
	}
}

func TestColorIsCheckedPerStream(t *testing.T) {
	r := clitest.Run(t, clitest.Opts{
		Args: []string{"fail"}, StdoutTTY: true, StderrTTY: false,
		Commands: []*cobra.Command{failing(errs.New(errs.Runtime, "boom", "wam sync"))},
	})
	if strings.Contains(r.Stderr, "\x1b[") {
		t.Errorf("stderr colored though not a TTY: %q", r.Stderr)
	}
	r = clitest.Run(t, clitest.Opts{
		Args: []string{"fail"}, StderrTTY: true,
		Commands: []*cobra.Command{failing(errs.New(errs.Runtime, "boom", "wam sync"))},
	})
	if !strings.Contains(r.Stderr, "\x1b[") || !strings.Contains(r.Stderr, "Error") {
		t.Errorf("stderr at a TTY = %q, want colored Error word", r.Stderr)
	}
}

func TestDiagnosticsGoToStderrAndQuietVerboseControlThem(t *testing.T) {
	cases := []struct {
		flags       []string
		info, debug bool
	}{
		{nil, true, false},
		{[]string{"-q"}, false, false},
		{[]string{"-v"}, true, true},
		{[]string{"-q", "-v"}, true, true},
	}
	for _, c := range cases {
		r := clitest.Run(t, clitest.Opts{Args: append([]string{"status"}, c.flags...), Commands: []*cobra.Command{status()}})
		if strings.Contains(r.Stdout, "line") {
			t.Errorf("%v: diagnostics on stdout: %q", c.flags, r.Stdout)
		}
		if got := strings.Contains(r.Stderr, "info line"); got != c.info {
			t.Errorf("%v: info shown = %v, want %v", c.flags, got, c.info)
		}
		if got := strings.Contains(r.Stderr, "debug line"); got != c.debug {
			t.Errorf("%v: debug shown = %v, want %v", c.flags, got, c.debug)
		}
	}
}

func asker() *cobra.Command {
	return &cobra.Command{
		Use: "ask",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := cli.From(cmd)
			ans, err := s.Prompt("Flavor?", errs.New(errs.Usage, "several flavors installed", "wam init --flavor <name>"))
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(s.Out, "answer:", ans)
			return nil
		},
	}
}

func TestPromptsAnswerAtATerminal(t *testing.T) {
	r := clitest.Run(t, clitest.Opts{Args: []string{"ask"}, StdinTTY: true, Stdin: "classic\n", Commands: []*cobra.Command{asker()}})
	if r.ExitCode != 0 || strings.TrimSpace(r.Stdout) != "answer: classic" {
		t.Errorf("exit=%d stdout=%q stderr=%q", r.ExitCode, r.Stdout, r.Stderr)
	}
	if !strings.Contains(r.Stderr, "Flavor?") {
		t.Errorf("question not on stderr: %q", r.Stderr)
	}
}

func TestPromptsImpossibleWithoutInteractiveInput(t *testing.T) {
	cases := map[string]clitest.Opts{
		"stdin not a TTY": {Args: []string{"ask"}},
		"--no-input":      {Args: []string{"ask", "--no-input"}, StdinTTY: true},
		"CI=true":         {Args: []string{"ask"}, StdinTTY: true, Env: map[string]string{"CI": "true"}},
	}
	for name, o := range cases {
		o.Stdin = "classic\n"
		o.Commands = []*cobra.Command{asker()}
		r := clitest.Run(t, o)
		if r.ExitCode != 2 || r.Stdout != "" {
			t.Errorf("%s: exit=%d stdout=%q", name, r.ExitCode, r.Stdout)
		}
		if strings.Contains(r.Stderr, "Flavor?") || !strings.Contains(r.Stderr, "wam init --flavor <name>") {
			t.Errorf("%s: stderr = %q", name, r.Stderr)
		}
	}
}

func panicking() *cobra.Command {
	return &cobra.Command{
		Use: "explode",
		Run: func(*cobra.Command, []string) { panic("kaboom") },
	}
}

func TestPanicIsReportedAsWamBug(t *testing.T) {
	r := clitest.Run(t, clitest.Opts{Args: []string{"explode"}, Version: "1.2.3", Commands: []*cobra.Command{panicking()}})
	if r.ExitCode == 0 {
		t.Errorf("exit = 0, want non-zero")
	}
	for _, want := range []string{"bug in wam", "kaboom", "1.2.3", "wam explode", "https://github.com/maddawik/wam/issues/new"} {
		if !strings.Contains(r.Stderr, want) {
			t.Errorf("crash report lacks %q: %q", want, r.Stderr)
		}
	}
	if r.Stdout != "" {
		t.Errorf("stdout = %q", r.Stdout)
	}
}

func TestPanicUnderJSONIsAnEnvelope(t *testing.T) {
	r := clitest.Run(t, clitest.Opts{Args: []string{"explode", "--json"}, Commands: []*cobra.Command{panicking()}})
	var env envelope
	if err := json.Unmarshal([]byte(r.Stderr), &env); err != nil {
		t.Fatalf("stderr is not JSON: %q", r.Stderr)
	}
	if r.ExitCode == 0 || env.Error.Code != "bug" || !strings.Contains(env.Error.Message, "kaboom") || r.Stdout != "" {
		t.Errorf("exit=%d envelope=%+v stdout=%q", r.ExitCode, env.Error, r.Stdout)
	}
}

func TestVersionJSON(t *testing.T) {
	r := clitest.Run(t, clitest.Opts{Args: []string{"version", "--json"}, Version: "1.2.3"})
	var v struct {
		Version string `json:"version"`
		OS      string `json:"os"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &v); err != nil {
		t.Fatalf("stdout is not JSON: %q", r.Stdout)
	}
	if r.ExitCode != 0 || v.Version != "1.2.3" || v.OS == "" {
		t.Errorf("exit=%d version=%+v", r.ExitCode, v)
	}
}

func TestHelpJSON(t *testing.T) {
	for _, args := range [][]string{{"help", "--json"}, {"--help", "--json"}, {"help", "version", "--json"}} {
		r := clitest.Run(t, clitest.Opts{Args: args})
		var h struct {
			Path     string                  `json:"path"`
			Examples []string                `json:"examples"`
			Flags    []struct{ Name string } `json:"flags"`
		}
		if err := json.Unmarshal([]byte(r.Stdout), &h); err != nil {
			t.Errorf("%v: stdout is not JSON: %q", args, r.Stdout)
			continue
		}
		if r.ExitCode != 0 || !strings.HasPrefix(h.Path, "wam") || len(h.Examples) == 0 {
			t.Errorf("%v: exit=%d help=%+v", args, r.ExitCode, h)
		}
		var names []string
		for _, f := range h.Flags {
			names = append(names, f.Name)
		}
		if !strings.Contains(strings.Join(names, ","), "json") {
			t.Errorf("%v: flags = %v, want global --json listed", args, names)
		}
	}
}
