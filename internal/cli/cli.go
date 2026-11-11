// Package cli defines the wam command tree and the output and error contract
// every command shares.
package cli

import (
	"context"
	"fmt"
	"io"
	"runtime"

	"github.com/maddawik/wam/internal/errs"
	"github.com/spf13/cobra"
)

// Env is everything the command tree may touch outside its own arguments.
// The real process environment is never read from inside this package.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	StdinTTY  bool
	StdoutTTY bool
	StderrTTY bool

	// Vars is the environment variable map.
	Vars map[string]string
}

// Execute runs the wam command tree and returns the process exit code.
// Cancelling ctx with an *errs.Error cause (see errs.Interrupt) makes that
// cause the reported error. extra commands are added to the root; tests use
// them to inject behaviour.
func Execute(ctx context.Context, args []string, e Env, version string, extra ...*cobra.Command) (code int) {
	if e.Vars == nil {
		e.Vars = map[string]string{}
	}
	s := &IO{In: e.Stdin, Out: e.Stdout, Err: e.Stderr, env: e}
	inv := &invocation{io: s, version: version, args: args}
	defer func() {
		if r := recover(); r != nil {
			code = inv.crash(r)
		}
	}()

	root := newRoot(inv)
	root.AddCommand(extra...)
	markParsed(root, inv)
	root.SetArgs(args)
	cmd, err := root.ExecuteContextC(s.with(ctx))
	// A signal outranks whatever the command made of it.
	if sig, ok := errs.As(context.Cause(ctx)); ok {
		err = sig
	}
	if err == nil {
		return 0
	}
	return inv.fail(cmd, err)
}

// markParsed wraps every command's run so inv.parsed is set once cobra has
// accepted flags, arguments, required flags and flag groups.
func markParsed(c *cobra.Command, inv *invocation) {
	if run := c.Run; run != nil {
		c.Run = func(cmd *cobra.Command, args []string) {
			inv.parsed = true
			run(cmd, args)
		}
	}
	if run := c.RunE; run != nil {
		c.RunE = func(cmd *cobra.Command, args []string) error {
			inv.parsed = true
			return run(cmd, args)
		}
	}
	for _, sub := range c.Commands() {
		markParsed(sub, inv)
	}
}

// invocation is one run of the tree: what reporting errors and crashes needs.
type invocation struct {
	io      *IO
	version string
	args    []string
	// parsed is set once flags and arguments have been accepted. An untyped
	// error before that point is a usage error.
	parsed bool
}

func newRoot(inv *invocation) *cobra.Command {
	s := inv.io
	root := &cobra.Command{
		Use:           "wam",
		Short:         "wam command-line tool",
		Example:       "  wam version\n  wam sync\n  wam completion zsh",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          unknownCommand,
		// Cobra only applies this default on its own unknown-command path,
		// which unknownCommand replaces.
		SuggestionsMinimumDistance: 2,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	s.bindFlags(root)
	installHelp(root, s)
	root.AddCommand(newSync(), newVersion(inv.version))
	addCompletion(root)
	root.SetIn(s.In)
	root.SetOut(s.Out)
	root.SetErr(s.Err)
	return root
}

// unknownCommand rejects stray arguments to the root with a suggestion.
func unknownCommand(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	msg := fmt.Sprintf("unknown command %q", args[0])
	run := "wam --help"
	if s := cmd.SuggestionsFor(args[0]); len(s) > 0 {
		msg += fmt.Sprintf(". Did you mean %s?", s[0])
		run = "wam " + s[0]
	}
	return errs.New(errs.Usage, msg, run)
}

// newSync is a placeholder until the real sync command lands.
func newSync() *cobra.Command {
	return &cobra.Command{
		Use:     "sync",
		Short:   "Sync (not implemented yet)",
		Example: "  wam sync\n  wam sync --help",
		RunE: func(*cobra.Command, []string) error {
			return errs.New(errs.Runtime, "sync is not implemented yet", "wam --help")
		},
	}
}

type versionInfo struct {
	Version string `json:"version"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Go      string `json:"go"`
}

func newVersion(version string) *cobra.Command {
	return &cobra.Command{
		Use:     "version",
		Short:   "Print the wam version",
		Example: "  wam version\n  wam version --json",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			v := versionInfo{Version: version, OS: runtime.GOOS, Arch: runtime.GOARCH, Go: runtime.Version()}
			return From(cmd).Emit(v, func(w io.Writer) error {
				_, err := fmt.Fprintln(w, v.Version)
				return err
			})
		},
	}
}

// addCompletion installs cobra's completion command and gives every level
// copy-pasteable examples.
func addCompletion(root *cobra.Command) {
	root.InitDefaultCompletionCmd()
	c, _, err := root.Find([]string{"completion"})
	if err != nil || c == root {
		return
	}
	c.Example = "  wam completion zsh > \"${fpath[1]}/_wam\"\n  source <(wam completion bash)"
	for _, sh := range c.Commands() {
		sh.Example = "  wam completion " + sh.Name()
		// Cobra's generated commands write to os.Stdout; route through the
		// injected writer instead.
		sh.RunE = completionRun(sh.Name())
		sh.Run = nil
	}
}

func completionRun(shell string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		out := cmd.OutOrStdout()
		noDesc, _ := cmd.Flags().GetBool("no-descriptions")
		root := cmd.Root()
		switch shell {
		case "bash":
			return root.GenBashCompletionV2(out, !noDesc)
		case "zsh":
			if noDesc {
				return root.GenZshCompletionNoDesc(out)
			}
			return root.GenZshCompletion(out)
		case "fish":
			return root.GenFishCompletion(out, !noDesc)
		default:
			if noDesc {
				return root.GenPowerShellCompletion(out)
			}
			return root.GenPowerShellCompletionWithDesc(out)
		}
	}
}
