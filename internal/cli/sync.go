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

// summary is what happened, without the addon: "installed v2" or
// "updated v1 -> v2".
func (c syncChange) summary() string {
	if c.From != "" && c.From != c.Tag {
		return fmt.Sprintf("%s %s -> %s", c.Action, c.From, c.Tag)
	}
	return c.Action + " " + c.Tag
}

// line is c's stdout line from sync and update: "installed <addon> v2" or
// "updated <addon> v1 -> v2".
func (c syncChange) line() string {
	if c.From != "" && c.From != c.Tag {
		return fmt.Sprintf("%s %s %s -> %s", c.Action, c.Addon, c.From, c.Tag)
	}
	return fmt.Sprintf("%s %s %s", c.Action, c.Addon, c.Tag)
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
			"download is checked against the lock's SHA-256. Up to 4 addons\n" +
			"download at once; each is reported as it finishes." + previewHelp,
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
			changes, err := runSync(cmd.Context(), s, gh, m, file, dir)
			return emitChanges(s, changes, err)
		},
	}
	bindPreview(c)
	return c
}

// emitChanges finishes a command whose text output went out as each addon
// finished: under --json it writes every change now. They are reported even
// when a later addon failed.
func emitChanges(s *IO, changes []syncChange, err error) error {
	if s.JSON() && (len(changes) > 0 || err == nil) {
		if jerr := writeJSON(s.Out, changes); err == nil {
			err = jerr
		}
	}
	return err
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
// every manifest addon the lock doesn't have yet, as addonRun.downloadAll
// does. One failure is returned as is, several as one runtime error naming
// each.
func runSync(ctx context.Context, s *IO, src source.Source, m *manifest.Manifest, manifestPath, addonsDir string) ([]syncChange, error) {
	r, err := startRun(ctx, manifestPath, addonsDir)
	if err != nil {
		return nil, err
	}

	wanted := map[string]bool{}
	for _, a := range m.Addons {
		wanted[a.Key] = true
	}
	var dropped []string
	for _, key := range slices.Sorted(maps.Keys(r.l.Addons)) {
		if !wanted[key] {
			dropped = append(dropped, key)
		}
	}
	var jobs []downloadJob
	for _, a := range m.Addons {
		if j, ok := syncJob(r, src, m.Game.Flavor, a); ok {
			jobs = append(jobs, j)
		}
	}
	r.prog = newProgress(s, len(dropped)+len(jobs))

	return r.finish(func() error {
		for _, key := range dropped {
			if err := ctx.Err(); err != nil {
				return err
			}
			e := r.l.Addons[key]
			if err := install.Remove(addonsDir, e.Folders); err != nil {
				r.failed(key, err)
				continue
			}
			delete(r.l.Addons, key)
			if err := r.changed(syncChange{Action: "removed", Addon: key, Tag: e.Tag, Folders: e.Folders}); err != nil {
				return err
			}
		}
		return r.downloadAll(jobs)
	})
}

// syncJob is the download that brings a in line with the manifest, if it
// needs one.
func syncJob(r *addonRun, src source.Source, flavor string, a manifest.Addon) (downloadJob, bool) {
	if a.Source != "github" {
		return unsupportedJob(a, "wam sync --help"), true
	}
	j := downloadJob{key: a.Key}
	locked, ok := r.l.Addons[a.Key]
	if ok && lockedVersion(locked) == a.Version && assetMatches(a.Asset, locked.Asset) {
		if installed(r.addonsDir, locked.Folders) {
			return j, false
		}
		// Install exactly what the lock records.
		j.fetch = func(ctx context.Context) (download, error) {
			return fetchLocked(ctx, src, a.Key, locked, r.addonsDir)
		}
		j.install = func(d download) (*syncChange, error) {
			if _, err := install.Zip(r.ctx, d.zip, r.addonsDir, locked.Folders, r.ownerOf(a.Key)); err != nil {
				return nil, err
			}
			return &syncChange{Action: "installed", Addon: a.Key, Tag: locked.Tag, Folders: locked.Folders}, nil
		}
		return j, true
	}
	// New to the lock, or its pin changed: resolve it again, replacing the
	// folders the old version put there.
	var owned []string
	if ok {
		owned = locked.Folders
	}
	j.fetch = func(ctx context.Context) (download, error) {
		return fetchRelease(ctx, src, flavor, a, r.addonsDir)
	}
	j.install = func(d download) (*syncChange, error) {
		entry, err := installRelease(r.ctx, a, d, r.addonsDir, owned, r.ownerOf(a.Key))
		if err != nil {
			return nil, err
		}
		r.l.Addons[a.Key] = entry
		return &syncChange{Action: "installed", Addon: a.Key, Tag: entry.Tag, Folders: entry.Folders}, nil
	}
	return j, true
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

// download is a release fetched to a temp file in the AddOns folder. Call
// cleanup to delete the file.
type download struct {
	rel     source.Release
	zip     string
	sum     string
	cleanup func()
}

// fetchRelease resolves a and downloads the release it names.
func fetchRelease(ctx context.Context, src source.Source, flavor string, a manifest.Addon, addonsDir string) (download, error) {
	rel, err := src.Resolve(ctx, a.ID, source.Spec{Version: a.Version, Asset: a.Asset, Flavor: flavor})
	if err != nil {
		return download{}, err
	}
	zipPath, sum, cleanup, err := fetch(ctx, src, rel.URL, addonsDir)
	if err != nil {
		return download{}, err
	}
	return download{rel: rel, zip: zipPath, sum: sum, cleanup: cleanup}, nil
}

// fetchLocked downloads the release the lock records, without asking the
// host what is newest. The download must match the lock's hash.
func fetchLocked(ctx context.Context, src source.Source, key string, e lock.Entry, addonsDir string) (download, error) {
	zipPath, sum, cleanup, err := fetch(ctx, src, e.URL, addonsDir)
	if err != nil {
		return download{}, err
	}
	if sum != e.SHA256 {
		cleanup()
		return download{}, errs.New(errs.Conflict,
			fmt.Sprintf("%s does not match the lock's SHA-256; the asset changed since it was locked", e.Tag),
			"wam update "+key)
	}
	return download{rel: source.Release{Tag: e.Tag, Asset: e.Asset, URL: e.URL}, zip: zipPath, sum: sum, cleanup: cleanup}, nil
}

// installRelease installs d over any owned folders and returns a's lock
// entry for it.
func installRelease(ctx context.Context, a manifest.Addon, d download, addonsDir string, owned []string, ownerOf install.OwnerFunc) (lock.Entry, error) {
	folders, err := install.Zip(ctx, d.zip, addonsDir, owned, ownerOf)
	if err != nil {
		return lock.Entry{}, err
	}
	return newLockEntry(a, d.rel, d.sum, addonsDir, folders)
}

// installAddon resolves a, installs it over any owned folders, and returns
// its lock entry.
func installAddon(ctx context.Context, src source.Source, flavor string, a manifest.Addon, addonsDir string, owned []string, ownerOf install.OwnerFunc) (lock.Entry, error) {
	d, err := fetchRelease(ctx, src, flavor, a, addonsDir)
	if err != nil {
		return lock.Entry{}, err
	}
	defer d.cleanup()
	return installRelease(ctx, a, d, addonsDir, owned, ownerOf)
}

// newLockEntry is the lock entry for a installed from rel, whose download
// hashed to sum, as the folders now in addonsDir.
func newLockEntry(a manifest.Addon, rel source.Release, sum, addonsDir string, folders []string) (lock.Entry, error) {
	tree, err := install.Digest(addonsDir, folders)
	if err != nil {
		return lock.Entry{}, err
	}
	return lock.Entry{
		Source: a.Source, Repo: a.ID, Version: a.Version, Tag: rel.Tag, Asset: rel.Asset, URL: rel.URL,
		SHA256: sum, Folders: folders, Tree: tree,
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
