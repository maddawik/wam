package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/maddawik/wam/internal/errs"
	"github.com/spf13/cobra"
)

// IO is a command's view of its streams and the global output flags.
// Commands get it with From.
type IO struct {
	In  io.Reader
	Out io.Writer // data only
	Err io.Writer // diagnostics, warnings and errors

	env Env

	json, quiet, verbose, noColor, noInput bool

	in *bufio.Reader
}

type ioKey struct{}

// From returns the IO for cmd. It panics if cmd was not run by Execute.
func From(cmd *cobra.Command) *IO {
	return cmd.Context().Value(ioKey{}).(*IO)
}

func (s *IO) with(ctx context.Context) context.Context {
	return context.WithValue(ctx, ioKey{}, s)
}

func (s *IO) bindFlags(root *cobra.Command) {
	f := root.PersistentFlags()
	f.BoolVar(&s.json, "json", false, "write output and errors as JSON")
	f.BoolVarP(&s.quiet, "quiet", "q", false, "print only data and errors")
	f.BoolVarP(&s.verbose, "verbose", "v", false, "print extra diagnostics (wins over --quiet)")
	f.BoolVar(&s.noColor, "no-color", false, "disable color")
	f.BoolVar(&s.noInput, "no-input", false, "never prompt; fail instead (implied by CI=true)")
}

// JSON reports whether output is JSON.
func (s *IO) JSON() bool { return s.json }

// Verbose reports whether extra diagnostics are wanted.
func (s *IO) Verbose() bool { return s.verbose }

// Quiet reports whether non-essential diagnostics are suppressed.
// --verbose wins over --quiet.
func (s *IO) Quiet() bool { return s.quiet && !s.verbose }

// Infof writes a diagnostic line to stderr unless quiet.
func (s *IO) Infof(format string, a ...any) {
	if !s.Quiet() {
		_, _ = fmt.Fprintf(s.Err, format+"\n", a...)
	}
}

// Debugf writes a diagnostic line to stderr only when verbose.
func (s *IO) Debugf(format string, a ...any) {
	if s.verbose {
		_, _ = fmt.Fprintf(s.Err, format+"\n", a...)
	}
}

// Emit writes v to stdout as JSON under --json, and otherwise calls text.
// Both renderings come from the same value.
func (s *IO) Emit(v any, text func(w io.Writer) error) error {
	if s.json {
		return writeJSON(s.Out, v)
	}
	return text(s.Out)
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// OutColor is the palette for stdout.
func (s *IO) OutColor() Palette { return Palette{on: s.colorAllowed(s.env.StdoutTTY)} }

// ErrColor is the palette for stderr.
func (s *IO) ErrColor() Palette { return Palette{on: s.colorAllowed(s.env.StderrTTY)} }

func (s *IO) colorAllowed(tty bool) bool {
	return tty && !s.noColor && s.env.Vars["NO_COLOR"] == "" && s.env.Vars["TERM"] != "dumb"
}

// CanPrompt reports whether the user may be asked a question: stdin is a
// terminal, --no-input is not set, and CI is not true.
func (s *IO) CanPrompt() bool {
	ci, _ := strconv.ParseBool(s.env.Vars["CI"])
	return s.env.StdinTTY && !s.noInput && !ci
}

// Prompt asks question on stderr and returns the answer line. When prompting
// is not allowed it returns fallback, which should be a Usage error naming the
// flag that supplies the answer.
func (s *IO) Prompt(question string, fallback *errs.Error) (string, error) {
	if !s.CanPrompt() {
		return "", fallback
	}
	_, _ = fmt.Fprint(s.Err, question+" ")
	if s.in == nil {
		s.in = bufio.NewReader(s.In)
	}
	line, err := s.in.ReadString('\n')
	if err != nil && line == "" {
		return "", fallback
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// Palette applies ANSI styles when color is enabled for a stream. Callers
// must carry status in words or symbols too; color is only a cue.
type Palette struct{ on bool }

func (p Palette) wrap(code, s string) string {
	if !p.on {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// Red styles s red.
func (p Palette) Red(s string) string { return p.wrap("31", s) }

// Green styles s green.
func (p Palette) Green(s string) string { return p.wrap("32", s) }

// Yellow styles s yellow.
func (p Palette) Yellow(s string) string { return p.wrap("33", s) }

// Bold styles s bold.
func (p Palette) Bold(s string) string { return p.wrap("1", s) }
