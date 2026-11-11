package main

import (
	"context"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/maddawik/wam/internal/cli"
	"github.com/maddawik/wam/internal/errs"
	"golang.org/x/term"
)

// version is injected at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigs
		// A second signal gets the default behaviour and kills the process.
		signal.Stop(sigs)
		cancel(errs.Interrupt(sig))
	}()

	vars := environ()
	stdoutTTY, stderrTTY := isTTY(os.Stdout), isTTY(os.Stderr)
	// Windows consoles need ANSI/VT processing switched on. If that fails,
	// color would print as escape codes, so turn it off.
	if (stdoutTTY && !enableVT(os.Stdout)) || (stderrTTY && !enableVT(os.Stderr)) {
		vars["NO_COLOR"] = "1"
	}

	return cli.Execute(ctx, os.Args[1:], cli.Env{
		Stdin:     os.Stdin,
		Stdout:    os.Stdout,
		Stderr:    os.Stderr,
		StdinTTY:  isTTY(os.Stdin),
		StdoutTTY: stdoutTTY,
		StderrTTY: stderrTTY,
		Vars:      vars,
	}, version)
}

func isTTY(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

func environ() map[string]string {
	m := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}
