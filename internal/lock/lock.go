// Package lock loads and saves wam.lock, the record of what wam resolved and
// installed.
package lock

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strings"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/maddawik/wam/internal/atomicfile"
	"github.com/maddawik/wam/internal/errs"
)

// Lock maps manifest keys to what was installed for them.
type Lock struct {
	Addons map[string]Entry `toml:"addons"`
}

// Entry is one locked addon.
type Entry struct {
	Source string `toml:"source"`
	// Version is the manifest version this entry was resolved for. Empty
	// means source.Latest.
	Version string `toml:"version"`
	Repo    string `toml:"repo"`
	Tag     string `toml:"tag"`
	Asset   string `toml:"asset"`
	URL     string `toml:"url"`
	SHA256  string `toml:"sha256"`
	// Folders are the top-level folders the addon owns, in the zip's casing.
	Folders []string `toml:"folders"`
	// Tree is the install.Digest of Folders as installed. Empty in locks
	// written before it was recorded.
	Tree string `toml:"tree"`
}

// Owner returns the key of the addon, other than except, that owns folder.
// Folder names are compared case-insensitively. ok is false when no other
// addon owns it.
func (l *Lock) Owner(folder, except string) (key string, ok bool) {
	keys := slices.Sorted(maps.Keys(l.Addons))
	for _, k := range keys {
		if k == except {
			continue
		}
		for _, f := range l.Addons[k].Folders {
			if strings.EqualFold(f, folder) {
				return k, true
			}
		}
	}
	return "", false
}

// Load reads the lock at path. A missing file is an empty lock.
func Load(path string) (*Lock, error) {
	l := &Lock{Addons: map[string]Entry{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot read lock %s", path), "")
	}
	if err := toml.Unmarshal(b, l); err != nil {
		return nil, errs.Wrap(errs.Runtime, err, fmt.Sprintf("lock %s is not valid TOML", path), "")
	}
	if l.Addons == nil {
		l.Addons = map[string]Entry{}
	}
	return l, nil
}

// Save writes the lock to path atomically.
func Save(path string, l *Lock) error {
	b, err := toml.Marshal(l)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, b)
}
