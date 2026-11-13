package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/maddawik/wam/internal/errs"
	"github.com/maddawik/wam/internal/locate"
	"github.com/maddawik/wam/internal/manifest"
	"github.com/spf13/cobra"
)

// initResult is what init wrote.
type initResult struct {
	Manifest string `json:"manifest"`
	Flavor   string `json:"flavor"`
	Dir      string `json:"dir,omitempty"`
}

func newInit() *cobra.Command {
	c := &cobra.Command{
		Use:   "init",
		Short: "Create a starter manifest",
		Long: "Create a starter manifest with the game flavor and no addons. The flavor\n" +
			"is detected from the installed game folders: one is used as is, several\n" +
			"are asked about at a terminal, and without a terminal --flavor is\n" +
			"required. dir is written only when --dir or WAM_DIR sets it. Existing\n" +
			"addons are not imported.",
		Example: "  wam init\n  wam init --flavor mainline\n  wam init -f wam.toml --flavor classic --dir ./AddOns --overwrite",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := From(cmd)
			file, err := manifestPath(cmd, s.env)
			if err != nil {
				return err
			}
			overwrite, _ := cmd.Flags().GetBool("overwrite")
			if _, err := os.Stat(file); err == nil && !overwrite {
				return errs.New(errs.Conflict, fmt.Sprintf("manifest %s already exists", file),
					"wam init --overwrite")
			} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot check %s", file), "")
			}

			flavor, _ := cmd.Flags().GetString("flavor")
			if flavor == "" {
				probe := probeFor(s.env)
				if flavor, err = chooseFlavor(s, locate.Flavors(probe)); err != nil {
					return err
				}
			} else if _, err := locate.GameFolder(flavor); err != nil {
				return err
			}

			dir, _ := cmd.Flags().GetString("dir")
			if dir == "" {
				dir = s.env.Vars["WAM_DIR"]
			}
			if dir != "" {
				if dir, err = filepath.Abs(dir); err != nil {
					return errs.Wrap(errs.Usage, err, "cannot resolve the AddOns folder path", "")
				}
			}
			if err := manifest.Save(file, &manifest.Manifest{Game: manifest.Game{Flavor: flavor, Dir: dir}}); err != nil {
				return errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot write manifest %s", file), "")
			}
			res := initResult{Manifest: file, Flavor: flavor, Dir: dir}
			return s.Emit(res, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "wrote %s\n", file)
				return err
			})
		},
	}
	c.Flags().String("flavor", "", "game flavor to write (default: the installed one)")
	c.Flags().Bool("overwrite", false, "replace an existing manifest")
	return c
}

// probeFor is the install probe for e.
func probeFor(e Env) locate.Probe {
	if e.Probe != nil {
		return *e.Probe
	}
	return locate.DefaultProbe(e.GOOS, e.Vars)
}

// chooseFlavor picks one of found: the only one, or the user's pick at a
// terminal. Otherwise it is a usage error naming --flavor.
func chooseFlavor(s *IO, found []string) (string, error) {
	if len(found) == 1 {
		return found[0], nil
	}
	if len(found) == 0 {
		return "", errs.New(errs.Usage, "no installed game flavor found; pass --flavor", "wam init --flavor <flavor>")
	}
	fallback := errs.New(errs.Usage,
		fmt.Sprintf("several flavors are installed (%s); pass --flavor", strings.Join(found, ", ")),
		"wam init --flavor "+found[0])
	if !s.CanPrompt() {
		return "", fallback
	}
	for i, f := range found {
		s.Infof("  %d) %s", i+1, f)
	}
	s.Infof("To skip this question, run: wam init --flavor <flavor>")
	ans, err := s.Prompt("Flavor (number or name)?", fallback)
	if err != nil {
		return "", err
	}
	ans = strings.TrimSpace(ans)
	if n, err := strconv.Atoi(ans); err == nil && n >= 1 && n <= len(found) {
		return found[n-1], nil
	}
	if slices.Contains(found, ans) {
		return ans, nil
	}
	return "", errs.New(errs.Usage, fmt.Sprintf("%q is not one of: %s", ans, strings.Join(found, ", ")),
		"wam init --flavor "+found[0])
}
