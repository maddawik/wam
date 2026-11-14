package cli

import (
	"context"
	"fmt"
	"io"
	"slices"

	"github.com/maddawik/wam/internal/errs"
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
			return emitChanges(s, changes, err)
		},
	}
}

// runUpdate re-resolves targets and installs what changed, as
// addonRun.downloadAll does.
func runUpdate(ctx context.Context, s *IO, p *project, targets []manifest.Addon, addonsDir string) ([]syncChange, error) {
	if len(targets) == 0 {
		return []syncChange{}, nil
	}
	r, err := startRun(ctx, p.file, addonsDir)
	if err != nil {
		return nil, err
	}
	jobs := make([]downloadJob, len(targets))
	for i, a := range targets {
		jobs[i] = updateJob(r, p, a)
	}
	r.prog = newProgress(s, len(jobs))
	return r.finish(func() error { return r.downloadAll(jobs) })
}

// updateJob resolves a again, whatever the lock says, and installs the
// result unless it is exactly what is already locked and installed.
func updateJob(r *addonRun, p *project, a manifest.Addon) downloadJob {
	if a.Source != "github" {
		return unsupportedJob(a, "wam update --help")
	}
	j := downloadJob{key: a.Key}
	flavor := p.m.Game.Flavor
	j.fetch = func(ctx context.Context) (download, error) {
		return fetchRelease(ctx, p.gh, flavor, a, r.addonsDir)
	}
	j.install = func(d download) (*syncChange, error) {
		locked, ok := r.l.Addons[a.Key]
		if ok && locked.Tag == d.rel.Tag && locked.SHA256 == d.sum && installed(r.addonsDir, locked.Folders) {
			return nil, nil
		}
		var owned []string
		if ok {
			owned = locked.Folders
		}
		entry, err := installRelease(r.ctx, a, d, r.addonsDir, owned, r.ownerOf(a.Key))
		if err != nil {
			return nil, err
		}
		r.l.Addons[a.Key] = entry
		c := &syncChange{Action: "updated", Addon: a.Key, Tag: entry.Tag, Folders: entry.Folders}
		if ok {
			c.From = locked.Tag
		}
		return c, nil
	}
	if isPinned(a) {
		j.unchanged = func() {
			r.prog.infof("%s is pinned to %s; edit its version in %s to move it", a.Key, a.Version, p.file)
		}
	}
	return j
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
