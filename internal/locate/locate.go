// Package locate finds wam's config directory and the game's AddOns folder.
package locate

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/maddawik/wam/internal/errs"
)

// ConfigDir is where the manifest and lock live by default:
// $XDG_CONFIG_HOME/wam when set, else ~/.config/wam on macOS and Linux and
// %APPDATA%\wam on Windows. It fails when it needs the home directory and
// home is empty.
func ConfigDir(goos, home string, vars map[string]string) (string, error) {
	if x := vars["XDG_CONFIG_HOME"]; x != "" {
		return filepath.Join(x, "wam"), nil
	}
	if goos == "windows" {
		if appdata := vars["APPDATA"]; appdata != "" {
			return filepath.Join(appdata, "wam"), nil
		}
	}
	if home == "" {
		return "", errs.New(errs.Usage, "cannot find the config directory: no home directory", "wam sync -f <path to wam.toml>")
	}
	if goos == "windows" {
		return filepath.Join(home, "AppData", "Roaming", "wam"), nil
	}
	return filepath.Join(home, ".config", "wam"), nil
}

// gameFolders maps release.json flavor names to the game's folder name.
// Every progression flavor shares _classic_ because the client only has one
// progression install at a time.
var gameFolders = map[string]string{
	"mainline": "_retail_",
	"classic":  "_classic_era_",
	"forever":  "_forever_",
	"bcc":      "_classic_",
	"wrath":    "_classic_",
	"cata":     "_classic_",
	"mists":    "_classic_",
	"titan":    "_classic_",
}

// GameFolder is the folder under the install root that holds flavor's game.
func GameFolder(flavor string) (string, error) {
	if f, ok := gameFolders[flavor]; ok {
		return f, nil
	}
	valid := make([]string, 0, len(gameFolders))
	for k := range gameFolders {
		valid = append(valid, k)
	}
	slices.Sort(valid)
	return "", errs.New(errs.Usage,
		fmt.Sprintf("unknown game flavor %q; valid flavors: %s", flavor, strings.Join(valid, ", ")),
		"edit game.flavor in the manifest")
}

// Probe is what auto-detect may look at, so tests can fake an install.
type Probe struct {
	// Roots are candidate install roots, tried in order.
	Roots []string
	// Registry returns install paths recorded by the Battle.net installer,
	// tried after Roots. It may be nil.
	Registry func() []string
}

// DefaultProbe is the probe for goos. vars supplies the Windows
// Program Files locations.
func DefaultProbe(goos string, vars map[string]string) Probe {
	const wow = "World of Warcraft"
	switch goos {
	case "darwin":
		return Probe{Roots: []string{"/Applications/" + wow}}
	case "windows":
		pf86, pf := vars["ProgramFiles(x86)"], vars["ProgramFiles"]
		if pf86 == "" {
			pf86 = `C:\Program Files (x86)`
		}
		if pf == "" {
			pf = `C:\Program Files`
		}
		return Probe{
			Roots:    []string{filepath.Join(pf86, wow), filepath.Join(pf, wow)},
			Registry: registryInstallPaths,
		}
	}
	return Probe{}
}

// Inputs are the sources of the AddOns dir, highest precedence first.
type Inputs struct {
	Flag     string // --dir
	Env      string // WAM_DIR
	Manifest string // the manifest's game.dir
	Flavor   string // the manifest's game.flavor
	Probe    Probe
}

// AddOnsDir resolves the AddOns folder: --dir, then WAM_DIR, then the
// manifest's dir, then auto-detect.
func AddOnsDir(in Inputs) (string, error) {
	for _, d := range []string{in.Flag, in.Env, in.Manifest} {
		if d != "" {
			return d, nil
		}
	}
	folder, err := GameFolder(in.Flavor)
	if err != nil {
		return "", err
	}
	roots := slices.Clone(in.Probe.Roots)
	if in.Probe.Registry != nil {
		for _, p := range in.Probe.Registry() {
			roots = append(roots, installRoot(p))
		}
	}
	for _, root := range roots {
		if st, err := os.Stat(filepath.Join(root, folder)); err == nil && st.IsDir() {
			return filepath.Join(root, folder, "Interface", "AddOns"), nil
		}
	}
	return "", errs.New(errs.Usage,
		fmt.Sprintf("cannot find the %s AddOns folder; set it with --dir, WAM_DIR or dir under [game] in the manifest", in.Flavor),
		"wam sync --dir <path to Interface/AddOns>")
}

// installRoot strips a trailing game folder such as _retail_: Battle.net
// records InstallPath as <install>\_retail_\.
func installRoot(p string) string {
	p = strings.TrimRight(p, `/\`)
	base := p[strings.LastIndexAny(p, `/\`)+1:]
	if len(base) > 2 && strings.HasPrefix(base, "_") && strings.HasSuffix(base, "_") {
		return p[:len(p)-len(base)-1]
	}
	return p
}
