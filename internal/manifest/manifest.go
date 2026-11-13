// Package manifest loads and saves wam.toml, the list of addons a player
// wants installed.
package manifest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
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
	// Asset is an optional glob that picks the release zip by name.
	Asset string
}

type file struct {
	Game struct {
		Flavor string `toml:"flavor"`
		Dir    string `toml:"dir,omitempty"`
	} `toml:"game"`
	Addons map[string]any `toml:"addons"`
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
	for key, raw := range f.Addons {
		source, id, ok := strings.Cut(key, ":")
		if !ok || source == "" || id == "" {
			return nil, errs.New(errs.Usage, fmt.Sprintf("addon %q must look like source:identifier", key), "wam sync --help")
		}
		version, asset, err := parseEntry(key, raw)
		if err != nil {
			return nil, err
		}
		m.Addons = append(m.Addons, Addon{Key: key, Source: source, ID: id, Version: version, Asset: asset})
	}
	slices.SortFunc(m.Addons, func(a, b Addon) int { return strings.Compare(a.Key, b.Key) })
	return m, nil
}

// parseEntry reads one addon value: a version string, or an inline table with
// a version and an optional asset glob.
func parseEntry(key string, raw any) (version, asset string, err error) {
	bad := func(msg string) error {
		return errs.New(errs.Usage, fmt.Sprintf("addon %q %s", key, msg), "wam sync --help")
	}
	switch v := raw.(type) {
	case string:
		version = v
	case map[string]any:
		for k := range v {
			if k != "version" && k != "asset" {
				return "", "", bad(fmt.Sprintf("has unknown option %q (use version and asset)", k))
			}
		}
		var ok bool
		if version, ok = v["version"].(string); !ok && v["version"] != nil {
			return "", "", bad("needs version to be a string")
		}
		if asset, ok = v["asset"].(string); !ok && v["asset"] != nil {
			return "", "", bad("needs asset to be a string")
		}
		if _, err := path.Match(asset, ""); err != nil {
			return "", "", bad(fmt.Sprintf("has an invalid asset glob %q", asset))
		}
	default:
		return "", "", bad("must be a version string or a { version, asset } table")
	}
	if version == "" {
		return "", "", bad("needs a version (\"latest\" or a tag)")
	}
	return version, asset, nil
}

// Save writes the manifest to path atomically.
func Save(path string, m *Manifest) error {
	var f file
	f.Game.Flavor = m.Game.Flavor
	f.Game.Dir = m.Game.Dir
	f.Addons = map[string]any{}
	for _, a := range m.Addons {
		if a.Asset == "" {
			f.Addons[a.Key] = a.Version
		} else {
			f.Addons[a.Key] = map[string]string{"version": a.Version, "asset": a.Asset}
		}
	}
	b, err := toml.Marshal(f)
	if err != nil {
		return err
	}
	return atomicfile.Write(path, b)
}
