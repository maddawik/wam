package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/maddawik/wam/internal/errs"
	"github.com/maddawik/wam/internal/install"
	"github.com/maddawik/wam/internal/locate"
	"github.com/maddawik/wam/internal/lock"
	"github.com/maddawik/wam/internal/manifest"
	"github.com/maddawik/wam/internal/source"
	"github.com/spf13/cobra"
)

// project is a loaded manifest with the places and host it works against.
type project struct {
	file string
	m    *manifest.Manifest
	gh   *source.GitHub
	// dir resolves the AddOns folder.
	dir func() (string, error)
}

// openProject loads the manifest that -f, WAM_FILE or the default names.
func openProject(cmd *cobra.Command) (*project, error) {
	s := From(cmd)
	file, err := manifestPath(cmd, s.env)
	if err != nil {
		return nil, err
	}
	m, err := manifest.Load(file)
	if err != nil {
		return nil, err
	}
	if _, err := locate.GameFolder(m.Game.Flavor); err != nil {
		return nil, err
	}
	return &project{
		file: file,
		m:    m,
		gh:   &source.GitHub{BaseURL: s.env.Vars["WAM_GITHUB_API"], Token: s.env.Vars["GITHUB_TOKEN"]},
		dir: func() (string, error) {
			flagDir, _ := cmd.Flags().GetString("dir")
			return locate.AddOnsDir(locate.Inputs{
				Flag: flagDir, Env: s.env.Vars["WAM_DIR"], Manifest: m.Game.Dir,
				Flavor: m.Game.Flavor, Probe: probeFor(s.env),
			})
		},
	}, nil
}

// find returns the manifest index of key, or -1.
func (p *project) find(key string) int {
	for i, a := range p.m.Addons {
		if a.Key == key {
			return i
		}
	}
	return -1
}

func emitChange(s *IO, c syncChange) error {
	return s.Emit(c, func(w io.Writer) error {
		if c.Action == "added" {
			_, err := fmt.Fprintf(w, "added %s %s (installed %s)\n", c.Addon, c.Version, c.Tag)
			return err
		}
		_, err := fmt.Fprintf(w, "%s %s\n", c.Action, c.Addon)
		return err
	})
}

func newAdd() *cobra.Command {
	c := &cobra.Command{
		Use:   "add <github:owner/repo>",
		Short: "Add an addon to the manifest and install it",
		Long: "Add an addon to the manifest, resolve and lock it, and install it, in one\n" +
			"step. It follows the latest release unless --pin names an exact release\n" +
			"tag. If the addon can't be found or installed, the manifest and lock are\n" +
			"left as they were." + previewHelp,
		Example: "  wam add github:owner/repo\n  wam add github:owner/repo --pin v1.2.3",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s := From(cmd)
			key := args[0]
			id, ok := strings.CutPrefix(key, "github:")
			if owner, repo, two := strings.Cut(id, "/"); !ok || !two || owner == "" || repo == "" || strings.Contains(repo, "/") {
				return errs.New(errs.Usage, fmt.Sprintf("%q must look like github:owner/repo", key), "wam add --help")
			}
			version, _ := cmd.Flags().GetString("pin")
			if version == "" {
				version = source.Latest
			}
			p, err := openProject(cmd)
			if err != nil {
				return err
			}
			if p.find(key) >= 0 {
				return errs.New(errs.Conflict, fmt.Sprintf("%s is already in the manifest", key),
					"edit its version in "+p.file+", or run: wam update "+key)
			}
			dir, err := p.dir()
			if err != nil {
				return err
			}
			lockFile := lockPath(p.file)
			l, err := lock.Load(lockFile)
			if err != nil {
				return err
			}
			if wantsPreview(cmd) {
				a := manifest.Addon{Key: key, Source: "github", ID: id, Version: version}
				ownerOf := func(folder string) (string, bool) { return l.Owner(folder, key) }
				tag, folders, err := previewAddon(cmd.Context(), p.gh, p.m.Game.Flavor, a, dir, nil, ownerOf)
				if err != nil {
					return addonError(key, err)
				}
				return emitPlan(s, []planChange{{Action: "add", Addon: key, Version: version, To: tag, Adds: folders, Removes: []string{}}})
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot create %s", dir), "")
			}
			if err := install.Recover(dir); err != nil {
				return err
			}
			a := manifest.Addon{Key: key, Source: "github", ID: id, Version: version}
			ownerOf := func(folder string) (string, bool) { return l.Owner(folder, key) }
			prog := newProgress(s, 1)
			prog.start(key)
			entry, err := installAddon(cmd.Context(), p.gh, p.m.Game.Flavor, a, dir, nil, ownerOf)
			if err != nil {
				prog.finish(key, "failed", "")
				_ = prog.close()
				return addonError(key, err)
			}
			prog.finish(key, "installed "+entry.Tag, "")
			_ = prog.close()
			l.Addons[key] = entry
			if err := lock.Save(lockFile, l); err != nil {
				return errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot write lock %s", lockFile), "")
			}
			p.m.Addons = append(p.m.Addons, a)
			if err := manifest.Save(p.file, p.m); err != nil {
				return errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot write manifest %s", p.file), "")
			}
			return emitChange(s, syncChange{Action: "added", Addon: key, Version: version, Tag: entry.Tag, Folders: entry.Folders})
		},
	}
	c.Flags().String("pin", "", "install this exact release tag instead of the latest")
	bindPreview(c)
	return c
}

func newRemove() *cobra.Command {
	c := &cobra.Command{
		Use:   "remove <addon>",
		Short: "Remove an addon and its folders",
		Long: "Remove an addon from the manifest and the lock, and delete the folders it\n" +
			"owns from the AddOns folder. It doesn't ask first: addon folders can be\n" +
			"installed again, and settings live in WTF.\n\n" +
			"--preview (-n) lists what would be removed and changes nothing. It doesn't\n" +
			"use the network.",
		Example: "  wam remove github:owner/repo",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s := From(cmd)
			key := args[0]
			p, err := openProject(cmd)
			if err != nil {
				return err
			}
			i := p.find(key)
			if i < 0 {
				return errs.New(errs.NotFound, fmt.Sprintf("%s is not in the manifest", key), "wam sync --help")
			}
			lockFile := lockPath(p.file)
			l, err := lock.Load(lockFile)
			if err != nil {
				return err
			}
			entry, locked := l.Addons[key]
			if wantsPreview(cmd) {
				return emitPlan(s, []planChange{{Action: "remove", Addon: key, From: entry.Tag, Adds: []string{}, Removes: append([]string{}, entry.Folders...)}})
			}
			if locked && len(entry.Folders) > 0 {
				dir, err := p.dir()
				if err != nil {
					return err
				}
				if err := install.Remove(dir, entry.Folders); err != nil {
					return addonError(key, err)
				}
			}
			if locked {
				delete(l.Addons, key)
				if err := lock.Save(lockFile, l); err != nil {
					return errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot write lock %s", lockFile), "")
				}
			}
			p.m.Addons = append(p.m.Addons[:i], p.m.Addons[i+1:]...)
			if err := manifest.Save(p.file, p.m); err != nil {
				return errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot write manifest %s", p.file), "")
			}
			return emitChange(s, syncChange{Action: "removed", Addon: key, Tag: entry.Tag, Folders: entry.Folders})
		},
	}
	bindPreview(c)
	return c
}
