package cli

import (
	"fmt"
	"slices"
	"sync"
)

// progress reports a command's way through its addons. Each addon's stdout
// line is written as it finishes. On stderr, a terminal gets one status line
// redrawn in place; anything else gets one plain line per finished addon.
// Stderr status is off under --quiet and --json, and stdout lines under
// --json, which writes everything at the end instead. Its methods are safe
// to call from any goroutine.
type progress struct {
	s      *IO
	status bool // report on stderr
	tty    bool // redraw a status line rather than print lines
	text   bool // write stdout lines

	mu     sync.Mutex
	total  int
	done   int
	active []string // addons downloading, oldest first
	drawn  bool
	err    error // the first stdout write error
}

// statusWidth caps the status line so it fits a narrow terminal unwrapped.
const statusWidth = 72

func newProgress(s *IO, total int) *progress {
	return &progress{
		s:      s,
		status: !s.Quiet() && !s.JSON(),
		tty:    s.env.StderrTTY && s.env.Vars["TERM"] != "dumb",
		text:   !s.JSON(),
		total:  total,
	}
}

// start marks key as downloading.
func (p *progress) start(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active = append(p.active, key)
	p.redraw()
}

// finish marks key done. outcome says what happened, for stderr; line, if
// not empty, is key's stdout line.
func (p *progress) finish(key, outcome, line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active = slices.DeleteFunc(p.active, func(k string) bool { return k == key })
	p.done++
	p.clear()
	if line != "" && p.text && p.err == nil {
		_, p.err = fmt.Fprintln(p.s.Out, line)
	}
	if p.status && !p.tty {
		_, _ = fmt.Fprintf(p.s.Err, "[%d/%d] %s %s\n", p.done, p.total, key, outcome)
	}
	p.redraw()
}

// infof writes a diagnostic line without breaking the status line.
func (p *progress) infof(format string, a ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clear()
	p.s.Infof(format, a...)
	p.redraw()
}

// close clears the status line and returns the first stdout write error.
func (p *progress) close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active = nil
	p.clear()
	return p.err
}

func (p *progress) redraw() {
	if !p.status || !p.tty || len(p.active) == 0 {
		return
	}
	p.clear()
	msg := fmt.Sprintf("[%d/%d] downloading %s", p.done, p.total, p.active[0])
	if more := len(p.active) - 1; more > 0 {
		msg = fmt.Sprintf("[%d/%d] downloading %d addons: %s", p.done, p.total, len(p.active), p.active[0])
	}
	// A line that wraps can't be cleared with one carriage return.
	if r := []rune(msg); len(r) > statusWidth {
		msg = string(r[:statusWidth-1]) + "…"
	}
	_, _ = fmt.Fprint(p.s.Err, msg)
	p.drawn = true
}

func (p *progress) clear() {
	if p.drawn {
		_, _ = fmt.Fprint(p.s.Err, "\r\x1b[K")
		p.drawn = false
	}
}
