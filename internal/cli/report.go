package cli

import (
	"fmt"
	"net/url"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/maddawik/wam/internal/errs"
	"github.com/spf13/cobra"
)

const issueURL = "https://github.com/maddawik/wam/issues/new"

type envelope struct {
	Error envelopeBody `json:"error"`
}

type envelopeBody struct {
	Code      string `json:"code"`
	ExitCode  int    `json:"exit_code"`
	Message   string `json:"message"`
	Run       string `json:"run"`
	Retryable bool   `json:"retryable"`
}

// fail reports err on stderr and returns its exit code. cmd is the command
// that failed, or nil.
func (inv *invocation) fail(cmd *cobra.Command, err error) int {
	e, ok := errs.As(err)
	if !ok {
		kind := errs.Runtime
		if !inv.parsed {
			kind = errs.Usage
		}
		e = errs.Wrap(kind, err, firstLine(err.Error()), "")
	}
	run := e.Run
	if run == "" {
		run = defaultRun(cmd, e)
	}
	inv.report(envelopeBody{
		Code:      e.CodeOrDefault(),
		ExitCode:  e.Kind.ExitCode(),
		Message:   firstLine(e.Message),
		Run:       run,
		Retryable: e.Retryable,
	})
	return e.Kind.ExitCode()
}

func (inv *invocation) report(b envelopeBody) {
	s := inv.io
	if inv.jsonRequested() {
		_ = writeJSON(s.Err, envelope{Error: b})
		return
	}
	p := s.ErrColor()
	_, _ = fmt.Fprintf(s.Err, "%s %s\n  %s %s\n", p.Bold(p.Red("Error:")), b.Message, "Try:", p.Bold(b.Run))
}

// jsonRequested is true when --json was parsed, or appears in the arguments
// of a run that failed before flag parsing finished.
func (inv *invocation) jsonRequested() bool {
	if inv.io.json {
		return true
	}
	for _, a := range inv.args {
		if a == "--" {
			break
		}
		if a == "--json" || a == "--json=true" {
			return true
		}
	}
	return false
}

// defaultRun suggests a fix for an error that didn't name one: re-running a
// retryable command, otherwise the failing command's help.
func defaultRun(cmd *cobra.Command, e *errs.Error) string {
	path := "wam"
	if cmd != nil {
		path = cmd.CommandPath()
	}
	if e.Retryable && len(path) > len("wam") {
		return path
	}
	return path + " --help"
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// crash reports a recovered panic as a wam bug and returns the exit code.
func (inv *invocation) crash(r any) int {
	command := strings.TrimSpace("wam " + strings.Join(inv.args, " "))
	platform := runtime.GOOS + "/" + runtime.GOARCH
	report := url.Values{
		"title": {fmt.Sprintf("crash: %v", r)},
		"body":  {fmt.Sprintf("version: %s\nplatform: %s\ncommand: %s\n\npanic: %v", inv.version, platform, command, r)},
	}
	link := issueURL + "?" + report.Encode()
	code := errs.Runtime.ExitCode()

	s := inv.io
	if inv.jsonRequested() {
		inv.report(envelopeBody{
			Code:     "bug",
			ExitCode: code,
			Message:  fmt.Sprintf("wam crashed (this is a bug in wam): %v [version %s, %s, command %q]", r, inv.version, platform, command),
			Run:      link,
		})
		return code
	}
	p := s.ErrColor()
	_, _ = fmt.Fprintf(s.Err, "%s wam crashed. This is a bug in wam, not in your setup.\n", p.Bold(p.Red("Bug:")))
	_, _ = fmt.Fprintf(s.Err, "  panic:    %v\n  version:  %s\n  platform: %s\n  command:  %s\n", r, inv.version, platform, command)
	_, _ = fmt.Fprintf(s.Err, "Please report it: %s\n", link)
	if s.Verbose() {
		_, _ = s.Err.Write(debug.Stack())
	}
	return code
}
