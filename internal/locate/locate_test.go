package locate_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maddawik/wam/internal/errs"
	"github.com/maddawik/wam/internal/locate"
)

func TestConfigDir(t *testing.T) {
	tests := []struct {
		name string
		goos string
		home string
		vars map[string]string
		want string
	}{
		{"xdg wins on mac", "darwin", "/Users/a", map[string]string{"XDG_CONFIG_HOME": "/x"}, filepath.Join("/x", "wam")},
		{"xdg wins on windows", "windows", `C:\Users\a`, map[string]string{"XDG_CONFIG_HOME": "/x", "APPDATA": "/ad"}, filepath.Join("/x", "wam")},
		{"mac default", "darwin", "/Users/a", nil, filepath.Join("/Users/a", ".config", "wam")},
		{"linux default", "linux", "/home/a", nil, filepath.Join("/home/a", ".config", "wam")},
		{"windows appdata", "windows", `C:\Users\a`, map[string]string{"APPDATA": "/ad"}, filepath.Join("/ad", "wam")},
		{"empty xdg ignored", "linux", "/home/a", map[string]string{"XDG_CONFIG_HOME": ""}, filepath.Join("/home/a", ".config", "wam")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := locate.ConfigDir(tt.goos, tt.home, tt.vars); got != tt.want {
				t.Errorf("ConfigDir = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConfigDirWindowsWithoutAppdataFallsBackToHome(t *testing.T) {
	got := locate.ConfigDir("windows", "/h", nil)
	if want := filepath.Join("/h", "AppData", "Roaming", "wam"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestGameFolder(t *testing.T) {
	want := map[string]string{
		"mainline": "_retail_", "classic": "_classic_era_", "forever": "_forever_",
		"bcc": "_classic_", "wrath": "_classic_", "cata": "_classic_", "mists": "_classic_", "titan": "_classic_",
	}
	for flavor, folder := range want {
		got, err := locate.GameFolder(flavor)
		if err != nil || got != folder {
			t.Errorf("GameFolder(%q) = %q, %v; want %q", flavor, got, err, folder)
		}
	}
}

func TestGameFolderUnknownFlavorIsUsageErrorListingValidOnes(t *testing.T) {
	_, err := locate.GameFolder("retail")
	e, ok := errs.As(err)
	if !ok || e.Kind != errs.Usage {
		t.Fatalf("err = %v, want usage error", err)
	}
	for _, f := range []string{`"retail"`, "mainline", "classic", "bcc", "wrath", "cata", "mists", "titan", "forever"} {
		if !strings.Contains(e.Message, f) {
			t.Errorf("message %q lacks %s", e.Message, f)
		}
	}
}

// install makes <root>/<folder> and returns the AddOns path under it.
func install(t *testing.T, root, folder string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, folder), 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, folder, "Interface", "AddOns")
}

func TestAddOnsDirPrecedence(t *testing.T) {
	root := t.TempDir()
	detected := install(t, root, "_retail_")
	probe := locate.Probe{Roots: []string{root}}
	in := func(flag, env, manifest string) locate.Inputs {
		return locate.Inputs{Flag: flag, Env: env, Manifest: manifest, Flavor: "mainline", Probe: probe}
	}
	tests := []struct {
		name string
		in   locate.Inputs
		want string
	}{
		{"flag beats all", in("f", "e", "m"), "f"},
		{"env beats manifest", in("", "e", "m"), "e"},
		{"manifest beats detect", in("", "", "m"), "m"},
		{"detect last", in("", "", ""), detected},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := locate.AddOnsDir(tt.in)
			if err != nil || got != tt.want {
				t.Errorf("got %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestDetectUsesFlavorFolder(t *testing.T) {
	root := t.TempDir()
	want := install(t, root, "_classic_era_")
	got, err := locate.AddOnsDir(locate.Inputs{Flavor: "classic", Probe: locate.Probe{Roots: []string{root}}})
	if err != nil || got != want {
		t.Errorf("got %q, %v; want %q", got, err, want)
	}
}

func TestDetectSkipsRootsWithoutTheFlavorFolder(t *testing.T) {
	empty, real := t.TempDir(), t.TempDir()
	want := install(t, real, "_retail_")
	got, err := locate.AddOnsDir(locate.Inputs{Flavor: "mainline", Probe: locate.Probe{Roots: []string{empty, "/does/not/exist", real}}})
	if err != nil || got != want {
		t.Errorf("got %q, %v; want %q", got, err, want)
	}
}

func TestDetectFallsBackToRegistryInOrder(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	install(t, first, "_retail_")
	want := install(t, second, "_retail_")
	probe := locate.Probe{Roots: []string{t.TempDir()}, Registry: func() []string { return []string{"/nope", second, first} }}
	got, err := locate.AddOnsDir(locate.Inputs{Flavor: "mainline", Probe: probe})
	if err != nil || got != want {
		t.Errorf("got %q, %v; want %q", got, err, want)
	}
}

func TestDetectPrefersRootsOverRegistry(t *testing.T) {
	root, reg := t.TempDir(), t.TempDir()
	want := install(t, root, "_retail_")
	install(t, reg, "_retail_")
	probe := locate.Probe{Roots: []string{root}, Registry: func() []string { return []string{reg} }}
	got, _ := locate.AddOnsDir(locate.Inputs{Flavor: "mainline", Probe: probe})
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestDetectAcceptsRegistryPathPointingAtAFlavorFolder(t *testing.T) {
	// Battle.net writes InstallPath as <install>\_retail_\.
	reg := t.TempDir()
	want := install(t, reg, "_retail_")
	probe := locate.Probe{Registry: func() []string { return []string{filepath.Join(reg, "_retail_") + string(filepath.Separator)} }}
	got, err := locate.AddOnsDir(locate.Inputs{Flavor: "mainline", Probe: probe})
	if err != nil || got != want {
		t.Errorf("got %q, %v; want %q", got, err, want)
	}
}

func TestUndetectableDirIsUsageErrorSayingHowToSetIt(t *testing.T) {
	_, err := locate.AddOnsDir(locate.Inputs{Flavor: "mainline"})
	e, ok := errs.As(err)
	if !ok || e.Kind != errs.Usage {
		t.Fatalf("err = %v, want usage error", err)
	}
	for _, s := range []string{"--dir", "WAM_DIR", "dir"} {
		if !strings.Contains(e.Message, s) {
			t.Errorf("message %q lacks %q", e.Message, s)
		}
	}
}

func TestExplicitDirSkipsFlavorCheck(t *testing.T) {
	got, err := locate.AddOnsDir(locate.Inputs{Flag: "/d", Flavor: "nonsense"})
	if err != nil || got != "/d" {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestDefaultProbeRoots(t *testing.T) {
	mac := locate.DefaultProbe("darwin", map[string]string{})
	if len(mac.Roots) != 1 || mac.Roots[0] != "/Applications/World of Warcraft" {
		t.Errorf("mac roots = %v", mac.Roots)
	}
	if lin := locate.DefaultProbe("linux", nil); len(lin.Roots) != 0 || lin.Registry != nil {
		t.Errorf("linux probe = %+v, want empty", lin)
	}
	win := locate.DefaultProbe("windows", map[string]string{"ProgramFiles": `C:\Program Files`, "ProgramFiles(x86)": `C:\Program Files (x86)`})
	want := []string{filepath.Join(`C:\Program Files (x86)`, "World of Warcraft"), filepath.Join(`C:\Program Files`, "World of Warcraft")}
	if strings.Join(win.Roots, "|") != strings.Join(want, "|") {
		t.Errorf("windows roots = %v, want %v", win.Roots, want)
	}
}
