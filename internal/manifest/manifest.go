// Package manifest loads and saves wam.toml, the list of addons a player
// wants installed.
package manifest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/maddawik/wam/internal/atomicfile"
	"github.com/maddawik/wam/internal/errs"
)

// Manifest is the parsed wam.toml.
type Manifest struct {
	Game   Game
	Addons []Addon // sorted by Key
}

// Game describes the game install the addons go to.
type Game struct {
	Flavor string
	Dir    string
}

// Addon is one manifest entry.
type Addon struct {
	// Key is the manifest key, for example "github:owner/repo".
	Key string
	// Source is the host prefix of Key, for example "github".
	Source string
	// ID is what follows the prefix, for example "owner/repo".
	ID string
	// Version is "latest" or an exact release tag.
	Version string
}

type file struct {
	Game struct {
		Flavor string `toml:"flavor"`
		Dir    string `toml:"dir,omitempty"`
	} `toml:"game"`
	Addons map[string]string `toml:"addons"`
}

// Load reads and validates the manifest at path.
func Load(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errs.Wrap(errs.NotFound, err, fmt.Sprintf("manifest %s does not exist", path), "wam sync --help")
	}
	if err != nil {
		return nil, errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot read manifest %s", path), "")
	}
	var f file
	if err := toml.Unmarshal(b, &f); err != nil {
		return nil, errs.Wrap(errs.Usage, err, fmt.Sprintf("manifest %s is not valid TOML", path), "")
	}
	if f.Game.Flavor == "" {
		return nil, errs.New(errs.Usage, fmt.Sprintf("manifest %s needs game.flavor", path), "wam sync --help")
	}
	m := &Manifest{Game: Game{Flavor: f.Game.Flavor, Dir: f.Game.Dir}}
	for key, version := range f.Addons {
		source, id, ok := strings.Cut(key, ":")
		if !ok || source == "" || id == "" {
			return nil, errs.New(errs.Usage, fmt.Sprintf("addon %q must look like source:identifier", key), "wam sync --help")
		}
		if version == "" {
			return nil, errs.New(errs.Usage, fmt.Sprintf("addon %q needs a version (\"latest\" or a tag)", key), "wam sync --help")
		}
		m.Addons = append(m.Addons, Addon{Key: key, Source: source, ID: id, Version: version})
	}
	slices.SortFunc(m.Addons, func(a, b Addon) int { return strings.Compare(a.Key, b.Key) })
	return m, nil
}

// Save writes the manifest to path atomically.
func Save(path string, m *Manifest) error {
	var f file
	f.Game.Flavor = m.Game.Flavor
	f.Game.Dir = m.Game.Dir
	f.Addons = map[string]string{}
	for _, a := range m.Addons {
		f.Addons[a.Key] = a.Version
	}
	b, err := toml.Marshal(f)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, b)
}
