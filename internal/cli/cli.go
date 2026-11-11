// Package cli defines the wam command tree.
package cli

import (
	"errors"
	"fmt"
	"io"

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
func Execute(args []string, e Env, version string) int {
	root := newRoot(e, version)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		_, _ = fmt.Fprintln(e.Stderr, "Error:", err)
		return 2
	}
	return 0
}

func newRoot(e Env, version string) *cobra.Command {
	root := &cobra.Command{
		Use:           "wam",
		Short:         "wam command-line tool",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	root.AddCommand(newSync(), newVersion(version))
	addCompletion(root)
	root.SetIn(e.Stdin)
	root.SetOut(e.Stdout)
	root.SetErr(e.Stderr)
	return root
}

// newSync is a placeholder until the real sync command lands.
func newSync() *cobra.Command {
	return &cobra.Command{
		Use:     "sync",
		Short:   "Sync (not implemented yet)",
		Example: "  wam sync\n  wam sync --help",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return errors.New("sync is not implemented yet")
		},
	}
}

func newVersion(version string) *cobra.Command {
	return &cobra.Command{
		Use:     "version",
		Short:   "Print the wam version",
		Example: "  wam version",
		Args:    cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), version)
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
