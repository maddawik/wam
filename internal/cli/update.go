package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/maddawik/wam/internal/errs"
	"github.com/maddawik/wam/internal/install"
	"github.com/maddawik/wam/internal/lock"
	"github.com/maddawik/wam/internal/manifest"
	"github.com/maddawik/wam/internal/source"
	"github.com/spf13/cobra"
)

// isPinned reports whether a stays on one release until the manifest changes.
func isPinned(a manifest.Addon) bool { return a.Version != source.Latest }

func newUpdate() *cobra.Command {
	return &cobra.Command{
		Use:   "update [addon]...",
		Short: "Move addons to their newest release",
		Long: "Re-resolve unpinned addons to their newest non-prerelease release, rewrite\n" +
			"the lock and install them. Name addons to update only those. Pinned\n" +
			"addons are never moved to another release; naming one says it's pinned\n" +
			"and exits 0, unless its asset changed since it was locked, in which case\n" +
			"update locks the new asset's SHA-256 and installs it. It uses the network.",
		Example: "  wam update\n  wam update github:owner/repo",
		RunE: func(cmd *cobra.Command, args []string) error {
			s := From(cmd)
			p, err := openProject(cmd)
			if err != nil {
				return err
			}
			targets := []manifest.Addon{}
			if len(args) == 0 {
				for _, a := range p.m.Addons {
					if !isPinned(a) {
						targets = append(targets, a)
					}
				}
			}
			for _, key := range slices.Compact(slices.Sorted(slices.Values(args))) {
				i := p.find(key)
				if i < 0 {
					return errs.New(errs.NotFound, fmt.Sprintf("%s is not in the manifest", key), "wam add "+key)
				}
				targets = append(targets, p.m.Addons[i])
			}
			dir, err := p.dir()
			if err != nil {
				return err
			}
			changes, err := runUpdate(cmd.Context(), s, p, targets, dir)
			if len(changes) > 0 || err == nil {
				if eerr := s.Emit(changes, func(w io.Writer) error {
					for _, c := range changes {
						msg := fmt.Sprintf("updated %s %s", c.Addon, c.Tag)
						if c.From != "" && c.From != c.Tag {
							msg = fmt.Sprintf("updated %s %s -> %s", c.Addon, c.From, c.Tag)
						}
						if _, err := fmt.Fprintln(w, msg); err != nil {
							return err
						}
					}
					return nil
				}); err == nil {
					err = eerr
				}
			}
			return err
		},
	}
}

// runUpdate re-resolves targets, installs what changed and saves the lock
// after each. A failing addon is skipped and the rest carry on.
func runUpdate(ctx context.Context, s *IO, p *project, targets []manifest.Addon, addonsDir string) (changes []syncChange, err error) {
	changes = []syncChange{}
	if len(targets) == 0 {
		return changes, nil
	}
	lockFile := lockPath(p.file)
	l, err := lock.Load(lockFile)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(addonsDir, 0o755); err != nil {
		return nil, errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot create %s", addonsDir), "")
	}
	if err := install.Recover(addonsDir); err != nil {
		return nil, err
	}
	var failed []*errs.Error
	defer func() {
		if err == nil {
			err = failure(failed, len(changes) > 0)
		}
	}()
	for _, a := range targets {
		if err := ctx.Err(); err != nil {
			return changes, err
		}
		if a.Source != "github" {
			failed = append(failed, addonError(a.Key, errs.New(errs.Usage, fmt.Sprintf("source %q is not supported; only github", a.Source), "wam update --help")))
			continue
		}
		var locked *lock.Entry
		if e, ok := l.Addons[a.Key]; ok {
			locked = &e
		}
		ownerOf := func(folder string) (string, bool) { return l.Owner(folder, a.Key) }
		entry, changed, err := updateAddon(ctx, p.gh, p.m.Game.Flavor, a, locked, addonsDir, ownerOf)
		if err != nil {
			failed = append(failed, addonError(a.Key, err))
			continue
		}
		if !changed {
			if isPinned(a) {
				s.Infof("%s is pinned to %s; edit its version in %s to move it", a.Key, a.Version, p.file)
			}
			continue
		}
		c := syncChange{Action: "updated", Addon: a.Key, Tag: entry.Tag, Folders: entry.Folders}
		if locked != nil {
			c.From = locked.Tag
		}
		l.Addons[a.Key] = entry
		changes = append(changes, c)
		if err := lock.Save(lockFile, l); err != nil {
			return changes, errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot write lock %s", lockFile), "")
		}
	}
	return changes, nil
}

// updateAddon resolves a again, whatever the lock says, and installs the
// result unless it is exactly what is already locked and installed.
func updateAddon(ctx context.Context, src source.Source, flavor string, a manifest.Addon, locked *lock.Entry, addonsDir string, ownerOf install.OwnerFunc) (entry lock.Entry, changed bool, err error) {
	rel, err := src.Resolve(ctx, a.ID, source.Spec{Version: a.Version, Asset: a.Asset, Flavor: flavor})
	if err != nil {
		return lock.Entry{}, false, err
	}
	zipPath, sum, cleanup, err := fetch(ctx, src, rel.URL, addonsDir)
	if err != nil {
		return lock.Entry{}, false, err
	}
	defer cleanup()
	if locked != nil && locked.Tag == rel.Tag && locked.SHA256 == sum && installed(addonsDir, locked.Folders) {
		return *locked, false, nil
	}
	var owned []string
	if locked != nil {
		owned = locked.Folders
	}
	folders, err := install.Zip(ctx, zipPath, addonsDir, owned, ownerOf)
	if err != nil {
		return lock.Entry{}, false, err
	}
	tree, err := install.Digest(addonsDir, folders)
	if err != nil {
		return lock.Entry{}, false, err
	}
	return lock.Entry{
		Source: a.Source, Repo: a.ID, Version: a.Version, Tag: rel.Tag, Asset: rel.Asset, URL: rel.URL,
		SHA256: sum, Folders: folders, Tree: tree,
	}, true, nil
}

// outdatedAddon is an addon whose newest release isn't the locked one.
type outdatedAddon struct {
	Addon  string `json:"addon"`
	Locked string `json:"locked"`
	Latest string `json:"latest"`
}

func newOutdated() *cobra.Command {
	return &cobra.Command{
		Use:   "outdated",
		Short: "List addons with a newer release",
		Long: "List the unpinned, locked addons whose newest release is newer than the\n" +
			"locked one, one per line: name, locked tag, latest tag. It changes\n" +
			"nothing. It uses the network to ask each addon's host, and is the only\n" +
			"read command that does.",
		Example: "  wam outdated\n  wam outdated --json",
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
			out := []outdatedAddon{}
			var failed []*errs.Error
			for _, a := range p.m.Addons {
				if err := cmd.Context().Err(); err != nil {
					return err
				}
				e, ok := l.Addons[a.Key]
				if !ok || isPinned(a) {
					continue
				}
				if a.Source != "github" {
					failed = append(failed, addonError(a.Key, errs.New(errs.Usage, fmt.Sprintf("source %q is not supported; only github", a.Source), "wam outdated --help")))
					continue
				}
				rel, err := p.gh.Resolve(cmd.Context(), a.ID, source.Spec{Version: a.Version, Asset: a.Asset, Flavor: p.m.Game.Flavor})
				if err != nil {
					failed = append(failed, addonError(a.Key, err))
					continue
				}
				if rel.Tag != e.Tag {
					out = append(out, outdatedAddon{Addon: a.Key, Locked: e.Tag, Latest: rel.Tag})
				}
			}
			if eerr := s.Emit(out, func(w io.Writer) error {
				if len(out) == 0 && s.env.StdoutTTY {
					_, err := fmt.Fprintln(w, "all addons are up to date")
					return err
				}
				for _, o := range out {
					if _, err := fmt.Fprintf(w, "%s %s -> %s\n", o.Addon, o.Locked, o.Latest); err != nil {
						return err
					}
				}
				return nil
			}); eerr != nil {
				return eerr
			}
			return failure(failed, len(out) > 0)
		},
	}
}
