package cli

import (
	"fmt"
	"io"

	"github.com/maddawik/wam/internal/install"
	"github.com/maddawik/wam/internal/lock"
	"github.com/spf13/cobra"
)

// Install states reported by list, as they appear in --json.
const (
	statusInstalled = "installed"
	statusMissing   = "missing"
	statusModified  = "modified"
)

// listedAddon is one line of list. The JSON field names are a stable
// contract: addon is the manifest key, version the manifest's wanted version,
// locked the locked release tag ("" when never installed) and status one of
// "installed", "missing" or "modified".
type listedAddon struct {
	Addon   string `json:"addon"`
	Version string `json:"version"`
	Locked  string `json:"locked"`
	Status  string `json:"status"`
}

var statusText = map[string]string{
	statusInstalled: "✓ installed",
	statusMissing:   "✗ missing",
	statusModified:  "! modified",
}

// entryStatus compares what e recorded with what is in addonsDir. A lock
// written before trees were recorded can't show modification, so it reports
// installed while its folders are present.
func entryStatus(addonsDir string, e lock.Entry) (string, error) {
	if !installed(addonsDir, e.Folders) {
		return statusMissing, nil
	}
	if e.Tree == "" {
		return statusInstalled, nil
	}
	sum, err := install.Digest(addonsDir, e.Folders)
	if err != nil {
		return "", err
	}
	if sum != e.Tree {
		return statusModified, nil
	}
	return statusInstalled, nil
}

func newList() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List addons with their install status",
		Long: "List every addon in the manifest, one per line: name, wanted version,\n" +
			"locked tag (- if none) and status. Status is ✓ installed, ✗ missing (an\n" +
			"owned folder is absent) or ! modified (owned folders differ from what\n" +
			"wam installed). Addons locked before wam recorded contents can't show\n" +
			"! modified. It never uses the network.\n\n" +
			"--json prints an array of objects with the stable fields addon, version,\n" +
			"locked (empty if none) and status (installed, missing or modified).",
		Example: "  wam list\n  wam list --json",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := From(cmd)
			p, err := openProject(cmd)
			if err != nil {
				return err
			}
			l, err := lock.Load(lockPath(p.file))
			if err != nil {
				return err
			}
			dir, err := p.dir()
			if err != nil {
				return err
			}
			out := []listedAddon{}
			for _, a := range p.m.Addons {
				la := listedAddon{Addon: a.Key, Version: a.Version, Status: statusMissing}
				if e, ok := l.Addons[a.Key]; ok {
					la.Locked = e.Tag
					if la.Status, err = entryStatus(dir, e); err != nil {
						return err
					}
				}
				out = append(out, la)
			}
			return s.Emit(out, func(w io.Writer) error {
				if len(out) == 0 && s.env.StdoutTTY {
					_, err := fmt.Fprintln(w, "no addons in the manifest")
					return err
				}
				for _, a := range out {
					locked := a.Locked
					if locked == "" {
						locked = "-"
					}
					if _, err := fmt.Fprintf(w, "%s %s %s %s\n", a.Addon, a.Version, locked, statusText[a.Status]); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}
}
