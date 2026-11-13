package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/maddawik/wam/internal/errs"
	"github.com/maddawik/wam/internal/install"
	"github.com/maddawik/wam/internal/locate"
	"github.com/maddawik/wam/internal/lock"
	"github.com/maddawik/wam/internal/manifest"
	"github.com/maddawik/wam/internal/source"
	"github.com/spf13/cobra"
)

// bindPathFlags adds the flags that locate the manifest and the AddOns folder.
func bindPathFlags(root *cobra.Command) {
	f := root.PersistentFlags()
	f.StringP("file", "f", "", "path to the manifest (default $WAM_FILE, else wam.toml in the config directory)")
	f.String("dir", "", "path to the game's AddOns folder (default $WAM_DIR, the manifest's dir, then auto-detect)")
}

// syncChange is one addon installed, added or removed.
type syncChange struct {
	// Action is "installed", "added", "updated" or "removed".
	Action string `json:"action"`
	Addon  string `json:"addon"`
	// Version is the manifest version, set by add.
	Version string `json:"version,omitempty"`
	// From is the tag an update replaced.
	From    string   `json:"from,omitempty"`
	Tag     string   `json:"tag"`
	Folders []string `json:"folders"`
}

func newSync() *cobra.Command {
	c := &cobra.Command{
		Use:   "sync",
		Short: "Install the manifest's addons into the AddOns folder",
		Long: "Install the manifest's addons into the AddOns folder and record what\n" +
			"was installed in the lock, which sits next to the manifest. A version\n" +
			"is \"latest\" or an exact release tag, matched as written. Addons the\n" +
			"lock already has are installed as locked and never moved to a newer\n" +
			"release; only new entries or changed versions are resolved. Every\n" +
			"download is checked against the lock's SHA-256." + previewHelp,
		Example: "  wam sync\n  wam sync -f wam.toml --dir \"/Applications/World of Warcraft/_retail_/Interface/AddOns\"\n" +
			"  wam sync -f wam.toml --dir ./AddOns --json",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := From(cmd)
			p, err := openProject(cmd)
			if err != nil {
				return err
			}
			dir, err := p.dir()
			if err != nil {
				return err
			}
			file, m, gh := p.file, p.m, p.gh
			if wantsPreview(cmd) {
				changes, err := planSync(cmd.Context(), gh, m, file, dir)
				if eerr := emitPlan(s, changes); err == nil {
					err = eerr
				}
				return err
			}
			changes, err := runSync(cmd.Context(), gh, m, file, dir)
			// Report what was installed even when a later addon failed.
			if len(changes) > 0 || err == nil {
				if eerr := s.Emit(changes, func(w io.Writer) error {
					for _, c := range changes {
						if _, err := fmt.Fprintf(w, "%s %s %s\n", c.Action, c.Addon, c.Tag); err != nil {
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
	bindPreview(c)
	return c
}

// manifestPath is the manifest to use: -f, then WAM_FILE, then wam.toml in
// the config directory.
func manifestPath(cmd *cobra.Command, e Env) (string, error) {
	if f, _ := cmd.Flags().GetString("file"); f != "" {
		return f, nil
	}
	if f := e.Vars["WAM_FILE"]; f != "" {
		return f, nil
	}
	dir, err := locate.ConfigDir(e.GOOS, e.Home, e.Vars)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "wam.toml"), nil
}

// lockPath is the lock beside the manifest: wam.toml gets wam.lock.
func lockPath(manifestPath string) string {
	return strings.TrimSuffix(manifestPath, filepath.Ext(manifestPath)) + ".lock"
}

// runSync removes the folders of addons the manifest dropped, then installs
// every manifest addon the lock doesn't have yet. An addon that fails is
// skipped and the rest carry on. The lock is saved after each change, so a
// run killed part way still records what it put in the AddOns folder.
// One failure is returned as is, several as one runtime error naming each.
func runSync(ctx context.Context, src source.Source, m *manifest.Manifest, manifestPath, addonsDir string) (changes []syncChange, err error) {
	lockFile := lockPath(manifestPath)
	l, err := lock.Load(lockFile)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(addonsDir, 0o755); err != nil {
		return nil, errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot create %s", addonsDir), "")
	}
	// Undo whatever a crashed run left half done before changing anything.
	if err := install.Recover(addonsDir); err != nil {
		return nil, err
	}
	changes = []syncChange{}
	var failed []*errs.Error
	defer func() {
		if err == nil {
			err = failure(failed, len(changes) > 0)
		}
	}()
	record := func(c syncChange) error {
		changes = append(changes, c)
		if err := lock.Save(lockFile, l); err != nil {
			return errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot write lock %s", lockFile), "")
		}
		return nil
	}

	wanted := map[string]bool{}
	for _, a := range m.Addons {
		wanted[a.Key] = true
	}
	for _, key := range slices.Sorted(maps.Keys(l.Addons)) {
		if wanted[key] {
			continue
		}
		e := l.Addons[key]
		if ctx.Err() != nil {
			return changes, ctx.Err()
		}
		if err := install.Remove(addonsDir, e.Folders); err != nil {
			failed = append(failed, addonError(key, err))
			continue
		}
		delete(l.Addons, key)
		if err := record(syncChange{Action: "removed", Addon: key, Tag: e.Tag, Folders: e.Folders}); err != nil {
			return changes, err
		}
	}

	for _, a := range m.Addons {
		if ctx.Err() != nil {
			return changes, ctx.Err()
		}
		if a.Source != "github" {
			failed = append(failed, addonError(a.Key, errs.New(errs.Usage, fmt.Sprintf("source %q is not supported; only github", a.Source), "wam sync --help")))
			continue
		}
		var locked *lock.Entry
		if e, ok := l.Addons[a.Key]; ok {
			locked = &e
		}
		ownerOf := func(folder string) (string, bool) { return l.Owner(folder, a.Key) }
		entry, changed, err := syncAddon(ctx, src, m.Game.Flavor, a, locked, addonsDir, ownerOf)
		if err != nil {
			failed = append(failed, addonError(a.Key, err))
			continue
		}
		if !changed {
			continue
		}
		l.Addons[a.Key] = entry
		if err := record(syncChange{Action: "installed", Addon: a.Key, Tag: entry.Tag, Folders: entry.Folders}); err != nil {
			return changes, err
		}
	}
	return changes, nil
}

// syncAddon brings one addon in line with the manifest. locked is its lock
// entry, or nil. changed is false when there was nothing to do.
func syncAddon(ctx context.Context, src source.Source, flavor string, a manifest.Addon, locked *lock.Entry, addonsDir string, ownerOf install.OwnerFunc) (entry lock.Entry, changed bool, err error) {
	if locked == nil || lockedVersion(*locked) != a.Version || !assetMatches(a.Asset, locked.Asset) {
		// New to the lock, or its pin changed: resolve it again, replacing
		// the folders the old version put there.
		var owned []string
		if locked != nil {
			owned = locked.Folders
		}
		entry, err = installAddon(ctx, src, flavor, a, addonsDir, owned, ownerOf)
		return entry, err == nil, err
	}
	// Install exactly what the lock records, if it isn't there yet.
	if installed(addonsDir, locked.Folders) {
		return *locked, false, nil
	}
	err = restoreAddon(ctx, src, a.Key, *locked, addonsDir, ownerOf)
	return *locked, err == nil, err
}

// assetMatches reports whether the locked asset still satisfies the
// manifest's glob. No glob matches anything.
func assetMatches(glob, asset string) bool {
	if glob == "" {
		return true
	}
	ok, _ := path.Match(glob, asset)
	return ok
}

// failure is the error for a run in which failed went wrong: nil for none,
// the failure itself when it is the run's only outcome, and otherwise a
// partial-failure runtime error summarising each one. changed reports whether
// anything else was installed or removed.
func failure(failed []*errs.Error, changed bool) error {
	switch {
	case len(failed) == 0:
		return nil
	case len(failed) == 1 && !changed:
		return failed[0]
	}
	msgs := make([]string, len(failed))
	for i, f := range failed {
		msgs[i] = f.Message
	}
	return errs.New(errs.Runtime, fmt.Sprintf("sync partly failed, %d addon(s) not synced: %s", len(failed), strings.Join(msgs, "; ")), "wam sync")
}

// addonError names the addon key in err's message.
func addonError(key string, err error) *errs.Error {
	e, ok := errs.As(err)
	if !ok {
		return errs.Wrap(errs.Runtime, err, key+": "+err.Error(), "")
	}
	if strings.HasPrefix(e.Message, key+" ") {
		return e
	}
	out := *e
	out.Message = key + ": " + e.Message
	out.Err = err
	return &out
}

// lockedVersion is the manifest version e was resolved for. Locks written
// before pins existed have none and were always latest.
func lockedVersion(e lock.Entry) string {
	if e.Version == "" {
		return source.Latest
	}
	return e.Version
}

// installed reports whether every one of folders is in addonsDir.
func installed(addonsDir string, folders []string) bool {
	if len(folders) == 0 {
		return false
	}
	for _, f := range folders {
		if _, err := os.Lstat(filepath.Join(addonsDir, f)); err != nil {
			return false
		}
	}
	return true
}

// restoreAddon installs the release the lock records, without asking the host
// what is newest. The download must match the lock's hash.
func restoreAddon(ctx context.Context, src source.Source, key string, e lock.Entry, addonsDir string, ownerOf install.OwnerFunc) error {
	zipPath, sum, cleanup, err := fetch(ctx, src, e.URL, addonsDir)
	if err != nil {
		return err
	}
	defer cleanup()
	if sum != e.SHA256 {
		return errs.New(errs.Conflict,
			fmt.Sprintf("%s does not match the lock's SHA-256; the asset changed since it was locked", e.Tag),
			"wam update "+key)
	}
	_, err = install.Zip(ctx, zipPath, addonsDir, e.Folders, ownerOf)
	return err
}

// installAddon resolves a, installs it over any owned folders, and returns
// its lock entry.
func installAddon(ctx context.Context, src source.Source, flavor string, a manifest.Addon, addonsDir string, owned []string, ownerOf install.OwnerFunc) (lock.Entry, error) {
	rel, err := src.Resolve(ctx, a.ID, source.Spec{Version: a.Version, Asset: a.Asset, Flavor: flavor})
	if err != nil {
		return lock.Entry{}, err
	}
	zipPath, sum, cleanup, err := fetch(ctx, src, rel.URL, addonsDir)
	if err != nil {
		return lock.Entry{}, err
	}
	defer cleanup()
	folders, err := install.Zip(ctx, zipPath, addonsDir, owned, ownerOf)
	if err != nil {
		return lock.Entry{}, err
	}
	return lock.Entry{
		Source: a.Source, Repo: a.ID, Version: a.Version, Tag: rel.Tag, Asset: rel.Asset, URL: rel.URL,
		SHA256: sum, Folders: folders,
	}, nil
}

// fetch downloads url to a temp file in addonsDir, hashing it on the way.
// Call cleanup to delete the file; install.Recover deletes it after a crash.
func fetch(ctx context.Context, src source.Source, url, addonsDir string) (path, sum string, cleanup func(), err error) {
	tmp, err := os.CreateTemp(addonsDir, install.TempPattern+".zip")
	if err != nil {
		return "", "", nil, errs.Wrap(errs.Runtime, err, "cannot create a temp file", "")
	}
	cleanup = func() { _ = os.Remove(tmp.Name()) }
	h := sha256.New()
	err = src.Download(ctx, url, io.MultiWriter(tmp, h))
	if cerr := tmp.Close(); err == nil && cerr != nil {
		err = errs.Wrap(errs.Runtime, cerr, "cannot write the download", "")
	}
	if err != nil {
		cleanup()
		return "", "", nil, err
	}
	return tmp.Name(), hex.EncodeToString(h.Sum(nil)), cleanup, nil
}
