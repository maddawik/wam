package cli

import (
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type helpDoc struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	Short    string    `json:"short"`
	Long     string    `json:"long,omitempty"`
	Usage    string    `json:"usage"`
	Examples []string  `json:"examples"`
	Flags    []flagDoc `json:"flags"`
	Commands []cmdDoc  `json:"commands"`
}

type flagDoc struct {
	Name      string `json:"name"`
	Shorthand string `json:"shorthand,omitempty"`
	Type      string `json:"type"`
	Default   string `json:"default"`
	Usage     string `json:"usage"`
	Global    bool   `json:"global"`
}

type cmdDoc struct {
	Name  string `json:"name"`
	Short string `json:"short"`
}

// installHelp makes help render as JSON under --json at every level.
func installHelp(root *cobra.Command, s *IO) {
	text := root.HelpFunc()
	root.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		if !s.JSON() {
			text(cmd, args)
			return
		}
		_ = writeJSON(s.Out, describe(cmd))
	})
}

func describe(cmd *cobra.Command) helpDoc {
	d := helpDoc{
		Name:     cmd.Name(),
		Path:     cmd.CommandPath(),
		Short:    cmd.Short,
		Long:     cmd.Long,
		Usage:    cmd.UseLine(),
		Examples: []string{},
		Flags:    []flagDoc{},
		Commands: []cmdDoc{},
	}
	for _, ex := range strings.Split(cmd.Example, "\n") {
		if ex = strings.TrimSpace(ex); ex != "" {
			d.Examples = append(d.Examples, ex)
		}
	}
	add := func(global bool) func(*pflag.Flag) {
		return func(f *pflag.Flag) {
			if f.Hidden {
				return
			}
			d.Flags = append(d.Flags, flagDoc{
				Name: f.Name, Shorthand: f.Shorthand, Type: f.Value.Type(),
				Default: f.DefValue, Usage: f.Usage, Global: global,
			})
		}
	}
	cmd.LocalFlags().VisitAll(add(false))
	cmd.InheritedFlags().VisitAll(add(true))
	for _, c := range cmd.Commands() {
		if c.IsAvailableCommand() || c.Name() == "help" {
			d.Commands = append(d.Commands, cmdDoc{Name: c.Name(), Short: c.Short})
		}
	}
	return d
}
