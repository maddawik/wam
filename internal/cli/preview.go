package cli

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/maddawik/wam/internal/errs"
	"github.com/maddawik/wam/internal/install"
	"github.com/maddawik/wam/internal/lock"
	"github.com/maddawik/wam/internal/manifest"
	"github.com/maddawik/wam/internal/source"
	"github.com/spf13/cobra"
)

// previewHelp is added to the long help of commands that resolve releases.
const previewHelp = "\n\n--preview (-n) shows what the command would change and changes nothing:\n" +
	"not the manifest, the lock or the AddOns folder. It still uses the network\n" +
	"to resolve releases and download them to a temp folder to list their\n" +
	"folders. With --json it prints the plan as {\"preview\": true, \"changes\": [...]}."

func bindPreview(c *cobra.Command) {
	c.Flags().BoolP("preview", "n", false, "show what would change, without changing anything")
}

func wantsPreview(cmd *cobra.Command) bool {
	v, _ := cmd.Flags().GetBool("preview")
	return v
}

// planChange is one addon a mutating command would change.
type planChange struct {
	// Action is "add", "update", "remove" or "restore" (put back a locked
	// addon whose folders are missing).
	Action string `json:"action"`
	Addon  string `json:"addon"`
	// Version is the manifest version, set by add.
	Version string `json:"version,omitempty"`
	// From and To are the locked and resulting release tags.
	From    string   `json:"from,omitempty"`
	To      string   `json:"to,omitempty"`
	Adds    []string `json:"adds"`
	Removes []string `json:"removes"`
}

// plan is what --preview reports.
type plan struct {
	Preview bool         `json:"preview"`
	Changes []planChange `json:"changes"`
}

func emitPlan(s *IO, changes []planChange) error {
	if changes == nil {
		changes = []planChange{}
	}
	return s.Emit(plan{Preview: true, Changes: changes}, func(w io.Writer) error {
		for _, c := range changes {
			var head string
			switch c.Action {
			case "update":
				head = fmt.Sprintf("~ %s %s -> %s", c.Addon, c.From, c.To)
			case "remove":
				head = "- " + c.Addon
			default:
				head = fmt.Sprintf("+ %s %s", c.Addon, c.To)
			}
			if _, err := fmt.Fprintln(w, head); err != nil {
				return err
			}
			for _, f := range c.Adds {
				if _, err := fmt.Fprintf(w, "    + %s\n", f); err != nil {
					return err
				}
			}
			for _, f := range c.Removes {
				if _, err := fmt.Fprintf(w, "    - %s\n", f); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// previewAddon resolves a and downloads it to a temp folder to learn which
// folders it would install, without touching addonsDir. It returns the
// release tag and the folders.
func previewAddon(ctx context.Context, src source.Source, flavor string, a manifest.Addon, addonsDir string, owned []string, ownerOf install.OwnerFunc) (tag string, folders []string, err error) {
	rel, err := src.Resolve(ctx, a.ID, source.Spec{Version: a.Version, Asset: a.Asset, Flavor: flavor})
	if err != nil {
		return "", nil, err
	}
	zipPath, _, cleanup, err := fetch(ctx, src, rel.URL, os.TempDir())
	if err != nil {
		return "", nil, err
	}
	defer cleanup()
	folders, err = install.Plan(zipPath, addonsDir, owned, ownerOf)
	return rel.Tag, folders, err
}

// without returns the entries of a that b doesn't have, compared
// case-insensitively.
func without(a, b []string) []string {
	skip := map[string]bool{}
	for _, f := range b {
		skip[strings.ToLower(f)] = true
	}
	out := []string{}
	for _, f := range a {
		if !skip[strings.ToLower(f)] {
			out = append(out, f)
		}
	}
	return out
}

// planSync is what runSync would do, without doing it. A failing addon is
// skipped and the rest carry on, as in runSync.
func planSync(ctx context.Context, src source.Source, m *manifest.Manifest, manifestPath, addonsDir string) ([]planChange, error) {
	l, err := lock.Load(lockPath(manifestPath))
	if err != nil {
		return nil, err
	}
	changes := []planChange{}
	var failed []*errs.Error

	wanted := map[string]bool{}
	for _, a := range m.Addons {
		wanted[a.Key] = true
	}
	for _, key := range slices.Sorted(maps.Keys(l.Addons)) {
		if wanted[key] {
			continue
		}
		e := l.Addons[key]
		changes = append(changes, planChange{Action: "remove", Addon: key, From: e.Tag, Adds: []string{}, Removes: e.Folders})
	}
	for _, a := range m.Addons {
		if err := ctx.Err(); err != nil {
			return changes, err
		}
		if a.Source != "github" {
			failed = append(failed, addonError(a.Key, errs.New(errs.Usage, fmt.Sprintf("source %q is not supported; only github", a.Source), "wam sync --help")))
			continue
		}
		var locked *lock.Entry
		if e, ok := l.Addons[a.Key]; ok {
			locked = &e
		}
		switch {
		case locked == nil || lockedVersion(*locked) != a.Version || !assetMatches(a.Asset, locked.Asset):
			var owned []string
			c := planChange{Action: "add", Addon: a.Key}
			if locked != nil {
				owned = locked.Folders
				c.Action, c.From = "update", locked.Tag
			}
			ownerOf := func(folder string) (string, bool) { return l.Owner(folder, a.Key) }
			tag, folders, err := previewAddon(ctx, src, m.Game.Flavor, a, addonsDir, owned, ownerOf)
			if err != nil {
				failed = append(failed, addonError(a.Key, err))
				continue
			}
			c.To, c.Adds, c.Removes = tag, without(folders, owned), without(owned, folders)
			changes = append(changes, c)
		case !installed(addonsDir, locked.Folders):
			changes = append(changes, planChange{Action: "restore", Addon: a.Key, To: locked.Tag, Adds: locked.Folders, Removes: []string{}})
		}
	}
	return changes, failure(failed, len(changes) > 0)
}
