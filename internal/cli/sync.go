package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/maddawik/wam/internal/errs"
	"github.com/maddawik/wam/internal/install"
	"github.com/maddawik/wam/internal/lock"
	"github.com/maddawik/wam/internal/manifest"
	"github.com/maddawik/wam/internal/source"
	"github.com/spf13/cobra"
)

// bindPathFlags adds the flags that locate the manifest and the AddOns folder.
func bindPathFlags(root *cobra.Command) {
	f := root.PersistentFlags()
	f.StringP("file", "f", "", "path to the manifest (wam.toml)")
	f.String("dir", "", "path to the game's AddOns folder")
}

// syncChange is one addon sync installed.
type syncChange struct {
	Addon   string   `json:"addon"`
	Tag     string   `json:"tag"`
	Folders []string `json:"folders"`
}

func newSync() *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Install the manifest's addons into the AddOns folder",
		Long: "Install the manifest's addons into the AddOns folder and record what\n" +
			"was installed in the lock, which sits next to the manifest. Addons\n" +
			"already in the lock are left alone.",
		Example: "  wam sync -f wam.toml --dir \"/Applications/World of Warcraft/_retail_/Interface/AddOns\"\n" +
			"  wam sync -f wam.toml --dir ./AddOns --json",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := From(cmd)
			file, _ := cmd.Flags().GetString("file")
			dir, _ := cmd.Flags().GetString("dir")
			if file == "" {
				return errs.New(errs.Usage, "no manifest: pass -f <path>", "wam sync -f wam.toml --dir <AddOns>")
			}
			if dir == "" {
				return errs.New(errs.Usage, "no AddOns folder: pass --dir <path>", "wam sync -f wam.toml --dir <AddOns>")
			}
			gh := &source.GitHub{BaseURL: s.env.Vars["WAM_GITHUB_API"], Token: s.env.Vars["GITHUB_TOKEN"]}
			changes, err := runSync(cmd.Context(), s, gh, file, dir)
			// Report what was installed even when a later addon failed.
			if len(changes) > 0 || err == nil {
				if eerr := s.Emit(changes, func(w io.Writer) error {
					for _, c := range changes {
						if _, err := fmt.Fprintf(w, "installed %s %s\n", c.Addon, c.Tag); err != nil {
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

// lockPath is the lock beside the manifest: wam.toml gets wam.lock.
func lockPath(manifestPath string) string {
	return strings.TrimSuffix(manifestPath, filepath.Ext(manifestPath)) + ".lock"
}

// runSync installs every manifest addon the lock doesn't have yet. It stops
// at the first failure but still saves the lock for addons already installed.
func runSync(ctx context.Context, s *IO, src source.Source, manifestPath, addonsDir string) (changes []syncChange, err error) {
	m, err := manifest.Load(manifestPath)
	if err != nil {
		return nil, err
	}
	lockFile := lockPath(manifestPath)
	l, err := lock.Load(lockFile)
	if err != nil {
		return nil, err
	}
	changes = []syncChange{}
	defer func() {
		if len(changes) == 0 {
			return
		}
		if serr := lock.Save(lockFile, l); serr != nil && err == nil {
			err = errs.Wrap(errs.Runtime, serr, fmt.Sprintf("cannot write lock %s", lockFile), "")
		}
	}()

	for _, a := range m.Addons {
		if _, ok := l.Addons[a.Key]; ok {
			continue
		}
		if a.Source != "github" {
			return changes, errs.New(errs.Usage, fmt.Sprintf("source %q is not supported; only github", a.Source), "wam sync --help")
		}
		entry, err := installAddon(ctx, src, a, addonsDir)
		if err != nil {
			return changes, err
		}
		l.Addons[a.Key] = entry
		changes = append(changes, syncChange{Addon: a.Key, Tag: entry.Tag, Folders: entry.Folders})
		s.Infof("installed %s %s", a.Key, entry.Tag)
	}
	return changes, nil
}

func installAddon(ctx context.Context, src source.Source, a manifest.Addon, addonsDir string) (lock.Entry, error) {
	rel, err := src.Resolve(ctx, a.ID, a.Version)
	if err != nil {
		return lock.Entry{}, err
	}
	sum, folders, err := downloadAndInstall(ctx, src, rel, addonsDir)
	if err != nil {
		return lock.Entry{}, err
	}
	return lock.Entry{
		Source: a.Source, Repo: a.ID, Tag: rel.Tag, Asset: rel.Asset, URL: rel.URL, SHA256: sum, Folders: folders,
	}, nil
}

// downloadAndInstall fetches rel's zip to a temp file, hashing it on the way,
// and installs it.
func downloadAndInstall(ctx context.Context, src source.Source, rel source.Release, addonsDir string) (sum string, folders []string, err error) {
	tmp, err := os.CreateTemp("", "wam-*.zip")
	if err != nil {
		return "", nil, errs.Wrap(errs.Runtime, err, "cannot create a temp file", "")
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	h := sha256.New()
	if err := src.Download(ctx, rel.URL, io.MultiWriter(tmp, h)); err != nil {
		return "", nil, err
	}
	if err := tmp.Close(); err != nil {
		return "", nil, errs.Wrap(errs.Runtime, err, "cannot write the download", "")
	}
	folders, err = install.Zip(tmp.Name(), addonsDir)
	if err != nil {
		return "", nil, err
	}
	return hex.EncodeToString(h.Sum(nil)), folders, nil
}
